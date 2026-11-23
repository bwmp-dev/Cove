package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type TunnelRequest struct {
	Credentials *DatabaseToolCredentials `json:"credentials,omitempty"`
	Tool        string                   `json:"tool,omitempty"`
	ID          string                   `json:"id"`
	Workload    string                   `json:"workload"`
	Service     string                   `json:"service"`
	Port        int                      `json:"port"`
	PublicKey   string                   `json:"public_key"`
	Expires     time.Time                `json:"expires_at"`
}
type tunnelLease struct {
	closed      bool
	helper      string
	request     TunnelRequest
	container   string
	connections map[net.Conn]bool
	timer       *time.Timer
}

func ValidateTunnel(in TunnelRequest) error {
	if err := validateDatabaseTool(in); err != nil {
		return err
	}
	if !identifier.MatchString(in.ID) || !workloadIdentifier.MatchString(in.Workload) || !serviceIdentifier.MatchString(in.Service) || in.Port < 1 || in.Port > 65535 || len(in.PublicKey) > 8192 {
		return errors.New("invalid database access request")
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(in.PublicKey))
	if err != nil || len(options) != 0 || len(rest) != 0 || key.Type() != ssh.KeyAlgoED25519 {
		return errors.New("provide one Ed25519 public key without options")
	}
	if time.Until(in.Expires) <= 0 || time.Until(in.Expires) > time.Hour {
		return errors.New("access must expire within one hour")
	}
	return nil
}

func (e *Engine) tunnelKeys(id, line string) error {
	path := filepath.Join(e.Root, ".ssh", "authorized_keys")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := []string{}
	for _, existing := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(existing) != "" && !strings.HasSuffix(existing, " cove-tunnel:"+id) {
			lines = append(lines, existing)
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".tunnel-keys-")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	_, err = io.WriteString(file, strings.Join(lines, "\n")+"\n")
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (e *Engine) OpenTunnel(ctx context.Context, in TunnelRequest) error {
	if err := ValidateTunnel(in); err != nil {
		return err
	}
	e.tunnelMu.Lock()
	defer e.tunnelMu.Unlock()
	if e.tunnels == nil {
		e.tunnels = map[string]*tunnelLease{}
	}
	if _, exists := e.tunnels[in.ID]; exists {
		return errors.New("access ID already exists")
	}
	if len(e.tunnels) >= 32 {
		return errors.New("node temporary access limit reached")
	}
	ids, err := e.output(ctx, "docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+in.Workload, "--filter", "label=com.docker.compose.service="+in.Service)
	if err != nil || len(strings.Fields(ids)) != 1 {
		return errors.New("select exactly one running service container")
	}
	container := strings.TrimSpace(ids)
	helper := ""
	if in.Tool != "" {
		if err = writeJSON(filepath.Join(e.Root, "database-tools.json"), true); err != nil {
			return err
		}
		for _, lease := range e.tunnels {
			if lease.request.Workload == in.Workload && lease.helper != "" {
				return errors.New("close the existing admin tool before opening another")
			}
		}
		helper = "cove-dbtool-" + in.ID
		defer func() {
			if e.tunnels[in.ID] == nil {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				if cleanupErr := e.removeDatabaseTool(cleanup, helper, in.ID); cleanupErr != nil {
					lease := &tunnelLease{request: in, helper: helper, closed: true, connections: map[net.Conn]bool{}}
					e.tunnels[in.ID] = lease
					lease.timer = time.AfterFunc(15*time.Second, func() {
						if err := e.CloseTunnel(in.ID); err != nil {
							slog.Error("admin tool cleanup requires retry", "access", in.ID)
						}
					})
				}
			}
		}()
		network := "cove-" + in.Workload + "_default"
		owner, inspectErr := e.output(ctx, "docker", "network", "inspect", "--format", `{{index .Labels "com.docker.compose.project"}}`, network)
		if inspectErr != nil || owner != "cove-"+in.Workload {
			return errors.New("private admin tool requires the database's managed default network")
		}
		seconds := strconv.Itoa(int(time.Until(in.Expires).Seconds()) + 1)
		image, memory, command, environment, mounts, toolErr := e.databaseToolCommand(in)
		if toolErr != nil {
			return toolErr
		}
		args := []string{"docker", "run", "-d", "--name", helper, "--log-driver", "none", "--label", "cove.database-tool=true", "--label", fmt.Sprintf("cove.database-tool.root=%x", sha256.Sum256([]byte(e.Root))), "--label", "cove.database-tool.workload=" + in.Workload, "--network", network, "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", memory, "--cpus", "0.5"}
		for _, value := range environment {
			name, _, _ := strings.Cut(value, "=")
			args = append(args, "-e", name)
		}
		args = append(args, mounts...)
		args = append(args, "--entrypoint", "timeout", image, seconds)
		args = append(args, command...)
		if err = e.Run(ctx, e.Root, args, environment, io.Discard); err != nil {
			return errors.New("could not start the private admin tool")
		}
		container = helper
	}
	if _, err = e.tunnelTarget(ctx, container, in.Port); err != nil {
		return err
	}
	key, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(in.PublicKey))
	existing, err := os.ReadFile(filepath.Join(e.Root, ".ssh", "authorized_keys"))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(existing), "\n") {
		other, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(line))
		if parseErr == nil && string(other.Marshal()) == string(key.Marshal()) {
			return errors.New("use a dedicated key not already authorized on this node")
		}
	}
	line := fmt.Sprintf("restrict,command=\"/usr/local/bin/cove-worker tunnel %s\",expiry-time=\"%s\" %s cove-tunnel:%s", in.ID, in.Expires.UTC().Format("20060102150405Z"), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), in.ID)
	if err = e.tunnelKeys(in.ID, line); err != nil {
		return err
	}
	lease := &tunnelLease{request: in, container: container, helper: helper, connections: map[net.Conn]bool{}}
	e.tunnels[in.ID] = lease
	lease.timer = time.AfterFunc(time.Until(in.Expires), func() {
		if err := e.CloseTunnel(in.ID); err != nil {
			slog.Error("expired access closed but SSH key cleanup failed", "access", in.ID, "error", err)
		}
	})
	return nil
}

