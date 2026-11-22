package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestSuspensionDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_SUSPENSION_DOCKER") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires isolated Docker/Traefik suspension fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	const workload = "suspension-acceptance"
	command := func(args ...string) string {
		t.Helper()
		out, err := e.output(ctx, args...)
		if err != nil {
			t.Fatal(args, err, out)
		}
		return out
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		ids, _ := e.output(cleanup, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+workload)
		if len(strings.Fields(ids)) > 0 {
			e.Run(cleanup, e.Root, append([]string{"docker", "rm", "-f", "-v"}, strings.Fields(ids)...), nil, io.Discard)
		}
		e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "cove-ingress"}, nil, io.Discard)
	})
	hash, err := bcrypt.GenerateFromPassword([]byte("test-password"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	routes := []Route{{Domain: "pause.example.test", Service: "app", Port: 8080, Auth: "reader:" + string(hash), Policy: &RoutePolicy{CertificateSource: "default"}}}
	compose := `services:
  init:
    image: busybox:1.37
    pull_policy: never
    command: [sh, -c, 'echo init >> /data/count']
    volumes: [data:/data]
  app:
    image: busybox:1.37
    pull_policy: never
    restart: unless-stopped
    depends_on:
      init:
        condition: service_completed_successfully
    command: [sh, -c, 'mkdir -p /www; echo ready > /www/index.html; exec httpd -f -p 8080 -h /www']
    volumes: [data:/data]
    healthcheck:
      test: [CMD, wget, -q, -O, /dev/null, 'http://127.0.0.1:8080/']
      interval: 1s
      timeout: 1s
      retries: 2
volumes:
  data: {}
`
	run := func(r Request, want string) Operation {
		t.Helper()
		r.Workload = workload
		r.Kind = "service"
		if _, err := e.Submit(r); err != nil {
			t.Fatal(err)
		}
		var jobs sync.WaitGroup
		if err := e.dispatch(ctx, &jobs); err != nil {
			t.Fatal(err)
		}
		jobs.Wait()
		op, err := e.Status(r.ID)
		logs, _ := e.Logs(r.ID)
		if err != nil || op.State != want {
			t.Fatal(op, err, logs)
		}
		return op
	}
	run(Request{ID: "deploy", Action: "deploy", Compose: compose, Routes: routes}, "succeeded")
	id := command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload, "--filter", "label=com.docker.compose.service=app")
	init := command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+workload, "--filter", "label=com.docker.compose.service=init")
	initStarted := command("docker", "inspect", "--format", "{{.State.StartedAt}}", init)
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	status := func(auth bool, want int) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://127.0.0.1/", nil)
		req.Host = "pause.example.test"
		if auth {
			req.SetBasicAuth("reader", "test-password")
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != want {
			data, _ := io.ReadAll(res.Body)
			t.Fatal(res.StatusCode, want, string(data))
		}
	}
	status(true, 200)
	status(false, 401)
	options := func(start bool) *SuspensionOptions {
		return &SuspensionOptions{Reason: "environment", Scope: "development", Start: start}
	}
	op := run(Request{ID: "pause", Action: "suspend", Suspension: options(false)}, "succeeded")
	if op.Suspension == nil || op.Suspension.PreviouslyRunning != 1 || op.Suspension.Phase != "paused" {
		t.Fatal(op)
	}
	status(true, 503)
	status(false, 401)
	if state := command("docker", "inspect", "--format", "{{.State.Running}}|{{.HostConfig.RestartPolicy.Name}}", id); state != "false|no" {
		t.Fatal(state)
	}
	op = run(Request{ID: "resume", Action: "resume", Suspension: options(true)}, "succeeded")
	if op.Suspension == nil || op.Suspension.Phase != "resumed" {
		t.Fatal(op)
	}
	status(true, 200)
	if state := command("docker", "inspect", "--format", "{{.HostConfig.RestartPolicy.Name}}", id); state != "unless-stopped" {
		t.Fatal(state)
	}
	if got := command("docker", "exec", id, "cat", "/data/count"); got != "init" {
		t.Fatal("init reran", got)
	}
	if got := command("docker", "inspect", "--format", "{{.State.StartedAt}}", init); got != initStarted {
		t.Fatal("completed init was restarted")
	}
	run(Request{ID: "pause-manual", Action: "suspend", Suspension: options(false)}, "succeeded")
	run(Request{ID: "manual-stop", Action: "stop"}, "succeeded")
	op = run(Request{ID: "resume-manual", Action: "resume", Suspension: options(true)}, "succeeded")
	if op.Suspension == nil || op.Suspension.Phase != "stopped" {
		t.Fatal(op)
	}
	if state := command("docker", "inspect", "--format", "{{.State.Running}}", id); state != "false" {
		t.Fatal("manual stop was lost", state)
	}
	run(Request{ID: "manual-start", Action: "start"}, "succeeded")
	status(true, 200)
	if got := command("docker", "exec", id, "cat", "/data/count"); got != "init" {
		t.Fatal("manual resume reran init", got)
	}
	var daemon *exec.Cmd
	call := func(r RPCRequest) (RPCResponse, error) {
		data, err := json.Marshal(r)
		if err != nil {
			return RPCResponse{}, err
		}
		var output bytes.Buffer
		if err = Proxy(ctx, filepath.Join(e.Root, "worker.sock"), bytes.NewReader(data), &output); err != nil {
			return RPCResponse{}, err
		}
		var response RPCResponse
		err = json.Unmarshal(output.Bytes(), &response)
		if response.Error != "" {
			err = fmt.Errorf("worker RPC: %s", response.Error)
		}
		return response, err
	}
	stop := func() {
		if daemon != nil {
			daemon.Process.Kill()
			daemon.Wait()
			daemon = nil
		}
	}
	defer stop()
	start := func(gate string) {
		t.Helper()
		daemon = exec.Command(os.Args[0], "rollout-daemon", e.Root)
		if gate != "" {
			daemon.Env = append(os.Environ(), "COVE_TEST_SUSPENSION_GATE="+gate)
		}
		log, err := os.OpenFile(filepath.Join(e.Root, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		daemon.Stdout, daemon.Stderr = log, log
		if err = daemon.Start(); err != nil {
			t.Fatal(err)
		}
		log.Close()
		for deadline := time.Now().Add(20 * time.Second); ; {
			if _, err := call(RPCRequest{Method: "status", ID: "deploy"}); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("worker unavailable")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	start("pause")
	pause := Request{ID: "crash-pause", Workload: workload, Kind: "service", Action: "suspend", Suspension: options(false)}
	if _, err := call(RPCRequest{Method: "submit", Request: &pause}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); ; {
		if _, err := os.Stat(filepath.Join(e.Root, "pause-gate")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pause journal was not reached")
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	start("")
	for deadline := time.Now().Add(45 * time.Second); ; {
		res, err := call(RPCRequest{Method: "status", ID: pause.ID})
		if err == nil && res.Operation != nil && res.Operation.State == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pause recovery did not settle", res.Operation, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	status(true, 503)
	status(false, 401)
	if err = os.Remove(filepath.Join(e.Root, "pause-gate")); err != nil {
		t.Fatal(err)
	}
	start("resume")
	resume := Request{ID: "crash-resume", Workload: workload, Kind: "service", Action: "resume", Suspension: options(true)}
	if _, err = call(RPCRequest{Method: "submit", Request: &resume}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; {
		if _, err = os.Stat(filepath.Join(e.Root, "pause-gate")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resume journal was not reached")
		}
		time.Sleep(50 * time.Millisecond)
	}
	resumedAt := command("docker", "inspect", "--format", "{{.State.StartedAt}}", id)
	status(true, 503)
	stop()
	start("")
	for deadline := time.Now().Add(45 * time.Second); ; {
		res, err := call(RPCRequest{Method: "status", ID: resume.ID})
		if err == nil && res.Operation != nil && res.Operation.State == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resume recovery did not settle", res.Operation, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	if now := command("docker", "inspect", "--format", "{{.State.StartedAt}}", id); now != resumedAt {
		t.Fatal("resume recovery restarted a container twice", now, resumedAt)
	}
	e, err = NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	status(true, 200)
	if got := command("docker", "exec", id, "cat", "/data/count"); got != "init" {
		t.Fatal("recovery reran init", got)
	}
	parallel := `services:
  app:
    image: busybox:1.37
    pull_policy: never
    init: true
    command: [sh, -c, 'mkdir -p /www; echo old > /www/index.html; exec httpd -f -p 8080 -h /www']
    healthcheck:
      test: [CMD, wget, -q, -O, /dev/null, 'http://127.0.0.1:8080/']
      interval: 1s
      timeout: 1s
      retries: 2
`
	rollout := &RolloutOptions{Strategy: "rolling", Replicas: map[string]int{"app": 2}, TimeoutSeconds: 20, DrainSeconds: 0, CanaryPercent: 25}
	run(Request{ID: "parallel", Action: "deploy", Compose: parallel, Routes: routes, Rollout: rollout}, "succeeded")
	rollout.Strategy = "canary"
	run(Request{ID: "candidate", Action: "deploy", Compose: strings.Replace(parallel, "echo old", "echo candidate", 1), Routes: routes, Rollout: rollout}, "succeeded")
	all := command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)
	if len(strings.Fields(all)) != 4 {
		t.Fatal("canary replicas not created", all)
	}
	run(Request{ID: "pause-stage", Action: "suspend", Suspension: options(false)}, "succeeded")
	status(true, 503)
	status(false, 401)
	staged, err := e.rolloutStatus(workload)
	if err != nil || staged == nil || staged.Phase != "waiting" || staged.CandidatePercent != 25 {
		t.Fatal(staged, err)
	}
	run(Request{ID: "promote-paused", Action: "rollout_promote"}, "failed")
	run(Request{ID: "resume-stage", Action: "resume", Suspension: options(true)}, "succeeded")
	if now := command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload); now != all {
		t.Fatal("stage resume replaced captured containers", all, now)
	}
	status(true, 200)
	run(Request{ID: "pause-abort", Action: "suspend", Suspension: options(false)}, "succeeded")
	run(Request{ID: "abort-paused", Action: "rollout_abort"}, "succeeded")
	if running := command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload); running != "" {
		t.Fatal("abort started a paused generation", running)
	}
	run(Request{ID: "resume-abort", Action: "resume", Suspension: options(true)}, "succeeded")
	remaining := strings.Fields(command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload))
	if len(remaining) != 2 {
		t.Fatal("aborted candidate was resumed", remaining)
	}
	id = remaining[0]
	run(Request{ID: "pause-missing", Action: "suspend", Suspension: options(false)}, "succeeded")
	command("docker", "rm", "-f", id)
	op = run(Request{ID: "resume-missing", Action: "resume", Suspension: options(true)}, "failed")
	if op.Suspension == nil || op.Suspension.Phase != "paused" {
		t.Fatal("missing container was reported resumed", op)
	}
	status(true, 503)
	status(false, 401)
	t.Log(fmt.Sprintf("same-container pause/resume, preserved init/restart policy/manual stop, SIGKILL recovery, authenticated 503 and missing-container failure verified"))
}
