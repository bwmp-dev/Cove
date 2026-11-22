package worker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestGitSSHLocalServer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git executable required for local SSH acceptance")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH executable required for local SSH acceptance")
	}
	repo := filepath.Join(t.TempDir(), "repository")
	run := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture Git failed: %v: %s", err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run("init", "--initial-branch=main", repo)
	if err := os.WriteFile(filepath.Join(repo, "compose.yaml"), []byte("services:\n  app:\n    image: nginx:alpine\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("-C", repo, "add", "compose.yaml")
	run("-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
	revision := run("-C", repo, "rev-parse", "HEAD")
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	host, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	configuration := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(key.Marshal(), identity.PublicKey().Marshal()) {
			return nil, nil
		}
		return nil, os.ErrPermission
	}}
	configuration.AddHostKey(host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				server, channels, requests, err := ssh.NewServerConn(connection, configuration)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						incoming.Reject(ssh.UnknownChannelType, "session required")
						continue
					}
					channel, requests, err := incoming.Accept()
					if err != nil {
						return
					}
					func() {
						defer channel.Close()
						for request := range requests {
							if request.Type != "exec" {
								request.Reply(false, nil)
								continue
							}
							var command struct{ Command string }
							if ssh.Unmarshal(request.Payload, &command) != nil || !strings.HasPrefix(command.Command, "git-upload-pack ") {
								request.Reply(false, nil)
								return
							}
							request.Reply(true, nil)
							context, cancel := context.WithTimeout(context.Background(), 15*time.Second)
							defer cancel()
							process := exec.CommandContext(context, "git", "upload-pack", repo)
							process.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
							process.Stdin = channel
							process.Stdout = channel
							process.Stderr = channel.Stderr()
							status := uint32(0)
							if err := process.Run(); err != nil {
								status = 1
							}
							channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
							return
						}
					}()
				}
			}()
		}
	}()
	for _, valid := range []bool{true, false} {
		pin := host.PublicKey()
		if !valid {
			_, other, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			signer, err := ssh.NewSignerFromKey(other)
			if err != nil {
				t.Fatal(err)
			}
			pin = signer.PublicKey()
		}
		source := &GitSource{Repository: "ssh://git@" + listener.Addr().String() + "/repo.git", Branch: "main", ComposePath: "compose.yaml", Revision: revision, SSHPrivateKey: string(pem.EncodeToMemory(key)), SSHHostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pin)))}
		engine, err := NewEngine(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		var output bytes.Buffer
		compose, commit, err := engine.checkout(ctx, Request{ID: "fixture", Workload: "app", Source: source}, t.TempDir(), &output)
		cancel()
		if valid {
			if err != nil || commit != revision {
				t.Fatalf("pinned SSH checkout: %v %s: %s", err, commit, output.String())
			}
			if _, err = os.Stat(compose); err != nil {
				t.Fatal(err)
			}
		} else if err == nil || !strings.Contains(strings.ToLower(output.String()), "host key verification failed") {
			t.Fatalf("wrong host pin was not refused: %v: %s", err, output.String())
		}
		if strings.Contains(output.String(), source.SSHPrivateKey) {
			t.Fatal("private key exposed in Git output")
		}
	}
}