func (e *Engine) CloseTunnel(id string) error {
	if !identifier.MatchString(id) {
		return errors.New("invalid access ID")
	}
	e.tunnelMu.Lock()
	defer e.tunnelMu.Unlock()
	if lease := e.tunnels[id]; lease != nil {
		lease.closed = true
		if lease.timer != nil {
			lease.timer.Stop()
		}
		for conn := range lease.connections {
			conn.Close()
		}
		err := e.tunnelKeys(id, "")
		if lease.helper != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err = errors.Join(err, e.removeDatabaseTool(ctx, lease.helper, id))
		}
		if err != nil {
			lease.timer = time.AfterFunc(15*time.Second, func() {
				if err := e.CloseTunnel(id); err != nil {
					slog.Error("expired admin tool cleanup requires retry", "access", id)
				}
			})
			return err
		}
		delete(e.tunnels, id)
		return nil
	}
	return e.tunnelKeys(id, "")
}

func (e *Engine) removeOrphanDatabaseTools() error {
	if _, err := os.Stat(filepath.Join(e.Root, "database-tools.json")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ids, err := e.output(ctx, "docker", "ps", "-aq", "--filter", fmt.Sprintf("label=cove.database-tool.root=%x", sha256.Sum256([]byte(e.Root))))
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(ids) {
		if err = e.Run(ctx, e.Root, []string{"docker", "rm", "-f", "-v", id}, nil, io.Discard); err != nil {
			return errors.New("could not remove orphaned private admin tool")
		}
	}
	return os.RemoveAll(filepath.Join(e.Root, "database-tools"))
}

func (e *Engine) tunnelTarget(ctx context.Context, container string, port int) (string, error) {
	raw, err := e.output(ctx, "docker", "inspect", "--format", `{{if .State.Running}}{{if ne .HostConfig.NetworkMode "host"}}{{range .NetworkSettings.Networks}}{{.IPAddress}}{{"\n"}}{{end}}{{end}}{{end}}`, container)
	if err != nil {
		return "", errors.New("database container unavailable")
	}
	for _, address := range strings.Fields(raw) {
		ip := net.ParseIP(address)
		if ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
		}
	}
	return "", errors.New("database access requires a running container on a bridge network")
}

func (e *Engine) serveTunnel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/tunnel/")
	e.tunnelMu.Lock()
	lease := e.tunnels[id]
	if lease == nil || lease.closed || time.Now().After(lease.request.Expires) || len(lease.connections) >= 16 {
		e.tunnelMu.Unlock()
		http.Error(w, "access unavailable", 403)
		return
	}
	target, err := e.tunnelTarget(r.Context(), lease.container, lease.request.Port)
	if err != nil {
		e.tunnelMu.Unlock()
		http.Error(w, "database unavailable", 502)
		return
	}
	remote, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		e.tunnelMu.Unlock()
		http.Error(w, "database unavailable", 502)
		return
	}
	conn, buffer, err := http.NewResponseController(w).Hijack()
	if err != nil {
		remote.Close()
		e.tunnelMu.Unlock()
		return
	}
	conn.SetDeadline(lease.request.Expires)
	remote.SetDeadline(lease.request.Expires)
	lease.connections[conn] = true
	lease.connections[remote] = true
	e.tunnelMu.Unlock()
	defer func() {
		conn.Close()
		remote.Close()
		e.tunnelMu.Lock()
		delete(lease.connections, conn)
		delete(lease.connections, remote)
		e.tunnelMu.Unlock()
	}()
	if _, err = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err = buffer.Flush(); err != nil {
		return
	}
	done := make(chan struct{})
	go func() { io.Copy(remote, buffer); remote.Close(); close(done) }()
	io.Copy(conn, remote)
	conn.Close()
	<-done
}

func TunnelProxy(ctx context.Context, socket, id string, in io.Reader, out io.Writer) error {
	if !identifier.MatchString(id) {
		return errors.New("invalid access ID")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err = fmt.Fprintf(conn, "CONNECT /tunnel/%s HTTP/1.1\r\nHost: worker\r\n\r\n", id); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: "CONNECT"})
	if err != nil {
		return err
	}
	if response.StatusCode != 200 {
		return errors.New("database access expired, revoked, or unavailable")
	}
	go func() {
		io.Copy(conn, in)
		if unix, ok := conn.(*net.UnixConn); ok {
			unix.CloseWrite()
		}
	}()
	_, err = io.Copy(out, reader)
	return err
}
