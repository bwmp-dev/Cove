package worker

import (
	"bufio"
	"context"
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
	ID        string    `json:"id"`
	Workload  string    `json:"workload"`
	Service   string    `json:"service"`
	Port      int       `json:"port"`
	PublicKey string    `json:"public_key"`
	Expires   time.Time `json:"expires_at"`
}
type tunnelLease struct {
	request     TunnelRequest
	container   string
	connections map[net.Conn]bool
	timer       *time.Timer
}

func ValidateTunnel(in TunnelRequest) error {
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
	lease := &tunnelLease{request: in, container: container, connections: map[net.Conn]bool{}}
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
		lease.timer.Stop()
		for conn := range lease.connections {
			conn.Close()
		}
		delete(e.tunnels, id)
	}
	return e.tunnelKeys(id, "")
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
	if lease == nil || time.Now().After(lease.request.Expires) || len(lease.connections) >= 16 {
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
