package worker

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTunnelConnectRespondsBeforeApplicationBytes(t *testing.T) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var address string
	for _, raw := range addresses {
		ip, _, err := net.ParseCIDR(raw.String())
		if err == nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			address = ip.String()
			break
		}
	}
	if address == "" {
		t.Skip("requires a local bridge or Ethernet address")
	}
	application, err := net.Listen("tcp", net.JoinHostPort(address, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	go func() {
		conn, err := application.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		b := make([]byte, 1)
		if _, err := io.ReadFull(conn, b); err == nil && b[0] == 'x' {
			conn.Write([]byte("y"))
		}
	}()
	e := &Engine{tunnels: map[string]*tunnelLease{"lease": {request: TunnelRequest{Port: application.Addr().(*net.TCPAddr).Port, Expires: time.Now().Add(5 * time.Second)}, container: "container", connections: map[net.Conn]bool{}}}}
	e.Run = func(_ context.Context, _ string, _, _ []string, out io.Writer) error {
		_, err := io.WriteString(out, address)
		return err
	}
	server := httptest.NewServer(http.HandlerFunc(e.serveTunnel))
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(conn, "CONNECT /tunnel/lease HTTP/1.1\r\nHost: worker\r\n\r\n")
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: "CONNECT"})
	if err != nil || response.StatusCode != 200 {
		t.Fatal("CONNECT did not acknowledge before application traffic", err)
	}
	conn.Write([]byte("x"))
	b := make([]byte, 1)
	if _, err := io.ReadFull(reader, b); err != nil || b[0] != 'y' {
		t.Fatal("acknowledged tunnel did not relay application bytes", err)
	}
}

func TestTunnelExpiryClosesConnectionsAndPreservesControllerKey(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(e.Root, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.Root, ".ssh", "authorized_keys")
	if err = os.WriteFile(path, []byte("controller-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		value := "container"
		if args[1] == "inspect" {
			value = "172.20.0.2"
		}
		_, err := io.WriteString(out, value)
		return err
	}
	in := TunnelRequest{ID: "lease", Workload: "app", Service: "db", Port: 5432, PublicKey: string(ssh.MarshalAuthorizedKey(key)), Expires: time.Now().Add(500 * time.Millisecond)}
	if err = e.OpenTunnel(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `restrict,command="/usr/local/bin/cove-worker tunnel lease"`) || strings.Contains(string(data), "port-forwarding") {
		t.Fatal("SSH key not restricted")
	}
	local, remote := net.Pipe()
	defer remote.Close()
	e.tunnelMu.Lock()
	e.tunnels[in.ID].connections[local] = true
	e.tunnelMu.Unlock()
	remote.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err = remote.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal("expiry did not close live connection", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(path)
		if string(data) == "controller-key\n" {
			return
		}
		time.Sleep(time.Millisecond * 10)
	}
	t.Fatal("expired key not removed")
}

func TestAdminToolExpiresAndIsRemovedWithoutPublishingPorts(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(e.Root, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.Root, ".ssh", "authorized_keys"), []byte("controller-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	removed := make(chan string, 1)
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		for _, arg := range args {
			if arg == "-p" || arg == "--publish" {
				t.Fatal("admin tool publishes a host port")
			}
		}
		if args[1] == "network" {
			_, err := io.WriteString(out, "cove-database")
			return err
		}
		if args[1] == "inspect" {
			_, err := io.WriteString(out, "172.20.0.2")
			return err
		}
		if args[1] == "rm" {
			removed <- args[len(args)-1]
			return nil
		}
		if args[1] == "ps" && strings.Contains(strings.Join(args, " "), "label=cove.database-tool.root=") {
			_, err := io.WriteString(out, "cove-dbtool-tool")
			return err
		}
		_, err := io.WriteString(out, "container")
		return err
	}
	in := TunnelRequest{ID: "tool", Workload: "database", Service: "postgres", Tool: "adminer", Port: 8080, PublicKey: string(ssh.MarshalAuthorizedKey(key)), Expires: time.Now().Add(500 * time.Millisecond)}
	if err = e.OpenTunnel(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	select {
	case name := <-removed:
		if name != "cove-dbtool-tool" {
			t.Fatal("removed unrelated container", name)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expired admin tool not removed")
	}
	e.tunnelMu.Lock()
	remaining := len(e.tunnels)
	e.tunnelMu.Unlock()
	if remaining != 0 {
		t.Fatal("expired admin tool lease remains")
	}
	keys, err := os.ReadFile(filepath.Join(e.Root, ".ssh", "authorized_keys"))
	if err != nil || string(keys) != "controller-key\n" {
		t.Fatal("expired admin tool key not removed", err)
	}
}
