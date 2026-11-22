package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func releaseFixture(args []string, out io.Writer) (bool, error) {
	var text string
	switch {
	case args[len(args)-1] == "json":
		text = `{"services":{"app":{"image":"mutable:latest","build":".","environment":{"VALUE":"literal$value"}}}}`
	case len(args) > 1 && args[1] == "ps":
		text = "testcontainer"
	case len(args) > 1 && args[1] == "inspect":
		text = "sha256:" + strings.Repeat("a", 64)
	default:
		return false, nil
	}
	_, err := io.WriteString(out, text)
	return true, err
}

func TestRollbackUsesRetainedImageAndLiteralConfiguration(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		commands = append(commands, args)
		_, err := releaseFixture(args, out)
		return err
	}
	r := Request{ID: "original", Workload: "app", Action: "deploy", Compose: "services: {}"}
	var jobs sync.WaitGroup
	run := func(r Request) {
		t.Helper()
		if _, err = e.Submit(r); err != nil {
			t.Fatal(err)
		}
		if err = e.dispatch(context.Background(), &jobs); err != nil {
			t.Fatal(err)
		}
		jobs.Wait()
		op, err := e.Status(r.ID)
		if err != nil || op.State != "succeeded" {
			t.Fatalf("%+v %v", op, err)
		}
	}
	run(r)
	data, err := os.ReadFile(filepath.Join(e.Root, "workloads", "app", "release-states", "original.compose.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err = json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"build"`) || !strings.Contains(string(data), "sha256:") || !strings.Contains(string(data), "literal$$value") {
		t.Fatalf("snapshot not immutable: %s", data)
	}
	commands = nil
	run(Request{ID: "rollback", Workload: "app", Action: "rollback", Release: "original"})
	if len(commands) != 1 || !strings.Contains(strings.Join(commands[0], " "), "--no-build --pull never") || !strings.Contains(strings.Join(commands[0], " "), "original.compose.json") {
		t.Fatalf("rollback rebuilt or pulled: %v", commands)
	}
	if _, err = e.Submit(Request{ID: "cross", Workload: "other", Action: "rollback", Release: "original"}); err != nil {
		t.Fatal(err)
	}
	if err = e.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	op, err := e.Status("cross")
	if err != nil || op.State != "failed" {
		t.Fatal("cross-workload rollback accepted")
	}
}
