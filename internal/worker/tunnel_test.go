package worker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
