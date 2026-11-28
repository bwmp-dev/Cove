package worker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestGitSSHKeyPinningAndCredentialCleanup(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	source := &GitSource{Repository: "ssh://git@git.example.com/team/app.git", Branch: "main", ComposePath: "compose.yaml", SSHPrivateKey: string(pem.EncodeToMemory(block)), SSHHostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))}
	if err = validateSource(source); err != nil {
		t.Fatal(err)
	}
	for _, failed := range []bool{false, true} {
		root := t.TempDir()
		engine, err := NewEngine(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		credentialDirectory := ""
		engine.Run = func(_ context.Context, dir string, args, environment []string, out io.Writer) error {
			if args[1] == "rev-parse" {
				_, err := io.WriteString(out, strings.Repeat("a", 40))
				return err
			}
			if args[len(args)-2] != source.Repository {
				return nil
			}
			command := ""
			for _, env := range environment {
				if strings.HasPrefix(env, "GIT_SSH_COMMAND=") {
					command = strings.TrimPrefix(env, "GIT_SSH_COMMAND=")
				}
			}
			for _, required := range []string{"StrictHostKeyChecking=yes", "HostKeyAlias=cove-git", "IdentitiesOnly=yes", "IdentityAgent=none", "GlobalKnownHostsFile=/dev/null", "PasswordAuthentication=no"} {
				if !strings.Contains(command, required) {
					t.Fatalf("missing SSH boundary %s", required)
				}
			}
			if strings.Contains(strings.Join(args, " ")+command, source.SSHPrivateKey) {
				t.Fatal("private key in command arguments")
			}
			_, identity, found := strings.Cut(command, " -i '")
			if !found {
				t.Fatal("missing identity")
			}
			identity = strings.TrimSuffix(identity, "'")
			credentialDirectory = filepath.Dir(identity)
			key, err := os.ReadFile(identity)
			if err != nil || string(key) != source.SSHPrivateKey {
				t.Fatal("identity was not private key")
			}
			known, err := os.ReadFile(filepath.Join(credentialDirectory, "known_hosts"))
			if err != nil || string(known) != "cove-git "+source.SSHHostKey+"\n" {
				t.Fatal("incorrect host pin")
			}
			if failed {
				return errors.New("fixture clone failed")
			}
			target := args[len(args)-1]
			if err = os.MkdirAll(target, 0700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(target, "compose.yaml"), []byte("services: {}"), 0600)
		}
		_, _, err = engine.checkout(context.Background(), Request{ID: "test", Workload: "app", Source: source}, root, io.Discard)
		if (err != nil) != failed {
			t.Fatalf("checkout error %v", err)
		}
		if credentialDirectory == "" {
			t.Fatal("SSH configuration not used")
		}
		if _, err = os.Stat(credentialDirectory); !os.IsNotExist(err) {
			t.Fatalf("credential directory survived checkout: %v", err)
		}
	}
	for _, repository := range []string{"ssh://git:password@git.example.com/app.git", "ssh://-option@git.example.com/app.git", "ssh://git@git.example.com/app.git?command=bad"} {
		copy := *source
		copy.Repository = repository
		if validateSource(&copy) == nil {
			t.Fatalf("accepted unsafe repository %s", repository)
		}
	}
	copy := *source
	copy.SSHHostKey = ""
	if validateSource(&copy) == nil {
		t.Fatal("accepted missing host key")
	}
	copy = *source
	copy.SSHPrivateKey = "not-a-private-key"
	if validateSource(&copy) == nil {
		t.Fatal("accepted invalid private key")
	}
}
