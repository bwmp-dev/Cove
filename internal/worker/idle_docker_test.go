package worker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

func TestIdleDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_IDLE_DOCKER") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires isolated Docker/Traefik idle fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	t.Cleanup(func() {
		if t.Failed() {
			if state, err := e.readIdle("idle-acceptance"); err == nil && state != nil {
				state.Config.Token = "REDACTED"
				data, _ := json.Marshal(state)
				t.Log(string(data))
			}
			for _, name := range []string{"cove-ingress", idleContainer("idle-acceptance")} {
				out, err := e.output(context.Background(), "docker", "logs", name)
				t.Log(name, err, out)
			}
		}
	})
	const workload = "idle-acceptance"
	run := func(r Request, want string) Operation {
		t.Helper()
		r.Workload = workload
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
	command := func(args ...string) string {
		t.Helper()
		out, err := e.output(ctx, args...)
		if err != nil {
			t.Fatal(args, err)
		}
		return out
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("local-only"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	routes := []Route{{Domain: "sleep.example.test", Service: "app", Port: 8080, Auth: "reader:" + string(hash), Policy: &RoutePolicy{CertificateSource: "default"}}}
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
    volumes: [data:/data]
    command: [sh, -c, 'mkdir -p /www/prefix; echo escaped > /www/prefix/hello; echo awake > /www/index.html; exec httpd -f -p 8080 -h /www']
    healthcheck:
      test: [CMD-SHELL, 'test "$$(cat /tmp/health-mode 2>/dev/null)" != fail && wget -q -O /dev/null http://127.0.0.1:8080/']
      interval: 1s
      timeout: 1s
      retries: 1
volumes:
  data: {}
`
	run(Request{ID: "deploy", Action: "deploy", Compose: compose, Routes: routes}, "succeeded")
	id := command("docker", "ps", "-q", "--no-trunc", "--filter", "label=com.docker.compose.project=cove-"+workload)
	initID := command("docker", "ps", "-aq", "--no-trunc", "--filter", "label=com.docker.compose.project=cove-"+workload, "--filter", "label=com.docker.compose.service=init")
	initStarted := command("docker", "inspect", "--format", "{{.State.StartedAt}}", initID)
	closeWake, err := e.startIdleWake(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWake()

	originalRun := e.Run
	cancelled := false
	cancelRequest := Request{ID: "enable-cancel", Workload: workload, Action: "idle_policy", Idle: &IdlePolicy{Enabled: true, AfterSeconds: 60, RequestDriven: true}}
	e.Run = func(commandCtx context.Context, dir string, args, env []string, out io.Writer) error {
		err := originalRun(commandCtx, dir, args, env, out)
		if err == nil && !cancelled && len(args) > 1 && args[1] == "start" {
			cancelled = true
			if _, cancelErr := e.Cancel(cancelRequest); cancelErr != nil {
				return cancelErr
			}
		}
		return err
	}
	run(cancelRequest, "cancelled")
	e.Run = originalRun
	if state, err := e.readIdle(workload); err != nil || state != nil {
		t.Fatal("cancelled enable kept the policy", state, err)
	}
	run(Request{ID: "enable", Action: "idle_policy", Idle: &IdlePolicy{Enabled: true, AfterSeconds: 60, RequestDriven: true}}, "succeeded")
	client := &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	requestPath := func(auth bool, path string) (int, string, error) {
		r, err := http.NewRequestWithContext(ctx, "GET", "https://127.0.0.1"+path, nil)
		if err != nil {
			return 0, "", err
		}
		r.Host = routes[0].Domain
		if auth {
			r.SetBasicAuth("reader", "local-only")
		}
		res, err := client.Do(r)
		if err != nil {
			return 0, "", err
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		return res.StatusCode, string(body), err
	}
	request := func(auth bool) (int, string, error) { return requestPath(auth, "/") }
	assertResponse := func(auth bool, want int) {
		t.Helper()
		code, body, err := request(auth)
		if err != nil || code != want {
			t.Fatal(code, body, err)
		}
	}
	assertResponse(true, 200)
	assertResponse(false, 401)
	if code, body, err := requestPath(true, "/prefix/hello?encoded=a%2Fb"); err != nil || code != 200 || strings.TrimSpace(body) != "escaped" {
		t.Fatal("gateway changed original path/query", code, body, err)
	}
	// The helper cannot be reached from an app-side peer, even with forged proxy headers.
	state, err := e.readIdle(workload)
	if err != nil {
		t.Fatal(err)
	}
	address := strings.TrimSuffix(state.Config.Listen, ":8080")
	if err := e.Run(ctx, e.Root, []string{"docker", "exec", id, "wget", "-T", "2", "-q", "-O", "-", "http://" + address + ":8080/cove/invalid/"}, nil, io.Discard); err == nil {
		t.Fatal("app reached the front listener")
	}
	makeIdle := func() {
		t.Helper()
		command("docker", "stop", "--timeout", "1", idleContainer(workload))
		command("docker", "start", idleContainer(workload))
		for deadline := time.Now().Add(10 * time.Second); ; {
			code, err := e.idleControl(ctx, workload, "GET", "/status", nil, nil)
			if err == nil && code == 200 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(61 * time.Second)
		if err := e.sleepIdleWorkload(ctx, workload); err != nil {
			t.Fatal(err)
		}
		var jobs sync.WaitGroup
		if err := e.dispatch(ctx, &jobs); err != nil {
			t.Fatal(err)
		}
		jobs.Wait()
		suspension, err := e.readSuspension(workload)
		if err != nil || suspension == nil || suspension.Options.Reason != "idle" || suspension.Phase != "paused" {
			t.Fatal(suspension, err)
		}
		if got := command("docker", "inspect", "--format", "{{.State.Running}}|{{.HostConfig.RestartPolicy.Name}}", id); got != "false|no" {
			t.Fatal(got)
		}
	}
	makeIdle()
	paused := run(Request{ID: "environment-pause", Action: "suspend", Suspension: &SuspensionOptions{Reason: "environment", Scope: "development"}}, "succeeded")
	if paused.Suspension == nil || paused.Suspension.PreviouslyRunning != 0 {
		t.Fatal("sleeping resource was counted as previously awake", paused)
	}
	assertResponse(true, 503)
	run(Request{ID: "environment-resume", Action: "resume", Suspension: &SuspensionOptions{Reason: "environment", Scope: "development", Start: false}}, "succeeded")
	if state, err := e.readSuspension(workload); err != nil || state == nil || state.Options.Reason != "idle" || state.Phase != "paused" {
		t.Fatal("environment resume discarded prior idle intent", state, err)
	}
	assertResponse(false, 401)
	var jobs sync.WaitGroup
	response := make(chan error, 1)
	go func() {
		code, body, err := request(true)
		if err == nil && (code != 200 || !strings.Contains(body, "awake")) {
			err = fmt.Errorf("wake returned %d: %s", code, body)
		}
		response <- err
	}()
	for deadline := time.Now().Add(15 * time.Second); ; {
		e.mu.Lock()
		queued := len(e.pending) > 0
		e.mu.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request did not queue wake")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := e.dispatch(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	if err := <-response; err != nil {
		t.Fatal(err)
	}
	if got := command("docker", "ps", "-q", "--no-trunc", "--filter", "label=com.docker.compose.project=cove-"+workload); got != id {
		t.Fatal("wake replaced container", got, id)
	}
	if got := command("docker", "inspect", "--format", "{{.HostConfig.RestartPolicy.Name}}", id); got != "unless-stopped" {
		t.Fatal(got)
	}

	// Exercise crash recovery using the same journal and exact containers, with a shortened private test quiesce.
	time.Sleep(1100 * time.Millisecond)
	const crashEpisode = "idle-crash"
	if code, err := e.idleControl(ctx, workload, "POST", "/quiesce", map[string]any{"seconds": 1, "episode": crashEpisode}, nil); err != nil || code != 204 {
		t.Fatal(code, err)
	}
	run(Request{ID: crashEpisode, Action: "idle_sleep"}, "succeeded")
	closeWake()
	var daemon *exec.Cmd
	stopDaemon := func() {
		if daemon != nil {
			daemon.Process.Kill()
			daemon.Wait()
			daemon = nil
		}
	}
	defer stopDaemon()
	startDaemon := func(gate bool) {
		t.Helper()
		daemon = exec.Command(os.Args[0], "rollout-daemon", e.Root)
		if gate {
			daemon.Env = append(os.Environ(), "COVE_TEST_SUSPENSION_GATE=resume")
		}
		log, err := os.OpenFile(filepath.Join(e.Root, "idle-daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		daemon.Stdout, daemon.Stderr = log, log
		err = daemon.Start()
		log.Close()
		if err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(15 * time.Second); ; {
			// The wake-only socket belongs to the new process, not the stopped parent fixture.
			connection, err := net.DialTimeout("unix", filepath.Join(e.Root, "worker.sock"), time.Second)
			if err == nil {
				connection.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("restarted worker did not listen")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	startDaemon(true)
	crashedResponse := make(chan error, 1)
	go func() { _, _, err := request(true); crashedResponse <- err }()
	for deadline := time.Now().Add(30 * time.Second); ; {
		if _, err := os.Stat(filepath.Join(e.Root, "pause-gate")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := os.ReadFile(filepath.Join(e.Root, "idle-daemon.log"))
			t.Fatal("wake did not reach restart-policy gate", string(logs))
		}
		time.Sleep(50 * time.Millisecond)
	}
	started := command("docker", "inspect", "--format", "{{.State.StartedAt}}", id)
	stopDaemon()
	startDaemon(false)
	for deadline := time.Now().Add(30 * time.Second); ; {
		op, err := e.Status("wake-" + crashEpisode)
		if err == nil && op.State == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := e.Logs("wake-" + crashEpisode)
			t.Fatal("wake recovery failed", op, err, logs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	<-crashedResponse
	assertResponse(true, 200)
	if got := command("docker", "inspect", "--format", "{{.State.StartedAt}}", id); got != started {
		t.Fatal("wake recovery restarted the container a second time", got, started)
	}
	stopDaemon()
	e, err = NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	run(Request{ID: "manual-stop", Action: "stop"}, "succeeded")
	assertResponse(true, 503)
	run(Request{ID: "manual-start", Action: "start"}, "succeeded")
	assertResponse(true, 200)

	closeWake, err = e.startIdleWake(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWake()
	quickSleep := func(episode string) {
		t.Helper()
		time.Sleep(1100 * time.Millisecond)
		if code, err := e.idleControl(ctx, workload, "POST", "/quiesce", map[string]any{"seconds": 1, "episode": episode}, nil); err != nil || code != 204 {
			t.Fatal(code, err)
		}
		run(Request{ID: episode, Action: "idle_sleep"}, "succeeded")
	}
	quickSleep("idle-health-failure")
	modePath := filepath.Join(e.Root, "health-mode")
	if err = os.WriteFile(modePath, []byte("fail"), 0600); err != nil {
		t.Fatal(err)
	}
	command("docker", "cp", modePath, id+":/tmp/health-mode")
	failedResponse := make(chan int, 1)
	go func() { code, _, _ := request(true); failedResponse <- code }()
	for deadline := time.Now().Add(15 * time.Second); ; {
		e.mu.Lock()
		queued := len(e.pending) > 0
		e.mu.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed-health request did not queue wake")
		}
		time.Sleep(25 * time.Millisecond)
	}
	var failedJobs sync.WaitGroup
	if err = e.dispatch(ctx, &failedJobs); err != nil {
		t.Fatal(err)
	}
	failedJobs.Wait()
	if code := <-failedResponse; code != 503 {
		t.Fatal("failed health was exposed", code)
	}
	op, err := e.Status("wake-idle-health-failure")
	if err != nil || op.State != "failed" {
		t.Fatal(op, err)
	}
	if got := command("docker", "inspect", "--format", "{{.State.Running}}|{{.HostConfig.RestartPolicy.Name}}", id); got != "false|no" {
		t.Fatal("failed wake left containers running", got)
	}
	failedStarted := command("docker", "inspect", "--format", "{{.State.StartedAt}}", id)
	assertResponse(true, 503)
	if got := command("docker", "inspect", "--format", "{{.State.StartedAt}}", id); got != failedStarted {
		t.Fatal("public retry restarted an already failed episode")
	}
	if status, err := e.IdleStatus(ctx, workload); err != nil || status.Phase != "wake_failed" {
		t.Fatal("failed wake was not visible", status, err)
	}
	if err = os.WriteFile(modePath, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	command("docker", "cp", modePath, id+":/tmp/health-mode")
	run(Request{ID: "wake-repair", Action: "start"}, "succeeded")
	assertResponse(true, 200)
	quickSleep("idle-disable-asleep")
	run(Request{ID: "disable", Action: "idle_policy", Idle: &IdlePolicy{}}, "succeeded")
	assertResponse(true, 200)
	if state, err := e.readIdle(workload); err != nil || state != nil {
		t.Fatal(state, err)
	}
	if got := command("docker", "ps", "-aq", "--filter", "label=cove.managed=idle"); got != "" {
		t.Fatal("helper remained after disabling", got)
	}
	if _, err := os.Stat(filepath.Join(e.Root, "workloads", workload, "suspension.json")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if got := command("docker", "inspect", "--format", "{{.State.StartedAt}}", initID); got != initStarted {
		t.Fatal("idle wake restarted completed init", got, initStarted)
	}
	if got := command("docker", "exec", id, "cat", "/data/count"); got != "init" {
		t.Fatal("init ran more than once", got)
	}
	data, _ := json.Marshal(map[string]any{"container": id, "wake": "same container, healthy, first request succeeded"})
	t.Log(string(data))
}
