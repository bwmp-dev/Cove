package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"cove/internal/worker"
)

func TestBastionPermissionsSealingAndNodeBindings(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("bastion-owner", "bastion-owner@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','bastion-owner','owner','[]')`)
	pub, _, _ := keyPair()
	in := bastionInput{Name: "Jump", Address: "127.0.0.1", Port: 22, User: "covejump", HostKey: pub}
	f.call(owner, "default", "POST", "/api/bastions", in, 403)
	f.call(owner, "default", "GET", "/api/bastions", nil, 403)
	created := f.call("root", "default", "POST", "/api/bastions", in, 201)
	id := jsonString(t, created, "id")
	if strings.Contains(created.Body.String(), "private_key") || strings.Contains(created.Body.String(), "seed") {
		t.Fatal("bastion leaked private identity")
	}
	var key []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT private_key FROM ssh_bastions WHERE id=$1`, id).Scan(&key); err != nil {
		t.Fatal(err)
	}
	plain, err := unseal(f.s.Key, key)
	if err != nil || len(plain) != 32 || string(plain) == string(key) {
		t.Fatal("bastion key not sealed", err)
	}
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('bastion-node','Node','worker.internal',22,'',$1,'pending')`, []byte{})
	f.call(owner, "default", "GET", "/api/nodes/bastion-node/identity", nil, 403)
	f.call(owner, "default", "POST", "/api/nodes/bastion-node/identity", map[string]any{"action": "rotate", "confirm": true}, 403)
	f.call(owner, "default", "PUT", "/api/nodes/bastion-node/bastion", map[string]string{"bastion_id": id}, 403)
	f.call("root", "default", "PUT", "/api/nodes/bastion-node/bastion", map[string]string{"bastion_id": "missing"}, 409)
	f.call("root", "default", "PUT", "/api/nodes/bastion-node/bastion", map[string]string{"bastion_id": id}, 200)
	listed := f.call("root", "default", "GET", "/api/bastions", nil, 200)
	if !strings.Contains(listed.Body.String(), "worker.internal:22") {
		t.Fatal("forwarding destination omitted")
	}
	f.call("root", "default", "DELETE", "/api/bastions/"+id, nil, 409)
	f.call("root", "default", "PUT", "/api/nodes/bastion-node/bastion", map[string]string{"bastion_id": ""}, 200)
	f.call("root", "default", "DELETE", "/api/bastions/"+id, nil, 200)
	for _, user := range []string{"root;id", "bad user", "-jump"} {
		in.User = user
		f.call("root", "default", "POST", "/api/bastions", in, 400)
	}
}

type sshNodeFixture struct {
	node      string
	public    string
	container string
	host      string
	port      int
	seed      []byte
	command   func(string, ...string) string
}

