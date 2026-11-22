package worker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestCheckoutPermissionsKeepPrivateMetadataAndReadableBuildInputs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(parent, "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"appsettings.json": 0600, "start.sh": 0700, ".git/config": 0600} {
		if err := os.WriteFile(filepath.Join(checkout, name), []byte("fixture"), mode); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(parent, "private")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(checkout, "link")); err != nil {
		t.Fatal(err)
	}
	if err := prepareCheckoutPermissions(checkout); err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]os.FileMode{parent: 0700, checkout: 0755, filepath.Join(checkout, "appsettings.json"): 0644, filepath.Join(checkout, "start.sh"): 0755, filepath.Join(checkout, ".git"): 0700, filepath.Join(checkout, ".git/config"): 0600, outside: 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != expected {
			t.Fatalf("unexpected permissions for %s: %v", path, err)
		}
	}
}

func TestGitDeploymentPreservesReleaseDirectory(t *testing.T) {
	e, _ := NewEngine(t.TempDir())
	commit := strings.Repeat("a", 40)
	source := &GitSource{Repository: "https://example.com/team/app.git", Branch: "main", ComposePath: "app/compose.yaml", Token: "private-repository-token"}
	var commands [][]string
	e.Run = func(_ context.Context, dir string, args, environment []string, out io.Writer) error {
		commands = append(commands, args)
		if strings.Contains(strings.Join(args, " "), source.Token) {
			t.Fatal("credential appeared in command arguments")
		}
		if args[0] == "git" && args[len(args)-2] == source.Repository {
			target := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(target, "app"), 0700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(target, "app", "compose.yaml"), []byte("services: {}"), 0600)
		}
		if args[0] == "git" && args[1] == "rev-parse" {
			_, err := io.WriteString(out, commit+"\n")
			return err
		}
		if handled, err := releaseFixture(args, out); handled {
			return err
		}
		return nil
	}
	request := Request{ID: "git-release", Workload: "app", Action: "deploy", Source: source}
	if _, err := e.Submit(request); err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	if err := e.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	result, _ := e.Status(request.ID)
	if result.State != "succeeded" || result.Revision != commit {
		t.Fatalf("unexpected deployment: %+v", result)
	}
	if len(commands) != 12 {
		t.Fatalf("expected checkout, deployment and immutable snapshot commands; got %d", len(commands))
	}
	current, err := os.ReadFile(filepath.Join(e.Root, "workloads", "app", "current.json"))
	if err != nil || !strings.Contains(string(current), "git-release") {
		t.Fatal("release path was not retained")
	}
}

func TestGitSourceValidation(t *testing.T) {
	for _, source := range []*GitSource{{Repository: "file:///etc", Branch: "main", ComposePath: "compose.yaml"}, {Repository: "https://user:secret@example.com/repo", Branch: "main", ComposePath: "compose.yaml"}, {Repository: "https://example.com/repo", Branch: "--upload-pack=bad", ComposePath: "compose.yaml"}, {Repository: "https://example.com/repo", Branch: "main", ComposePath: "../../compose.yaml"}} {
		if validateSource(source) == nil {
			t.Fatalf("accepted unsafe source: %+v", source)
		}
	}
}

func TestGitInlineComposeUsesCheckoutWithoutChangingRepositoryFiles(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	commit := strings.Repeat("b", 40)
	source := &GitSource{Repository: "https://example.com/team/app.git", Branch: "main", InlineCompose: true}
	request := Request{ID: "inline-release", Workload: "app", Action: "deploy", Source: source, Compose: "services:\n  app:\n    build: .\n"}
	if err := ValidateRequest(request); err != nil {
		t.Fatal(err)
	}
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		if strings.Contains(strings.Join(args, " "), " clone ") {
			target := args[len(args)-1]
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(target, "compose.yaml"), []byte("repository configuration"), 0600)
		}
		if args[0] == "git" && args[1] == "rev-parse" {
			_, err := io.WriteString(out, commit)
			return err
		}
		return nil
	}
	path, revision, err := e.checkout(context.Background(), request, e.Root, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if revision != commit || filepath.Dir(path) != filepath.Join(e.Root, "releases", request.ID) {
		t.Fatal("inline Compose did not use the checked-out revision and build context")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != request.Compose {
		t.Fatal("saved deployment recipe was not used", err)
	}
	data, err = os.ReadFile(filepath.Join(filepath.Dir(path), "compose.yaml"))
	if err != nil || string(data) != "repository configuration" {
		t.Fatal("repository file was overwritten", err)
	}
	request.Compose = ""
	if ValidateRequest(request) == nil {
		t.Fatal("inline source accepted without Compose")
	}
	request.Compose = "services: {}"
	request.Preview = true
	source.Revision = commit
	if ValidateRequest(request) == nil {
		t.Fatal("private panel configuration exposed to preview code")
	}
	source.ComposePath = "compose.yaml"
	if validateSource(source) == nil {
		t.Fatal("ambiguous Compose source accepted")
	}
}
