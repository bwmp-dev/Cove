package connect

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestMain(m *testing.M) {
	if os.Getenv("COVE_CONNECT_TEST_SSH") == "1" {
		if _, err := http.ReadRequest(bufio.NewReader(os.Stdin)); err != nil {
			os.Exit(1)
		}
		io.WriteString(os.Stdout, "HTTP/1.0 200 OK\r\nConnection: close\r\n\r\nprivate-admin-tool")
		// Close the simulated SSH stream before the race runtime delays process exit.
		if err := os.Stdout.Close(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestConnectRelaysResponseUntilServerClosesWithoutWaitingForClientEOF(t *testing.T) {
	root := t.TempDir()
	name := "ssh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), data, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COVE_CONNECT_TEST_SSH", "1")
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strings.Split(listener.Addr().String(), ":")[1]
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []string{"--host", "127.0.0.1", "--local-port", port, "--host-key", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), "--expires", time.Now().Add(time.Minute).Format(time.RFC3339)})
	}()
	client := &http.Client{Timeout: time.Second}
	var response *http.Response
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, err = client.Get("http://127.0.0.1:" + port)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "private-admin-tool" {
		t.Fatal("SSH response waited for client EOF", err, string(body))
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not stop after its connections closed")
	}
}
