package worker

import (
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUnhealthyRestartDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_UNHEALTHY_DOCKER") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires isolated Docker health restart fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) string {
		t.Helper()
		out, err := e.output(ctx, args...)
		if err != nil {
			t.Fatal(args, err, out)
		}
		return out
	}
	const workload = "health-acceptance"
	ids := []string{}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if len(ids) > 0 {
			e.Run(cleanup, e.Root, append([]string{"docker", "rm", "-f", "-v"}, ids...), nil, io.Discard)
		}
	})
	for i := 0; i < 3; i++ {
		id := command("docker", "run", "--detach", "--label", "com.docker.compose.project=cove-"+workload, "--label", "com.docker.compose.service=app", "--health-cmd", "test -f /tmp/healthy", "--health-interval", "1s", "--health-retries", "1", "--health-timeout", "1s", "busybox:1.37", "sh", "-c", "touch /tmp/healthy; exec sleep 300")
		ids = append(ids, id)
	}
	wait := func(id, want string) {
		t.Helper()
		until := time.Now().Add(15 * time.Second)
		for command("docker", "inspect", "--format", "{{.State.Health.Status}}", id) != want {
			if time.Now().After(until) {
				t.Fatal("health did not settle", id, want)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	for _, id := range ids {
		wait(id, "healthy")
	}
	started := []string{}
	for _, id := range ids {
		started = append(started, command("docker", "inspect", "--format", "{{.State.StartedAt}}", id))
	}
	command("docker", "exec", ids[0], "rm", "/tmp/healthy")
	command("docker", "exec", ids[2], "rm", "/tmp/healthy")
	wait(ids[0], "unhealthy")
	wait(ids[2], "unhealthy")
	command("docker", "stop", "--timeout", "1", ids[2])
	r := Request{ID: "recover-health", Workload: workload, Kind: "service", Action: "restart_unhealthy"}
	if _, err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	if err := e.dispatch(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	operation, err := e.Status(r.ID)
	logs, _ := e.Logs(r.ID)
	if err != nil || operation.State != "succeeded" {
		t.Fatal(operation, err, logs)
	}
	wait(ids[0], "healthy")
	if now := command("docker", "inspect", "--format", "{{.State.StartedAt}}", ids[0]); now == started[0] {
		t.Fatal("unhealthy container was not restarted")
	}
	if now := command("docker", "inspect", "--format", "{{.State.StartedAt}}", ids[1]); now != started[1] {
		t.Fatal("healthy container was restarted")
	}
	if state := command("docker", "inspect", "--format", "{{.State.Running}}", ids[2]); strings.TrimSpace(state) != "false" {
		t.Fatal("stopped container was restarted")
	}
}
