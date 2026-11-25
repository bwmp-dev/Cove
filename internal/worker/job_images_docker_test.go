package worker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestJobImageCheckDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_JOB_DOCKER") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires isolated Docker job fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workload := "job-image-" + strings.ToLower(rand.Text()[:16])
	command := func(args ...string) string {
		t.Helper()
		value, err := e.output(ctx, args...)
		if err != nil {
			t.Fatal("fixture command failed", args[1], err)
		}
		return value
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		ids, err := e.output(cleanup, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+workload)
		if err != nil {
			t.Error(err)
			return
		}
		if ids != "" {
			if err := e.Run(cleanup, e.Root, append([]string{"docker", "rm", "-f", "-v"}, strings.Fields(ids)...), nil, io.Discard); err != nil {
				t.Error(err)
			}
		}
		networks, err := e.output(cleanup, "docker", "network", "ls", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)
		if err != nil {
			t.Error(err)
			return
		}
		for _, network := range strings.Fields(networks) {
			if err := e.Run(cleanup, e.Root, []string{"docker", "network", "rm", network}, nil, io.Discard); err != nil {
				t.Error(err)
			}
		}
	})
	image := command("docker", "image", "inspect", "--format", "{{.Id}}", "busybox:1.37")
	run := func(id, action, compose, want string) Operation {
		t.Helper()
		request := Request{ID: id, Workload: workload, Kind: "job", JobService: "task", Action: action, Compose: compose}
		if action == "deploy" {
			request.Job = &JobOptions{TimeoutSeconds: 10}
		}
		if _, err := e.Submit(request); err != nil {
			t.Fatal(err)
		}
		var operations sync.WaitGroup
		if err := e.dispatch(ctx, &operations); err != nil {
			t.Fatal(err)
		}
		operations.Wait()
		operation, err := e.Status(id)
		if err != nil || operation.State != want {
			logs, _ := e.Logs(id)
			t.Fatal(operation.State, err, logs)
		}
		return operation
	}
	never := run("never", "image_check", "", "failed")
	logs, err := e.Logs("never")
	if err != nil || !strings.Contains(logs, "run the job") || never.ImageCheck != nil {
		t.Fatal("never-run job appeared checked", never.Error, logs)
	}
	dependency := command("docker", "run", "--detach", "--pull", "never", "--name", "cove-"+workload+"-dependency", "--label", "com.docker.compose.project=cove-"+workload, "--label", "com.docker.compose.service=db", image, "true")
	partial := run("dependency-only", "image_check", "", "failed")
	logs, err = e.Logs("dependency-only")
	if err != nil || !strings.Contains(logs, "do not include job service") || partial.ImageCheck != nil {
		t.Fatal("dependency-only check appeared complete", err, logs)
	}
	command("docker", "rm", "-v", dependency)
	for _, attempt := range []struct{ id, script, state string }{{"success", "echo once > /tmp/job-image-marker", "succeeded"}, {"failure", "echo failed > /tmp/job-image-marker; exit 17", "failed"}} {
		data, _ := json.Marshal(map[string]any{"services": map[string]any{"task": map[string]any{"image": image, "pull_policy": "never", "command": []string{"sh", "-ec", attempt.script}}}})
		run(attempt.id, "deploy", string(data), attempt.state)
		container := command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+workload)
		if len(strings.Fields(container)) != 1 {
			t.Fatal("expected one retained job container")
		}
		snapshot := func() string {
			return command("docker", "inspect", "--format", "{{.Id}}|{{.State.StartedAt}}|{{.State.FinishedAt}}|{{.State.ExitCode}}|{{.State.Running}}|{{.Image}}", container)
		}
		before := snapshot()
		marker := filepath.Join(e.Root, attempt.id+".marker")
		command("docker", "cp", container+":/tmp/job-image-marker", marker)
		original, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		checked := run(attempt.id+"-check", "image_check", "", "succeeded")
		if checked.ImageCheck == nil || len(checked.ImageCheck.Images) != 1 || checked.ImageCheck.Images[0].ImageID != image || checked.ImageCheck.Images[0].Status != "pinned" || before != snapshot() {
			t.Fatal("checking changed the retained job or image identity")
		}
		command("docker", "cp", container+":/tmp/job-image-marker", marker)
		after, err := os.ReadFile(marker)
		if err != nil || string(after) != string(original) {
			t.Fatal("job marker changed during metadata inspection", err)
		}
	}
}
