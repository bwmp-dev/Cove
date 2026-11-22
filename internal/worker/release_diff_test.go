package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseComparisonUsesActualRetainedImagesAndCommits(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "workloads", "app", "release-states")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "operations"), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Root: root, Run: func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		if len(args) > 1 && args[1] == "log" {
			_, err := io.WriteString(out, strings.Repeat("b", 40)+"\tFix runtime behavior\n")
			return err
		}
		_, err := io.WriteString(out, "false\n")
		return err
	}}
	for _, id := range []string{"from", "to"} {
		revision := strings.Repeat("a", 40)
		image := "sha256:" + strings.Repeat("a", 64)
		if id == "to" {
			revision = strings.Repeat("b", 40)
			image = "sha256:" + strings.Repeat("b", 64)
		}
		if err := writeJSON(e.path(id), journal{Operation: Operation{ID: id, Workload: "app", State: "succeeded"}}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, id+".compose.json")
		if err := writeJSON(path, map[string]any{"services": map[string]any{"web": map[string]string{"image": image}}}); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(dir, id+".json"), deployedState{ComposePath: path, Revision: revision, Environment: map[string]string{"PASSWORD": "private-value-" + id}}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := e.CompareReleases(context.Background(), ReleaseComparisonRequest{Workload: "app", From: "from", To: "to"}, nil)
	if err != nil || len(result.Images) != 1 || !result.Images[0].Changed || len(result.Commits) != 1 || !result.CommitsComplete {
		t.Fatal(result, err)
	}
	raw, err := json.Marshal(result)
	if err != nil || !result.EnvironmentChanges["PASSWORD"] || strings.Contains(string(raw), "private-value-") {
		t.Fatal("effective variable changes must be reported without values", string(raw), err)
	}
	if _, err = e.CompareReleases(context.Background(), ReleaseComparisonRequest{Workload: "other", From: "from", To: "to"}, nil); err == nil {
		t.Fatal("cross-workload release accepted")
	}
	for _, id := range []string{"from", "to"} {
		if err := writeJSON(filepath.Join(dir, id+".json"), deployedState{ComposePath: filepath.Join(dir, id+".compose.json")}); err != nil {
			t.Fatal(err)
		}
	}
	result, err = e.CompareReleases(context.Background(), ReleaseComparisonRequest{Workload: "app", From: "from", To: "to"}, nil)
	if err != nil || !result.CommitsComplete || len(result.Commits) != 0 || !strings.Contains(result.CommitMessage, "Neither release is Git-backed") {
		t.Fatal("non-Git releases must not report incomplete Git history", result, err)
	}
}

func TestReleaseComparisonReadsRealLocalGitHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git executable required")
	}
	root := t.TempDir()
	checkout := filepath.Join(root, "workloads", "app", "releases", "to")
	states := filepath.Join(root, "workloads", "app", "release-states")
	for _, dir := range []string{checkout, states, filepath.Join(root, "operations")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		var output strings.Builder
		if err := RunCommand(context.Background(), checkout, append([]string{"git"}, args...), nil, &output); err != nil {
			t.Fatalf("fixture Git failed: %v", err)
		}
		return strings.TrimSpace(output.String())
	}
	git("init", "--quiet")
	commit := func(value string) string {
		if err := os.WriteFile(filepath.Join(checkout, "file.txt"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		git("add", "file.txt")
		git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "-m", value)
		return git("rev-parse", "HEAD")
	}
	from := commit("first")
	commit("second")
	to := commit("third")
	e := &Engine{Root: root, Run: RunCommand}
	for id, revision := range map[string]string{"from": from, "to": to} {
		path := filepath.Join(states, id+".compose.json")
		if err := writeJSON(path, map[string]any{"services": map[string]any{"app": map[string]string{"image": "sha256:" + strings.Repeat("a", 64)}}}); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(states, id+".json"), deployedState{ComposePath: path, Revision: revision}); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(e.path(id), journal{Operation: Operation{ID: id, Workload: "app", State: "succeeded"}}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := e.CompareReleases(context.Background(), ReleaseComparisonRequest{Workload: "app", From: "from", To: "to"}, nil)
	if err != nil || !result.CommitsComplete || len(result.Commits) != 2 || result.Commits[0].Subject != "third" || result.Commits[1].Subject != "second" {
		t.Fatal(result, err)
	}
}
