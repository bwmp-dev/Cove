package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestGitCredentialsAreBoundToRepositoryHost(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git executable required")
	}
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := &GitSource{Repository: "https://git.example.com:8443/team/app.git", Branch: "main", ComposePath: "compose.yaml", Token: "fixture-credential", Username: "fixture-user"}
	var configuration []string
	var environment []string
	engine.Run = func(_ context.Context, _ string, args, env []string, _ io.Writer) error {
		for i, arg := range args {
			if arg == "-c" && i+1 < len(args) && strings.HasPrefix(args[i+1], "credential.helper=") {
				configuration = append(configuration, "-c", args[i+1])
			}
		}
		environment = append(os.Environ(), env...)
		return errors.New("fixture stops after capturing Git credential invocation")
	}
	engine.checkout(context.Background(), Request{ID: "fixture", Workload: "app", Source: source}, t.TempDir(), io.Discard)
	if len(configuration) == 0 {
		t.Fatal("credential helper missing")
	}
	for _, test := range []struct {
		protocol, host string
		allowed        bool
	}{{"https", "git.example.com:8443", true}, {"https", "other.example.com", false}, {"https", "git.example.com:9443", false}, {"http", "git.example.com:8443", false}} {
		command := exec.Command("git", append(configuration, "credential", "fill")...)
		command.Env = environment
		command.Stdin = strings.NewReader("protocol=" + test.protocol + "\nhost=" + test.host + "\n\n")
		output, err := command.CombinedOutput()
		if test.allowed {
			if err != nil || !strings.Contains(string(output), "username=fixture-user") || !strings.Contains(string(output), "password=fixture-credential") {
				t.Fatalf("credential helper rejected intended host: %v", err)
			}
		} else if err == nil || strings.Contains(string(output), "fixture-credential") {
			t.Fatal("credential helper supplied credentials outside the repository origin")
		}
	}
}
