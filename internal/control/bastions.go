package control

import (
	"context"
	"errors"
	"golang.org/x/crypto/ssh"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
)

var sshLogin = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

type bastionInput struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	HostKey string `json:"host_key"`
}

type bastionView struct {
	bastionInput
	ID          string   `json:"id"`
	PublicKey   string   `json:"public_key"`
	Fingerprint string   `json:"fingerprint"`
	Targets     []string `json:"targets"`
	Limited     bool     `json:"limited"`
}

func validSSHAddress(address string, port int) bool {
	return len(address) > 0 && len(address) <= 253 && port > 0 && port <= 65535 && strings.IndexFunc(address, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) < 0 && !strings.ContainsAny(address, "/\\\"'")
}

func (s *Server) bastions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodDelete {
		result, err := s.DB.Exec(r.Context(), `DELETE FROM ssh_bastions WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM nodes WHERE bastion_id=$1)`, r.PathValue("id"))
		if err != nil {
			http.Error(w, "could not remove bastion", 503)
			return
		}
		if result.RowsAffected() != 1 {
			http.Error(w, "detach this bastion's nodes before removing it", 409)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	if r.Method == http.MethodPost {
		var in bastionInput
		if !decode(w, r, &in) {
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		host, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(in.HostKey))
		if in.Name == "" || len(in.Name) > 120 || !validSSHAddress(in.Address, in.Port) || !sshLogin.MatchString(in.User) || err != nil || len(strings.TrimSpace(string(rest))) != 0 {
			http.Error(w, "provide a name, SSH endpoint, restricted forwarding user and pinned host public key", 400)
			return
		}
		if host.Type() != ssh.KeyAlgoRSA && !slices.Contains(ssh.SupportedAlgorithms().HostKeys, host.Type()) {
			http.Error(w, "use a supported secure SSH host public key", 400)
			return
		}
		in.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(host)))
		pub, priv, err := keyPair()
		if err != nil {
			http.Error(w, "key generation unavailable", 503)
			return
		}
		sealed, err := seal(s.Key, priv)
		if err != nil {
			http.Error(w, "key protection unavailable", 503)
			return
		}
		tx, err := s.DB.Begin(r.Context())
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(783529105)`); err != nil {
			http.Error(w, "bastion inventory unavailable", 503)
			return
		}
		var count int
		if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM ssh_bastions`).Scan(&count); err != nil {
			http.Error(w, "bastion inventory unavailable", 503)
			return
		}
		if count >= 32 {
			http.Error(w, "at most 32 bastions are supported", 409)
			return
		}
		id := resourceID()
		if _, err = tx.Exec(r.Context(), `INSERT INTO ssh_bastions(id,name,address,port,login,host_key,public_key,private_key) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, in.Name, in.Address, in.Port, in.User, in.HostKey, pub, sealed); err != nil {
			http.Error(w, "could not create bastion", 503)
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			http.Error(w, "could not save bastion", 503)
			return
		}
		respond(w, 201, bastionView{bastionInput: in, ID: id, PublicKey: pub, Fingerprint: ssh.FingerprintSHA256(host), Targets: []string{}})
		return
	}
	rows, err := s.DB.Query(r.Context(), `SELECT b.id,b.name,b.address,b.port,b.login,b.host_key,b.public_key,ARRAY(SELECT CASE WHEN n.address LIKE '%:%' THEN '['||n.address||']' ELSE n.address END||':'||n.port::text FROM nodes n WHERE n.bastion_id=b.id ORDER BY n.id LIMIT 257) FROM ssh_bastions b ORDER BY b.name,b.id LIMIT 32`)
	if err != nil {
		http.Error(w, "bastions unavailable", 503)
		return
	}
	defer rows.Close()
	out := []bastionView{}
	for rows.Next() {
		var b bastionView
		if err = rows.Scan(&b.ID, &b.Name, &b.Address, &b.Port, &b.User, &b.HostKey, &b.PublicKey, &b.Targets); err != nil {
			http.Error(w, "bastions unavailable", 503)
			return
		}
		host, _, _, _, err := ssh.ParseAuthorizedKey([]byte(b.HostKey))
		if err != nil {
			http.Error(w, "bastion host identity unavailable", 503)
			return
		}
		b.Fingerprint = ssh.FingerprintSHA256(host)
		if len(b.Targets) > 256 {
			b.Targets, b.Limited = b.Targets[:256], true
		}
		out = append(out, b)
	}
	if rows.Err() != nil {
		http.Error(w, "bastions unavailable", 503)
		return
	}
	respond(w, 200, out)
}

func (s *Server) nodeBastion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodPut {
		var in struct {
			BastionID string `json:"bastion_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		if len(in.BastionID) > 64 {
			http.Error(w, "invalid bastion", 400)
			return
		}
		result, err := s.DB.Exec(r.Context(), `UPDATE nodes SET bastion_id=NULLIF($2,''),state=CASE WHEN host_key IS NULL THEN 'pending' ELSE 'enrolled' END WHERE id=$1 AND mode='ssh' AND ($2='' OR EXISTS(SELECT 1 FROM ssh_bastions WHERE id=$2)) AND NOT EXISTS(SELECT 1 FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE w.node_id=nodes.id AND o.state IN ('queued','running'))`, r.PathValue("id"), in.BastionID)
		if err != nil {
			http.Error(w, "could not save node bastion", 503)
			return
		}
		if result.RowsAffected() != 1 {
			http.Error(w, "select an SSH node and saved bastion, and wait for active operations", 409)
			return
		}
	}
	var id, hostError string
	if err := s.DB.QueryRow(r.Context(), `SELECT COALESCE(bastion_id,''),host_key_error FROM nodes WHERE id=$1`, r.PathValue("id")).Scan(&id, &hostError); err != nil {
		http.Error(w, "node unavailable", 503)
		return
	}
	respond(w, 200, map[string]string{"bastion_id": id, "host_key_error": hostError})
}

