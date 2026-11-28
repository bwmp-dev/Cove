package control

import (
	"archive/tar"
	"bytes"
	"context"
	"cove/internal/worker"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func startMoveNode(t *testing.T, f *accountFixture, image string) sshNodeFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
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
	name := "cove-move-node-" + resourceID()
	command("", "run", "-d", "--privileged", "--memory", "1536m", "--cpus", "1", "--name", name, "--label", "cove.test=true", "-p", "127.0.0.1::22", image)
	t.Cleanup(func() {
		if t.Failed() {
			out, err := exec.Command("docker", "logs", "--tail", "20", name).CombinedOutput()
			t.Logf("private worker startup diagnostics (%v):\n%s", err, out)
			logPath := filepath.Join(t.TempDir(), "dockerd.log")
			out, err = exec.Command("docker", "cp", name+":/var/log/dockerd.log", logPath).CombinedOutput()
			if err != nil {
				t.Logf("private Docker log capture (%v): %s", err, out)
			} else if data, err := os.ReadFile(logPath); err != nil {
				t.Logf("private Docker log read: %v", err)
			} else {
				lines := strings.Split(string(data), "\n")
				t.Logf("private Docker daemon diagnostics:\n%s", strings.Join(lines[max(0, len(lines)-40):], "\n"))
			}
		}
		out, err := exec.Command("docker", "rm", "-f", "-v", name).CombinedOutput()
		if err != nil {
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
	f.sql(`INSERT INTO nodes(id,name,address,port,host_key,public_key,private_key,state) VALUES($1,'Move worker','127.0.0.1',$2,$3,$4,$5,'enrolled')`, node, port, host, pub, sealed)
	var last worker.Probe
	var lastError error
	for attempt := 0; attempt < 90; attempt++ {
		if command("", "inspect", "--format", "{{.State.Status}}", name) == "exited" {
			t.Fatal("private Docker worker exited before becoming ready")
		}
		p, err := probeSSH(ctx, "127.0.0.1", port, host, seed)
		last, lastError = p, err
		if err == nil && p.Ready && p.WorkloadMoves {
			data, _ := json.Marshal(p)
			f.sql(`UPDATE nodes SET state='ready',capabilities=$2 WHERE id=$1`, node, data)
			return sshNodeFixture{node: node, container: name, host: host, port: port, public: pub, seed: seed, command: command}
		}
		time.Sleep(time.Second)
	}
	t.Logf("last probe docker=%q compose=%q ready=%v moves=%v error=%v", last.Docker, last.Compose, last.Ready, last.WorkloadMoves, lastError)
	t.Fatal("private Docker worker did not become ready", command("", "logs", name), command("", "exec", name, "cat", "/var/log/dockerd.log"))
	return sshNodeFixture{}
}

func moveTestOperation(t *testing.T, f *accountFixture, node string, request worker.Request) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	method := "submit"
	if version := worker.RouteProtocol(request.Routes); version > 1 {
		method = fmt.Sprintf("submit_routes_v%d", version)
	}
	if _, err := f.s.nodeRPC(ctx, node, worker.RPCRequest{Method: method, Request: &request}); err != nil {
		t.Fatal(err)
	}
	for {
		response, err := f.s.nodeRPC(ctx, node, worker.RPCRequest{Method: "status", ID: request.ID})
		if err != nil {
			t.Fatal(err)
		}
		if response.Operation == nil {
			t.Fatal("missing worker operation")
		}
		switch response.Operation.State {
		case "succeeded":
			return
		case "failed", "interrupted", "cancelled":
			logs, _ := f.s.nodeRPC(ctx, node, worker.RPCRequest{Method: "logs", ID: request.ID})
			t.Fatal(response.Operation.Error, logs.Logs)
		}
		select {
		case <-ctx.Done():
			t.Fatal("move fixture operation timed out")
		case <-time.After(time.Second):
		}
	}
}

func TestWorkloadMoveTwoDockerDaemons(t *testing.T) {
	image := os.Getenv("COVE_TEST_NODE_MOVE_IMAGE")
	if image == "" {
		t.Skip("scripts/test-workload-move.py starts private Docker daemons")
	}
	f := newAccountFixture(t)
	source := startMoveNode(t, f, image)
	target := startMoveNode(t, f, image)
	f.sql(`UPDATE nodes SET name=CASE WHEN id=$1 THEN 'Source worker' ELSE 'Destination worker' END WHERE id IN ($1,$2)`, source.node, target.node)
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('default',$1),('default',$2)`, source.node, target.node)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('move-project','Move project','default')`)
	compose := "services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'nc -u -lk -p 9001 -e /bin/cat & exec httpd -f -p 8080 -h /data']\n    restart: always\n    ports: ['8080:8080']\n    volumes: ['data:/data']\n  idle:\n    image: busybox:1.37\n    command: [sleep, '3600']\n    restart: unless-stopped\nvolumes:\n  data: {}\n"
	backup := &worker.BackupConfig{Repository: "local", Password: "synthetic-move-backup-password", KeepLast: 3}
	created := f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Move app", NodeID: source.node, Compose: compose, Kind: "service", ProjectID: "move-project", Environment: map[string]string{"PRIVATE": "synthetic-private-environment"}, Backup: backup}, 201)
	resource := jsonString(t, created, "id")
	certificate, key := certificateFixture(t, "move-private.example.test")
	certificateID := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+resource+"/certificates", routeCertificateInput{Name: "Move certificate", CertificatePEM: certificate, PrivateKeyPEM: key}, 201), "id")
	gateways := map[string]string{}
	for _, node := range []sshNodeFixture{source, target} {
		gateway := node.command("", "exec", node.container, "docker", "network", "inspect", "--format", "{{range .IPAM.Config}}{{.Gateway}}{{end}}", "bridge")
		gateways[node.node] = gateway
		f.call("root", "default", "POST", "/api/nodes/"+node.node+"/ingress-listeners/initialize", nil, 200)
		settings := worker.IngressExposureSettings{Listeners: []worker.IngressListener{
			{ID: "office", Protocol: "https", Visibility: "lan", Bindings: []worker.IngressBinding{{Address: gateway, Port: 9443}}},
			{ID: "socket", Protocol: "tcp", Visibility: "public", Bindings: []worker.IngressBinding{{Address: "127.0.0.1", Port: 15432}}},
			{ID: "datagrams", Protocol: "udp", Visibility: "public", Bindings: []worker.IngressBinding{{Address: "127.0.0.1", Port: 15433}}},
		}}
		var plan worker.IngressExposurePlan
		if err := json.Unmarshal(f.call("root", "default", "POST", "/api/nodes/"+node.node+"/ingress-listeners/plan", settings, 200).Body.Bytes(), &plan); err != nil {
			t.Fatal(err)
		}
		f.call("root", "default", "PUT", "/api/nodes/"+node.node+"/ingress-listeners", worker.IngressExposureRequest{Settings: plan.Settings, Revision: plan.Revision, Digest: plan.Digest, AcceptInterruption: true}, 200)
		node.command("", "exec", node.container, "docker", "start", "cove-ingress")
	}
	routes := []worker.Route{
		{Domain: "move-private.example.test", Service: "app", Port: 8080, Policy: &worker.RoutePolicy{Visibility: "lan", Listener: "office", CertificateSource: "uploaded", CertificateID: certificateID}},
		{Protocol: "tcp", Service: "app", Port: 8080, Policy: &worker.RoutePolicy{Visibility: "public", Listener: "socket"}},
		{Protocol: "udp", Service: "app", Port: 9001, Policy: &worker.RoutePolicy{Visibility: "public", Listener: "datagrams"}},
	}
	f.call("root", "default", "PUT", "/api/workloads/"+resource+"/routes", map[string]any{"routes": routes}, 200)
	deploy := worker.Request{ID: resourceID(), Workload: resource, Action: "deploy", Compose: compose, Kind: "service", Environment: map[string]string{"PRIVATE": "synthetic-private-environment"}, Routes: routes, Certificates: []worker.RouteCertificate{{ID: certificateID, CertificatePEM: certificate, PrivateKeyPEM: key}}}
	moveTestOperation(t, f, source.node, deploy)
	deployData, _ := json.Marshal(deploy)
	deployPayload, _ := seal(f.s.Key, deployData)
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES($1,$2,'deploy','succeeded',$3)`, deploy.ID, resource, deployPayload)
	app := source.command("", "exec", source.container, "docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+resource, "--filter", "label=com.docker.compose.service=app")
	idle := source.command("", "exec", source.container, "docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+resource, "--filter", "label=com.docker.compose.service=idle")
	source.command("", "exec", source.container, "docker", "exec", app, "sh", "-ec", `printf 'before-move\n' > /data/marker; chmod 640 /data/marker; chown 10001:10002 /data/marker; ln -s marker /data/link`)
	source.command("", "exec", source.container, "docker", "stop", idle)
	backupRequest := worker.Request{ID: resourceID(), Workload: resource, Action: "backup", Backup: backup}
	moveTestOperation(t, f, source.node, backupRequest)
	source.command("", "exec", source.container, "docker", "exec", app, "mkfifo", "/data/pipe")
	// This non-Cove container belongs only to this disposable daemon and must survive rollback.
	target.command("", "exec", target.container, "docker", "run", "-d", "--name", "foreign-port-holder", "-p", "8080:8080", "busybox:1.37", "sleep", "3600")
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	restartedSource := false
	run := func(expectedFailure string) string {
		t.Helper()
		previewResponse := f.call("root", "default", "POST", "/api/workloads/"+resource+"/node-move/preview", map[string]string{"destination_node_id": target.node}, 200)
		var preview workloadMovePreview
		if err := json.Unmarshal(previewResponse.Body.Bytes(), &preview); err != nil {
			t.Fatal(err)
		}
		if preview.Source.Running != 1 || preview.Source.Containers != 2 || len(preview.Source.Volumes) != 2 {
			t.Fatal("move omitted stopped containers or local backups")
		}
		queued := f.call("root", "default", "POST", "/api/workloads/"+resource+"/node-move", map[string]any{"destination_node_id": target.node, "revision": preview.Revision, "confirm": true}, 202)
		id := jsonString(t, queued, "id")
		var payload []byte
		if err := f.s.DB.QueryRow(ctx, `SELECT payload FROM operations WHERE id=$1`, id).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		previousPhase := ""
		for {
			if err := f.s.processOperation(ctx, id, source.node, payload); err != nil {
				t.Fatal(err)
			}
			var state, phase, message string
			if err := f.s.DB.QueryRow(ctx, `SELECT o.state,m.phase,m.last_error FROM operations o JOIN workload_moves m ON m.operation_id=o.id WHERE o.id=$1`, id).Scan(&state, &phase, &message); err != nil {
				t.Fatal(err)
			}
			t.Log("move phase", phase)
			if phase == "exporting" && !restartedSource {
				restartedSource = true
				source.command("", "restart", source.container)
				_, portText, err := net.SplitHostPort(source.command("", "port", source.container, "22/tcp"))
				if err != nil {
					t.Fatal(err)
				}
				port, err := strconv.Atoi(portText)
				if err != nil {
					t.Fatal(err)
				}
				f.sql(`UPDATE nodes SET port=$2 WHERE id=$1`, source.node, port)
				ready := false
				var last worker.Probe
				var lastError error
				for attempt := 0; attempt < 90; attempt++ {
					p, err := probeSSH(ctx, "127.0.0.1", port, source.host, source.seed)
					last, lastError = p, err
					if err == nil && p.Ready {
						ready = true
						break
					}
					time.Sleep(time.Second)
				}
				if !ready {
					t.Logf("last restarted probe ready=%v docker=%q compose=%q error=%v", last.Ready, last.Docker, last.Compose, lastError)
					t.Fatal("source did not recover after its private Docker daemon restarted", source.command("", "logs", source.container), source.command("", "exec", source.container, "cat", "/var/log/dockerd.log"))
				}
				if got := source.command("", "exec", source.container, "docker", "inspect", "--format", "{{.State.Running}}|{{.HostConfig.RestartPolicy.Name}}", app); got != "false|no" {
					t.Fatal("source fence did not survive Docker daemon restart", got)
				}
				state, err := f.s.moveRPC(ctx, source.node, id, resource, "status", preview.Source.Daemon)
				if err != nil || state.Phase != "frozen" {
					t.Fatal("source worker forgot its durable move receipt", err)
				}
			}
			if phase == "rolling_back" && expectedFailure != "" && previousPhase != expectedFailure {
				t.Fatalf("move failed at %s before expected %s", previousPhase, expectedFailure)
			}
			previousPhase = phase
			if state == "succeeded" || state == "failed" || state == "cancelled" {
				return state
			}
			select {
			case <-ctx.Done():
				t.Fatal("move timed out", phase, message)
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if state := run("starting"); state != "failed" {
		t.Fatal("port conflict did not trigger rollback", state)
	}
	if got := source.command("", "exec", source.container, "docker", "inspect", "--format", "{{.State.Running}}|{{.HostConfig.RestartPolicy.Name}}", app); got != "true|always" {
		t.Fatal("source restart policy/running state not restored", got)
	}
	if got := target.command("", "exec", target.container, "docker", "inspect", "--format", "{{.State.Running}}", "foreign-port-holder"); got != "true" {
		t.Fatal("move removed an unrelated destination container")
	}
	target.command("", "exec", target.container, "docker", "rm", "-f", "foreign-port-holder")
	if os.Getenv("COVE_MOVE_BROWSER_SCRIPT") != "" {
		runWorkloadMoveBrowser(t, f, resource, target.node)
	} else if state := run(""); state != "succeeded" {
		t.Fatal("move did not commit", state)
	}
	var placement string
	if err := f.s.DB.QueryRow(ctx, `SELECT node_id FROM workloads WHERE id=$1`, resource).Scan(&placement); err != nil || placement != target.node {
		t.Fatal("move did not update placement", err)
	}
	targetApp := target.command("", "exec", target.container, "docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+resource, "--filter", "label=com.docker.compose.service=app")
	if got := target.command("", "exec", target.container, "docker", "exec", targetApp, "sh", "-ec", `cat /data/marker; stat -c '%u:%g:%a' /data/marker; readlink /data/link; test -p /data/pipe`); got != "before-move\n10001:10002:640\nmarker" {
		t.Fatal("move changed volume bytes, ownership, modes, symlink or fifo", got)
	}
	if got := target.command("", "exec", target.container, "docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+resource, "--filter", "label=com.docker.compose.service=idle"); got != "" {
		t.Fatal("move started a manually stopped service")
	}
	response, err := f.s.nodeRPC(ctx, target.node, worker.RPCRequest{Method: "backups", ID: resource})
	if err != nil || len(response.Backups) != 1 || response.Backups[0].ID != backupRequest.ID {
		t.Fatal("move lost retained backup records", err)
	}
	if got := target.command("", "exec", target.container, "docker", "volume", "inspect", "--format", `{{index .Labels "cove.backup"}}`, "cove-backups-"+resource); got != resource {
		t.Fatal("encrypted local repository did not move")
	}
	download := worker.Request{ID: resourceID(), Workload: resource, Action: "restore", BackupID: backupRequest.ID, Backup: backup}
	if err = f.s.nodeStream(ctx, target.node, worker.RPCRequest{Method: "stream", Stream: &worker.StreamRequest{Action: "backup_download", Request: &download, Limit: 32 << 20}}, nil, func(meta worker.StreamResponse, body io.Reader) error {
		if meta.Size < 1 || meta.Size > 32<<20 {
			return fmt.Errorf("moved backup size invalid")
		}
		data, err := io.ReadAll(io.LimitReader(body, meta.Size+1))
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		if int64(len(data)) != meta.Size || hex.EncodeToString(digest[:]) != meta.SHA256 {
			return fmt.Errorf("moved repository dump failed checksum")
		}
		archive := tar.NewReader(bytes.NewReader(data))
		for {
			header, err := archive.Next()
			if err != nil {
				return fmt.Errorf("moved repository has no saved marker: %w", err)
			}
			if strings.HasSuffix(header.Name, "/marker") {
				marker, err := io.ReadAll(io.LimitReader(archive, 1024))
				if err != nil {
					return err
				}
				if string(marker) != "before-move\n" {
					return fmt.Errorf("moved backup restored different data")
				}
				return nil
			}
		}
	}); err != nil {
		t.Fatal("moved encrypted backup repository could not be read", err)
	}
	if _, err = f.s.nodeRPC(ctx, target.node, worker.RPCRequest{Method: "status", ID: deploy.ID}); err != nil {
		t.Fatal("move lost retained operation journals", err)
	}
	if got := source.command("", "exec", source.container, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+resource); got != "" {
		t.Fatal("committed source containers remain")
	}
	saved := readDatabaseConfig(t, f, resource)
	if !worker.RoutesEqual(saved.Routes, routes) {
		t.Fatal("move lost private/raw routes or certificate reference")
	}
	probe, _ := json.Marshal([]map[string]any{{"hostname": "move-private.example.test", "certificate_pem": certificate, "verified_at": time.Now()}})
	target.command(string(probe), "exec", "-i", target.container, "sh", "-ec", "cat > /tmp/moved-certificate-probe.json; /usr/local/bin/cove-worker certificate-probe /tmp/moved-certificate-probe.json; rm /tmp/moved-certificate-probe.json")
	for _, address := range []string{"http://127.0.0.1:15432/marker", "https://" + gateways[target.node] + ":9443/marker"} {
		body := target.command("", "exec", target.container, "docker", "run", "--rm", "--network", "host", "busybox:1.37", "wget", "-q", "-O", "-", "--no-check-certificate", "--header", "Host: move-private.example.test", address)
		if body != "before-move" {
			t.Fatal("moved listener did not reach retained application data")
		}
	}
	udp := target.command("", "exec", target.container, "docker", "run", "--rm", "--network", "host", "busybox:1.37", "sh", "-c", `{ printf handshake; sleep 1; printf moved-datagram; } | timeout 2 nc -u -w 1 127.0.0.1 15433; status=$?; test "$status" -eq 0 -o "$status" -eq 143`)
	if !strings.Contains(udp, "moved-datagram") {
		t.Fatal("moved UDP listener did not return the application's datagram")
	}
	// The strict certificate probe above pins the moved leaf; the public listener must still deny the private host.
	denied := target.command("", "exec", target.container, "docker", "run", "--rm", "--network", "host", "busybox:1.37", "sh", "-c", `wget --no-check-certificate -S -O /dev/null --header 'Host: move-private.example.test' https://127.0.0.1/ 2>&1 || true`)
	if !strings.Contains(denied, "418") {
		t.Fatal("move exposed a private hostname on public HTTPS")
	}
	t.Log(fmt.Sprintf("moved resource %s across independent Docker daemons with rollback, encrypted backups, private/raw listeners and verified certificate retention", resource))
}
