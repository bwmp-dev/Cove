package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestJobDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_JOB_DOCKER") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires isolated Docker job fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const workload = "job-acceptance"
	command := func(args ...string) string {
		t.Helper()
		value, err := e.output(ctx, args...)
		if err != nil {
			t.Fatal(args, err, value)
		}
		return value
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		ids, _ := e.output(cleanup, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+workload)
		if ids != "" {
			e.Run(cleanup, e.Root, append([]string{"docker", "rm", "-f", "-v"}, strings.Fields(ids)...), nil, io.Discard)
		}
		networks, _ := e.output(cleanup, "docker", "network", "ls", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)
		for _, id := range strings.Fields(networks) {
			e.Run(cleanup, e.Root, []string{"docker", "network", "rm", id}, nil, io.Discard)
		}
	})
	compose := func(script string) string {
		data, _ := json.Marshal(map[string]any{"services": map[string]any{
			"init":    map[string]any{"image": "busybox:1.37", "pull_policy": "never", "command": []string{"sh", "-c", "echo initialized"}},
			"sidecar": map[string]any{"image": "busybox:1.37", "pull_policy": "never", "restart": "always", "command": []string{"sleep", "300"}},
			"app":     map[string]any{"image": "busybox:1.37", "pull_policy": "never", "restart": "always", "environment": map[string]string{"SECRET": "${SECRET:-}"}, "depends_on": map[string]any{"init": map[string]any{"condition": "service_completed_successfully"}, "sidecar": map[string]any{"condition": "service_started"}}, "command": []string{"sh", "-c", script}},
		}})
		return string(data)
	}
	submit := func(id, script string, seconds int) Request {
		t.Helper()
		r := Request{ID: id, Workload: workload, Kind: "job", Action: "deploy", JobService: "app", Compose: compose(script), Environment: map[string]string{"SECRET": "job-sensitive-value"}, Job: &JobOptions{TimeoutSeconds: seconds}}
		if _, err := e.Submit(r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	finish := func(id, want string) Operation {
		t.Helper()
		operation, err := e.Status(id)
		logs, _ := e.Logs(id)
		if err != nil || operation.State != want || operation.Job == nil || !operation.Job.ContainersStopped {
			t.Fatal(operation, err, logs)
		}
		if running := command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload); running != "" {
			t.Fatal("job containers still running", running)
		}
		if strings.Contains(logs, "job-sensitive-value") {
			t.Fatal("job secret leaked")
		}
		return operation
	}
	run := func(id, script, want string, seconds int) Operation {
		t.Helper()
		submit(id, script, seconds)
		var jobs sync.WaitGroup
		if err := e.dispatch(ctx, &jobs); err != nil {
			t.Fatal(err)
		}
		jobs.Wait()
		return finish(id, want)
	}
	success := run("success", "sleep 1; echo stdout; echo stderr >&2; echo $$SECRET >&2", "succeeded", 30)
	logs, _ := e.Logs("success")
	if success.Job.ExitCode == nil || *success.Job.ExitCode != 0 || !strings.Contains(logs, "stdout") || !strings.Contains(logs, "stderr") || !strings.Contains(logs, "[redacted]") {
		t.Fatal(success, logs)
	}
	failed := run("failure", "echo broken >&2; exit 42", "failed", 30)
	if failed.Job.ExitCode == nil || *failed.Job.ExitCode != 42 {
		t.Fatal(failed)
	}
	timed := run("timeout", "sleep 300", "failed", 5)
	if !timed.Job.TimedOut {
		t.Fatal("timeout not recorded", timed)
	}
	r := submit("cancel", "sleep 300", 30)
	var jobs sync.WaitGroup
	if err := e.dispatch(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for command("docker", "ps", "-q", "--filter", "label=dev.cove.job-run=cancel", "--filter", "label=com.docker.compose.service=app") == "" {
		if time.Now().After(deadline) {
			t.Fatal("job did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := e.Cancel(r); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	finish("cancel", "cancelled")
	var daemon *exec.Cmd
	stop := func() {
		if daemon != nil {
			daemon.Process.Kill()
			daemon.Wait()
			daemon = nil
		}
	}
	t.Cleanup(stop)
	call := func(r RPCRequest) (RPCResponse, error) {
		data, _ := json.Marshal(r)
		var out bytes.Buffer
		err := Proxy(ctx, filepath.Join(e.Root, "worker.sock"), bytes.NewReader(data), &out)
		var response RPCResponse
		if err == nil {
			err = json.Unmarshal(out.Bytes(), &response)
		}
		if response.Error != "" {
			err = fmt.Errorf("RPC: %s", response.Error)
		}
		return response, err
	}
	start := func() {
		t.Helper()
		daemon = exec.Command(os.Args[0], "rollout-daemon", e.Root)
		log, err := os.OpenFile(filepath.Join(e.Root, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		daemon.Stdout, daemon.Stderr = log, log
		if err := daemon.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		log.Close()
		until := time.Now().Add(15 * time.Second)
		for {
			if _, err := call(RPCRequest{Method: "status", ID: "success"}); err == nil {
				break
			}
			if time.Now().After(until) {
				t.Fatal("daemon unavailable")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	start()
	restart := Request{ID: "restart", Workload: workload, Kind: "job", Action: "deploy", JobService: "app", Compose: compose("echo once; sleep 5; echo recovered $$SECRET >&2"), Environment: map[string]string{"SECRET": "sensitive-recovery"}, Job: &JobOptions{TimeoutSeconds: 30}}
	if _, err := call(RPCRequest{Method: "submit", Request: &restart}); err != nil {
		t.Fatal(err)
	}
	var before string
	deadline = time.Now().Add(20 * time.Second)
	for before == "" {
		before = command("docker", "ps", "-q", "--filter", "label=dev.cove.job-run=restart", "--filter", "label=com.docker.compose.service=app")
		if time.Now().After(deadline) {
			operation, _ := e.Status(restart.ID)
			logs, _ := e.Logs(restart.ID)
			daemonLog, _ := os.ReadFile(filepath.Join(e.Root, "daemon.log"))
			t.Fatal("restart job did not start", operation, logs, string(daemonLog))
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	start()
	deadline = time.Now().Add(30 * time.Second)
	for {
		op, err := e.Status(restart.ID)
		if err != nil {
			t.Fatal(err)
		}
		if op.State != "running" && op.State != "queued" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job did not recover", op)
		}
		time.Sleep(200 * time.Millisecond)
	}
	stop()
	recovered := finish("restart", "succeeded")
	after := command("docker", "ps", "-aq", "--filter", "label=dev.cove.job-run=restart", "--filter", "label=com.docker.compose.service=app")
	if before != after || recovered.Job.ExitCode == nil || *recovered.Job.ExitCode != 0 {
		t.Fatal("restart re-executed the job", before, after, recovered)
	}
	logs, _ = e.Logs(restart.ID)
	if strings.Count(logs, " once") != 1 || !strings.Contains(logs, "recovered [redacted]") || strings.Contains(logs, "sensitive-recovery") {
		t.Fatal("recovery lost or repeated job output", logs)
	}
	start()
	restart.ID, restart.Compose, restart.Job.TimeoutSeconds = "deadline", compose("sleep 300"), 5
	if _, err := call(RPCRequest{Method: "submit", Request: &restart}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(20 * time.Second)
	for command("docker", "ps", "-q", "--filter", "label=dev.cove.job-run=deadline", "--filter", "label=com.docker.compose.service=app") == "" {
		if time.Now().After(deadline) {
			t.Fatal("deadline job did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	time.Sleep(3 * time.Second)
	start()
	deadline = time.Now().Add(20 * time.Second)
	for {
		op, _ := e.Status("deadline")
		if op.State != "running" && op.State != "queued" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovery reset the execution deadline", op)
		}
		time.Sleep(200 * time.Millisecond)
	}
	stop()
	expired := finish("deadline", "failed")
	if !expired.Job.TimedOut {
		t.Fatal("recovered deadline not enforced", expired)
	}
}
