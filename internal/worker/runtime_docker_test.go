package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestRuntimeDockerHealth(t *testing.T) {
	if os.Getenv("COVE_TEST_RUNTIME_DOCKER") != "1" {
		t.Skip("set COVE_TEST_RUNTIME_DOCKER=1 for isolated Docker health inspection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "runtime-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	names := []string{}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		for _, name := range names {
			if err := e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "-v", name}, nil, io.Discard); err != nil {
				t.Error(err)
			}
		}
	})
	for _, kind := range []string{"healthy", "unhealthy", "starting", "migrate"} {
		name := "cove-" + id + "-" + kind
		args := []string{"docker", "run", "--name", name, "--label", "com.docker.compose.project=cove-" + id, "--label", "com.docker.compose.service=" + kind}
		if kind != "migrate" {
			check := "true"
			if kind == "unhealthy" {
				check = "false"
			}
			if kind == "starting" {
				check = "sleep 30; true"
			}
			args = append(args, "--detach", "--health-cmd", check, "--health-interval", "1s", "--health-retries", "1")
			args = append(args, "busybox:1.37", "sleep", "90")
		} else {
			args = append(args, "busybox:1.37", "true")
		}
		names = append(names, name)
		if err = e.Run(ctx, e.Root, args, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(e.Root, "workloads", id)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(dir, "release.compose.json")
	if err = os.WriteFile(compose, []byte(`{"services":{"healthy":{"depends_on":{"migrate":{"condition":"service_completed_successfully"}}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(dir, "current.json"), deployedState{ComposePath: compose}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		containers, err := e.HealthContainers(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		states := map[string]string{}
		for _, container := range containers {
			states[container.Service] = container.Health
			if container.Service == "migrate" {
				if !container.ExpectedCompleted || container.ExitCode == nil || *container.ExitCode != 0 {
					t.Fatal(container)
				}
				states["migrate"] = "completed"
			}
		}
		if states["healthy"] == "healthy" && states["unhealthy"] == "unhealthy" && states["starting"] == "starting" && states["migrate"] == "completed" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(fmt.Sprintf("Docker health states did not settle: %v", states))
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