type forwardedSSHConn struct {
	net.Conn
	parent *ssh.Client
	once   sync.Once
}

func (c *forwardedSSHConn) Close() error {
	var err error
	c.once.Do(func() { err = errors.Join(c.Conn.Close(), c.parent.Close()) })
	return err
}

func (s *Server) openNodeSSH(ctx context.Context, id string, override []byte) (*ssh.Client, error) {
	var address string
	var port int
	var host *string
	var encrypted []byte
	var bastion *string
	if err := s.DB.QueryRow(ctx, `SELECT address,port,host_key,private_key,bastion_id FROM nodes WHERE id=$1 AND mode='ssh'`, id).Scan(&address, &port, &host, &encrypted, &bastion); err != nil {
		return nil, err
	}
	if host == nil {
		return nil, errors.New("node is not enrolled")
	}
	seed, err := unseal(s.Key, encrypted)
	if err != nil {
		return nil, err
	}
	if override != nil {
		seed = override
	}
	if bastion == nil {
		return openSSH(ctx, address, port, *host, seed)
	}
	var jump bastionInput
	var jumpKey []byte
	if err = s.DB.QueryRow(ctx, `SELECT address,port,login,host_key,private_key FROM ssh_bastions WHERE id=$1`, *bastion).Scan(&jump.Address, &jump.Port, &jump.User, &jump.HostKey, &jumpKey); err != nil {
		return nil, err
	}
	jumpSeed, err := unseal(s.Key, jumpKey)
	if err != nil {
		return nil, err
	}
	parent, err := openSSHUser(ctx, jump.Address, jump.Port, jump.User, jump.HostKey, jumpSeed, nil)
	if err != nil {
		var changed *HostKeyChanged
		if errors.As(err, &changed) {
			changed.Hop = "bastion"
		}
		return nil, err
	}
	dial := func(ctx context.Context, network, endpoint string) (net.Conn, error) {
		conn, err := parent.DialContext(ctx, network, endpoint)
		if err != nil {
			return nil, err
		}
		return &forwardedSSHConn{Conn: conn, parent: parent}, nil
	}
	client, err := openSSHUser(ctx, address, port, "cove", *host, seed, dial)
	if err != nil {
		parent.Close()
	}
	return client, err
}

func (s *Server) noteHostIdentity(ctx context.Context, id string, probeErr error) error {
	var mismatch *HostKeyChanged
	if probeErr != nil && !errors.As(probeErr, &mismatch) {
		return nil
	}
	message := ""
	if mismatch != nil {
		hop := "worker"
		if mismatch.Hop == "bastion" {
			hop = "bastion"
		}
		message = "Pinned " + hop + " SSH identity changed; connection refused (expected " + mismatch.Expected + ", observed " + mismatch.Observed + ")"
	}
	if _, err := s.DB.Exec(ctx, `UPDATE nodes SET host_key_error=$2 WHERE id=$1`, id, message); err != nil {
		return err
	}
	rows, err := s.DB.Query(ctx, `SELECT team_id FROM team_nodes WHERE node_id=$1`, id)
	if err != nil {
		return err
	}
	var teams []string
	for rows.Next() {
		var team string
		if err = rows.Scan(&team); err != nil {
			break
		}
		teams = append(teams, team)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, team := range teams {
		if err = s.recordHealth(ctx, team, "host_key_changed", id, message, mismatch != nil); err != nil {
			return err
		}
	}
	return nil
}
