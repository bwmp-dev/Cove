package worker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDurableOperationLifecycle(t *testing.T) {
	root := t.TempDir()
	engine, err := NewEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{ID: "release-1", Workload: "app", Action: "deploy", Compose: "services:\n  web:\n    image: nginx:alpine\n", Environment: map[string]string{"PASSWORD": "very-secret-value"}}
	op, err := engine.Submit(request)
	if err != nil || op.State != "queued" {
		t.Fatal(err)
	}
	if _, err = engine.Submit(request); err != nil {
		t.Fatal(err)
	}
	different := request
	different.Compose = "changed"
	if _, err = engine.Submit(different); err == nil {
		t.Fatal("conflicting replay accepted")
	}
	engine, err = NewEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	engine.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		calls++
		if handled, err := releaseFixture(args, out); handled {
			return err
		}
		_, err := out.Write([]byte("PASSWORD=very-secret-value\n"))
		return err
	}
	var jobs sync.WaitGroup
	if err = engine.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	op, err = engine.Status(request.ID)
	if err != nil || op.State != "succeeded" || calls != 9 {
		t.Fatalf("unexpected result %+v calls=%d err=%v", op, calls, err)
	}
	logs, err := engine.Logs(request.ID)
	if err != nil || strings.Contains(logs, "very-secret-value") || !strings.Contains(logs, "[redacted]") {
		t.Fatal("secret redaction failed")
	}
	if _, err = engine.Submit(request); err != nil {
		t.Fatal(err)
	}
	if err = engine.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	if calls != 9 {
		t.Fatal("completed operation executed again")
	}
	if _, err = os.Stat(filepath.Join(root, "workloads", "app", "current.json")); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedExecutionIsNotRetried(t *testing.T) {
	engine, _ := NewEngine(t.TempDir())
	r := Request{ID: "op", Workload: "app", Action: "stop"}
	engine.Submit(r)
	j, _ := engine.read(r.ID)
	j.Operation.State = "running"
	if err := writeJSON(engine.path(r.ID), j); err != nil {
		t.Fatal(err)
	}
	if err := engine.Recover(); err != nil {
		t.Fatal(err)
	}
	op, _ := engine.Status(r.ID)
	if op.State != "interrupted" || op.FinishedAt == nil {
		t.Fatal("interrupted execution not preserved")
	}
}

func TestWorkloadSerialization(t *testing.T) {
	engine, _ := NewEngine(t.TempDir())
	dir := filepath.Join(engine.Root, "workloads", "app")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "current.json"), map[string]string{"compose_path": filepath.Join(dir, "compose.yaml")}); err != nil {
		t.Fatal(err)
	}
	engine.Submit(Request{ID: "first", Workload: "app", Action: "stop"})
	engine.Submit(Request{ID: "second", Workload: "app", Action: "stop"})
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	engine.Run = func(context.Context, string, []string, []string, io.Writer) error {
		started <- struct{}{}
		<-release
		return nil
	}
	var jobs sync.WaitGroup
	if err := engine.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	<-started
	select {
	case <-started:
		t.Fatal("concurrent operations on one workload")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	jobs.Wait()
}

func TestRejectUnsafeRequests(t *testing.T) {
	for _, r := range []Request{{ID: "../escape", Workload: "app", Action: "stop"}, {ID: "ok", Workload: "../escape", Action: "stop"}, {ID: "ok", Workload: "app", Action: "shell"}, {ID: "ok", Workload: "app", Action: "stop", Environment: map[string]string{"DOCKER_HOST": "tcp://other"}}} {
		if ValidateRequest(r) == nil {
			t.Fatalf("accepted unsafe request: %+v", r)
		}
	}
}

func TestStopAfterFailedFirstDeployment(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		if args[1] == "ps" {
			if !strings.Contains(strings.Join(args, " "), "label=com.docker.compose.project=cove-app") {
				t.Fatal("missing workload scope")
			}
			_, err := io.WriteString(out, `{"ID":"0123456789ab","Names":"app","State":"running"}`)
			return err
		}
		if strings.Join(args, " ") != "docker stop --time 30 0123456789ab" {
			t.Fatalf("unexpected stop: %v", args)
		}
		stopped = true
		return nil
	}
	if _, err = e.Submit(Request{ID: "stop", Workload: "app", Action: "stop"}); err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	if err = e.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	op, err := e.Status("stop")
	if err != nil || op.State != "succeeded" || !stopped {
		t.Fatalf("stop without current release failed: %+v %v", op, err)
	}
}

func TestLogRedactionAcrossChunks(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	logger := &boundedLog{file: f, remaining: 1024, secrets: []string{"secret\nvalue"}}
	for _, chunk := range []string{"before sec", "ret\n", "val", "ue after"} {
		if _, err = logger.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err = logger.flush(true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "before [redacted] after" {
		t.Fatalf("unexpected log: %s", data)
	}
}

func TestLogRedactionPreservesGeneratedNamesAcrossChunks(t *testing.T) {
	const project = "cove-0123456789abcdef0123456789abcdef"
	input := "Container " + project + "-app-1 Started; password=9; token=abc-secret"
	expected := "Container " + project + "-app-1 Started; password=[redacted]; token=[redacted]"
	for split := 0; split <= len(input); split++ {
		var output strings.Builder
		logger := &boundedLog{file: &output, remaining: 4096, secrets: []string{"9", "abc-secret"}, publicIdentifiers: []string{project}}
		if _, err := logger.Write([]byte(input[:split])); err != nil {
			t.Fatal(err)
		}
		if _, err := logger.Write([]byte(input[split:])); err != nil {
			t.Fatal(err)
		}
		if err := logger.flush(true); err != nil {
			t.Fatal(err)
		}
		if output.String() != expected {
			t.Fatalf("split %d: %s", split, output.String())
		}
	}
}
