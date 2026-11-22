package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestExecRestrictsContainerAndBoundsOutput(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 12)
	calls := 0
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		calls++
		if args[1] == "ps" {
			if !strings.Contains(strings.Join(args, " "), "project=cove-app") || !strings.Contains(strings.Join(args, " "), "service=web") {
				t.Fatal("unscoped container lookup")
			}
			_, err := io.WriteString(out, id+"\n")
			return err
		}
		if strings.Join(args, " ") != "docker exec "+id+" printf fixture" {
			t.Fatal("command escaped selected container")
		}
		_, err := io.WriteString(out, strings.Repeat("x", 70000))
		return err
	}
	request := ExecRequest{Workload: "app", Service: "web", Command: []string{"printf", "fixture"}, TimeoutSeconds: 2}
	result, err := e.Exec(context.Background(), request)
	if err != nil || !result.Truncated || len(result.Output) != 65536 || result.ExitCode != 0 || calls != 2 {
		t.Fatal(result.ExitCode, err)
	}
	request.Container = strings.Repeat("b", 12)
	if _, err = e.Exec(context.Background(), request); err == nil || calls != 3 {
		t.Fatal("foreign container accepted")
	}
	request.Container = ""
	e.active["app"] = true
	if _, err = e.Exec(context.Background(), request); err == nil {
		t.Fatal("active deployment allowed exec")
	}
	delete(e.active, "app")
	e.Run = func(ctx context.Context, _ string, args, _ []string, out io.Writer) error {
		if args[1] == "ps" {
			_, err := io.WriteString(out, id)
			return err
		}
		return errors.New("command failed")
	}
	result, err = e.Exec(context.Background(), request)
	if err != nil || result.ExitCode == 0 {
		t.Fatal("nonzero command failure hidden")
	}
	request.TimeoutSeconds = 31
	if _, err = e.Exec(context.Background(), request); err == nil {
		t.Fatal("unbounded command accepted")
	}
}

func TestExecDockerAcceptance(t *testing.T) {
	if os.Getenv("COVE_EXEC_DOCKER") != "1" {
		t.Skip("COVE_EXEC_DOCKER=1 enables isolated local Docker exec acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	image := "postgres:17-alpine"
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", image).Run(); err != nil {
		t.Skip("cached postgres:17-alpine fixture image required")
	}
	name := fmt.Sprintf("cove-exec-fixture-%d", time.Now().UnixNano())
	workload := strings.TrimPrefix(name, "cove-")
	command := exec.CommandContext(ctx, "docker", "run", "--detach", "--rm", "--network", "none", "--name", name, "--label", "com.docker.compose.project=cove-"+workload, "--label", "com.docker.compose.service=web", "--entrypoint", "sh", image, "-c", "sleep 120")
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture start failed: %v %s", err, raw)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if output, err := exec.CommandContext(cleanup, "docker", "rm", "--force", "--volumes", name).CombinedOutput(); err != nil {
			t.Errorf("fixture cleanup failed: %v %s", err, output)
		}
	})
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	containers, err := e.Containers(ctx, workload)
	if err != nil || len(containers) != 1 || containers[0].Service != "web" {
		t.Fatal("Compose service label missing from runtime metadata", err)
	}
	id := strings.TrimSpace(string(raw))
	if len(id) < 12 {
		t.Fatal("fixture omitted container ID")
	}
	request := ExecRequest{Workload: workload, Service: "web", Container: id[:12], Command: []string{"sh", "-c", "printf fixture-output; exit 7"}, TimeoutSeconds: 10}
	result, err := e.Exec(ctx, request)
	if err != nil || result.ExitCode != 7 || result.Output != "fixture-output" {
		t.Fatal("real container exit/output mismatch", result, err)
	}
	request.Command = []string{"sh", "-c", "printf '%70000s' x"}
	result, err = e.Exec(ctx, request)
	if err != nil || !result.Truncated || len(result.Output) != 65536 {
		t.Fatal("real container output limit failed", err)
	}
	request.Command = []string{"sleep", "3"}
	request.TimeoutSeconds = 1
	result, err = e.Exec(ctx, request)
	if err != nil || !result.TimedOut {
		t.Fatal("real container wait did not time out", err)
	}
}