func startSSHNodeFixture(t *testing.T, f *accountFixture) sshNodeFixture {
	t.Helper()
	image := os.Getenv("COVE_TEST_NODE_SSH_IMAGE")
	if image == "" {
		t.Skip("scripts/test-node-ssh.py builds a disposable OpenSSH worker fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	command := func(input string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	name := "cove-node-ssh-" + resourceID()
	command("", "run", "-d", "--name", name, "--label", "cove.test=true", "-p", "127.0.0.1::22", image)
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "rm", "-f", "-v", name).CombinedOutput(); err != nil {
			t.Error(err, string(out))
		}
	})
	pub, seed, err := keyPair()
	if err != nil {
		t.Fatal(err)
	}
	command(`restrict,command="/usr/local/bin/cove-worker rpc" `+pub+"\n", "exec", "-i", name, "sh", "-ec", `cat > /var/lib/cove/.ssh/authorized_keys; chown cove:cove /var/lib/cove/.ssh/authorized_keys; chmod 600 /var/lib/cove/.ssh/authorized_keys`)
	host := command("", "exec", name, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
	_, portText, err := net.SplitHostPort(command("", "port", name, "22/tcp"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := seal(f.s.Key, seed)
	if err != nil {
		t.Fatal(err)
	}
	node := resourceID()
	f.sql(`INSERT INTO nodes(id,name,address,port,host_key,public_key,private_key,state) VALUES($1,'Worker','127.0.0.1',$2,$3,$4,$5,'enrolled')`, node, port, host, pub, sealed)
	ready := false
	for attempt := 0; attempt < 30; attempt++ {
		p, err := probeSSH(ctx, "127.0.0.1", port, host, seed)
		if err == nil && p.Protocol == 1 {
			capabilities, encodeErr := json.Marshal(p)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			f.sql(`UPDATE nodes SET capabilities=$2 WHERE id=$1`, node, capabilities)
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		t.Fatal("disposable SSH worker did not start", command("", "logs", name))
	}
	return sshNodeFixture{node: node, public: pub, container: name, host: host, port: port, seed: seed, command: command}
}

func TestBastionOpenSSHDockerAndPinnedIdentityAlerts(t *testing.T) {
	f := newAccountFixture(t)
	fixture := startSSHNodeFixture(t, f)
	created := f.call("root", "default", "POST", "/api/bastions", bastionInput{Name: "Jump", Address: "127.0.0.1", Port: fixture.port, User: "covejump", HostKey: fixture.host}, 201)
	var bastion bastionView
	if err := json.Unmarshal(created.Body.Bytes(), &bastion); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("restrict,port-forwarding,permitopen=\"127.0.0.1:22\",command=\"/bin/false\" %s\n", bastion.PublicKey)
	fixture.command(line, "exec", "-i", fixture.container, "sh", "-ec", `cat > /home/covejump/.ssh/authorized_keys; chown covejump:covejump /home/covejump/.ssh/authorized_keys; chmod 600 /home/covejump/.ssh/authorized_keys`)
	f.sql(`UPDATE nodes SET address='127.0.0.1',port=22 WHERE id=$1`, fixture.node)
	f.call("root", "default", "PUT", "/api/nodes/"+fixture.node+"/bastion", map[string]string{"bastion_id": bastion.ID}, 200)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := f.s.nodeRPC(ctx, fixture.node, worker.RPCRequest{Method: "probe"})
	if err != nil || result.Probe == nil || result.Probe.Protocol != 1 {
		t.Fatal("pinned forwarded worker RPC", err)
	}
	client, err := f.s.openNodeSSH(ctx, fixture.node, nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Run("id"); err == nil {
		t.Fatal("worker RPC identity acquired a shell")
	}
	session.Close()
	client.Close()
	wrong, _, _ := keyPair()
	f.sql(`UPDATE nodes SET host_key=$2 WHERE id=$1`, fixture.node, wrong)
	_, err = f.s.nodeRPC(ctx, fixture.node, worker.RPCRequest{Method: "probe"})
	var changed *HostKeyChanged
	if !errors.As(err, &changed) || changed.Expected == changed.Observed {
		t.Fatal("forwarded host mismatch was not typed/refused", err)
	}
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('default',$1)`, fixture.node)
	if err = f.s.noteHostIdentity(ctx, fixture.node, err); err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE health_alerts SET first_seen=now()-interval '3 minutes' WHERE kind='host_key_changed'`)
	if err = f.s.noteHostIdentity(ctx, fixture.node, changed); err != nil {
		t.Fatal(err)
	}
	var active bool
	if err = f.s.DB.QueryRow(ctx, `SELECT active FROM health_alerts WHERE kind='host_key_changed' AND subject=$1`, fixture.node).Scan(&active); err != nil || !active {
		t.Fatal("host mismatch did not alert", err)
	}
	f.sql(`UPDATE nodes SET host_key=$2 WHERE id=$1`, fixture.node, fixture.host)
	result, err = f.s.nodeRPC(ctx, fixture.node, worker.RPCRequest{Method: "probe"})
	if err != nil || result.Probe == nil {
		t.Fatal("restored pinned key rejected", err)
	}
	if err = f.s.noteHostIdentity(ctx, fixture.node, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM health_alerts WHERE kind='host_key_changed' AND subject=$1`, fixture.node).Scan(&count); err != nil || count != 0 {
		t.Fatal("host mismatch did not recover", err)
	}
	f.sql(`UPDATE ssh_bastions SET host_key=$2 WHERE id=$1`, bastion.ID, wrong)
	_, err = f.s.nodeRPC(ctx, fixture.node, worker.RPCRequest{Method: "probe"})
	if !errors.As(err, &changed) || changed.Hop != "bastion" {
		t.Fatal("changed bastion pin accepted or misclassified", err)
	}
	f.sql(`UPDATE ssh_bastions SET host_key=$2 WHERE id=$1`, bastion.ID, fixture.host)
	var encrypted []byte
	if err = f.s.DB.QueryRow(ctx, `SELECT private_key FROM ssh_bastions WHERE id=$1`, bastion.ID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	jumpSeed, err := unseal(f.s.Key, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	jump, err := openSSHUser(ctx, "127.0.0.1", fixture.port, "covejump", fixture.host, jumpSeed, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer jump.Close()
	if listener, err := jump.Listen("tcp", "127.0.0.1:0"); err == nil {
		listener.Close()
		t.Fatal("bastion allowed remote forwarding")
	}
	if conn, err := jump.DialContext(ctx, "tcp", "127.0.0.1:1"); err == nil {
		conn.Close()
		t.Fatal("bastion allowed an unlisted destination")
	}
}

func TestForwardedSSHHandshakeCancellation(t *testing.T) {
	pub, seed, err := keyPair()
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer server.Close()
	go io.Copy(io.Discard, server)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = openSSHUser(ctx, "not-dialed.invalid", 22, "cove", pub, seed, func(context.Context, string, string) (net.Conn, error) { return client, nil })
	if err == nil || time.Since(started) > time.Second {
		t.Fatal("forwarded handshake ignored cancellation", err)
	}
}

func TestSSHIdentityRotationResumesAndCancelsOnRealWorker(t *testing.T) {
	f := newAccountFixture(t)
	fixture := startSSHNodeFixture(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	path := "/api/nodes/" + fixture.node + "/identity"
	f.call("root", "default", "POST", path, map[string]any{"action": "rotate", "confirm": false}, 400)
	f.call("root", "default", "POST", path, map[string]any{"action": "rotate", "confirm": true}, 202)
	f.call("root", "default", "POST", "/api/nodes/"+fixture.node+"/enrollment", nil, 409)
	f.s.workerRPC = func(ctx context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		client, err := f.s.openNodeSSH(ctx, node, nil)
		if err != nil {
			return worker.RPCResponse{}, err
		}
		defer client.Close()
		if _, err = callSSHClient(client, r); err != nil {
			return worker.RPCResponse{}, err
		}
		return worker.RPCResponse{}, errors.New("injected lost acknowledgement")
	}
	if err := f.s.processIdentity(ctx, fixture.node); err != nil {
		t.Fatal(err)
	}
	f.s.workerRPC = nil
	status := f.call("root", "default", "GET", path, nil, 200)
	if !strings.Contains(status.Body.String(), "queued") || !strings.Contains(status.Body.String(), "Could not confirm") {
		t.Fatal("lost receipt was not retained", status.Body.String())
	}
	f.call("root", "default", "POST", path, map[string]any{"action": "retry", "confirm": true}, 202)
	if err := f.s.processIdentity(ctx, fixture.node); err != nil {
		t.Fatal(err)
	}
	staged := f.call("root", "default", "GET", path, nil, 200)
	if !strings.Contains(staged.Body.String(), `"state":"staged"`) {
		t.Fatal("key was not staged before restart", staged.Body.String())
	}
	fixture.command("", "exec", "--user", "cove", fixture.container, "sh", "-ec", `umask 077; printf interrupted > /var/lib/cove/.ssh/authorized_keys.cove-stage`)
	fixture.command("", "restart", fixture.container)
	_, portText, err := net.SplitHostPort(fixture.command("", "port", fixture.container, "22/tcp"))
	if err != nil {
		t.Fatal(err)
	}
	newPort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("disposable SSH mapping after restart: %d -> %d", fixture.port, newPort)
	fixture.port = newPort
	f.sql(`UPDATE nodes SET port=$2 WHERE id=$1`, fixture.node, newPort)
	for attempt := 0; attempt < 60; attempt++ {
		_, probeErr := probeSSH(ctx, "127.0.0.1", fixture.port, fixture.host, fixture.seed)
		if probeErr == nil {
			break
		}
		if attempt == 59 {
			t.Fatal("worker failed to restart during key rotation", probeErr, fixture.command("", "logs", fixture.container))
		}
		time.Sleep(200 * time.Millisecond)
	}
	resumed := &Server{DB: f.s.DB, Key: f.s.Key}
	if err := resumed.processIdentity(ctx, fixture.node); err != nil {
		t.Fatal(err)
	}
	fixture.command(`restrict,command="/usr/local/bin/cove-worker rpc" `+fixture.public+"\n", "exec", "-i", fixture.container, "sh", "-ec", `cat > /var/lib/cove/.ssh/authorized_keys`)
	if err := resumed.processIdentity(ctx, fixture.node); err != nil {
		t.Fatal(err)
	}
	status = f.call("root", "default", "GET", path, nil, 200)
	if !strings.Contains(status.Body.String(), "succeeded") || strings.Contains(status.Body.String(), "seed") || strings.Contains(status.Body.String(), "private_key") {
		t.Fatal("rotation status or secrecy", status.Body.String())
	}
	if _, err := probeSSH(ctx, "127.0.0.1", fixture.port, fixture.host, fixture.seed); err == nil {
		t.Fatal("old key still authenticates")
	}
	if result, err := f.s.nodeRPC(ctx, fixture.node, worker.RPCRequest{Method: "probe"}); err != nil || result.Probe == nil {
		t.Fatal("saved new identity does not authenticate", err)
	}
	fixture.command("", "exec", fixture.container, "sh", "-ec", `test ! -e /var/lib/cove/.ssh/authorized_keys.cove-stage`)
	var retained bool
	if err := f.s.DB.QueryRow(ctx, `SELECT configuration IS NOT NULL FROM node_identity_rotations WHERE node_id=$1`, fixture.node).Scan(&retained); err != nil || retained {
		t.Fatal("completed rotation retained duplicate key material", err)
	}
	f.call("root", "default", "POST", path, map[string]any{"action": "rotate", "confirm": true}, 202)
	if err := f.s.processIdentity(ctx, fixture.node); err != nil {
		t.Fatal(err)
	}
	var encrypted []byte
	var rotation string
	if err := f.s.DB.QueryRow(ctx, `SELECT id,configuration FROM node_identity_rotations WHERE node_id=$1`, fixture.node).Scan(&rotation, &encrypted); err != nil {
		t.Fatal(err)
	}
	plain, err := unseal(f.s.Key, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	var config identityConfiguration
	if json.Unmarshal(plain, &config) != nil {
		t.Fatal("invalid staged identity")
	}
	f.call("root", "default", "POST", path, map[string]any{"action": "cancel", "confirm": true}, 202)
	if err := f.s.processIdentity(ctx, fixture.node); err != nil {
		t.Fatal(err)
	}
	status = f.call("root", "default", "GET", path, nil, 200)
	if !strings.Contains(status.Body.String(), "cancelled") {
		t.Fatal("rotation not cancelled", status.Body.String())
	}
	if _, err := probeSSH(ctx, "127.0.0.1", fixture.port, fixture.host, config.NewSeed); err == nil {
		t.Fatal("cancelled key still authenticates")
	}
	request := worker.IdentityRequest{ID: rotation, Action: "stage", Old: config.Old, New: config.New}
	if _, err := f.s.nodeRPC(ctx, fixture.node, worker.RPCRequest{Method: "identity", Identity: &request}); err == nil {
		t.Fatal("cancelled stage replayed")
	}
}
