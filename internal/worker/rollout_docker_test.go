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
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "build-cache-filesystem" {
		if err := BuildCacheFilesystem(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 2 && os.Args[1] == "registry-storage" {
		file, err := os.Open("/config/registry.json")
		if err == nil {
			err = RunManagedRegistryStorage("/registry", file, os.Stdout)
			_ = file.Close()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) >= 2 && (os.Args[1] == "cloudflare-connector-origin-probe" || os.Args[1] == "cloudflare-origin" || os.Args[1] == "cloudflare-origin-probe" || os.Args[1] == "cloudflare-probe" || os.Args[1] == "cloudflare-fake-edge" || os.Args[1] == "cloudflare-fixture-client" || os.Args[1] == "tunnel" && os.Getenv("COVE_TEST_FAKE_EDGE_URL") != "") {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		var err error
		switch os.Args[1] {
		case "cloudflare-connector-origin-probe":
			err = RunCloudflareConnectorOriginProbe(ctx)
		case "cloudflare-origin":
			err = RunCloudflareOrigin(ctx)
		case "cloudflare-origin-probe":
			err = RunCloudflareOriginProbe(ctx)
		case "cloudflare-probe":
			revision := ""
			if len(os.Args) > 3 {
				revision = os.Args[3]
			}
			err = RunCloudflareProbe(ctx, os.Args[2], revision)
		case "cloudflare-fake-edge":
			err = runCloudflareFakeEdge(ctx)
		case "cloudflare-fixture-client":
			err = runCloudflareFixtureClient(ctx)
		case "tunnel":
			err = runCloudflareFakeConnector(ctx)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if len(os.Args) == 4 && os.Args[1] == "transport-route-probe" {
		if err := runTransportRouteProbe(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 3 && os.Args[1] == "transport-route-fixture" {
		if err := runTransportRouteFixture(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if (len(os.Args) == 7 || len(os.Args) == 8) && os.Args[1] == "private-route-probe" {
		if err := runPrivateRouteTestProbe(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if (len(os.Args) == 2 || len(os.Args) == 3) && os.Args[1] == "certificate-probe" {
		path := "/config/probes.json"
		if len(os.Args) == 3 {
			path = os.Args[2]
		}
		file, err := os.Open(path)
		if err == nil {
			err = RunRouteCertificateProbe(context.Background(), file)
			file.Close()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if len(os.Args) == 2 && os.Args[1] == "route-helper" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		if err := RunRouteAuxiliary(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 2 && os.Args[1] == "idle-gateway" {
		if err := RunIdleGateway(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 3 && os.Args[1] == "rollout-daemon" {
		e, err := NewEngine(os.Args[2])
		if err == nil {
			e.ACMEDirectory = "https://127.0.0.1:9/directory"
			if gate := os.Getenv("COVE_TEST_SUSPENSION_GATE"); gate != "" {
				run := e.Run
				e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
					err := run(ctx, dir, args, env, out)
					if err == nil && len(args) > 2 && args[1] == "update" && (gate == "pause" && args[2] == "--restart=no" || gate == "resume" && strings.HasPrefix(args[2], "--restart=") && args[2] != "--restart=no") {
						if err := os.WriteFile(filepath.Join(e.Root, "pause-gate"), []byte("ready"), 0600); err != nil {
							return err
						}
						<-ctx.Done()
						return ctx.Err()
					}
					return err
				}
			}
			err = e.Listen(context.Background(), filepath.Join(e.Root, "worker.sock"))
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 3 && os.Args[1] == "healthcheck" {
		if err := HTTPHealth(context.Background(), os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRolloutDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_ROLLOUT_DOCKER") != "1" || runtime.GOOS != "linux" {
		t.Skip("isolated Linux Docker daemon required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 11*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	workload := fmt.Sprintf("rollout-%x", time.Now().UnixNano())
	command := func(args ...string) string {
		t.Helper()
		var output strings.Builder
		if err := e.Run(ctx, e.Root, args, nil, &output); err != nil {
			t.Fatal(args, err, output.String())
		}
		return strings.TrimSpace(output.String())
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		ids, _ := e.output(cleanup, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+workload)
		for _, id := range strings.Fields(ids) {
			e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "-v", id}, nil, io.Discard)
		}
		e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "cove-ingress"}, nil, io.Discard)
		networks, _ := e.output(cleanup, "docker", "network", "ls", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)
		for _, id := range strings.Fields(networks) {
			e.Run(cleanup, e.Root, []string{"docker", "network", "rm", id}, nil, io.Discard)
		}
		e.Run(cleanup, e.Root, []string{"docker", "network", "rm", "cove-ingress"}, nil, io.Discard)
		e.Run(cleanup, e.Root, []string{"docker", "volume", "rm", "cove-ingress"}, nil, io.Discard)
	})
	options := func(strategy string, replicas int) *RolloutOptions {
		return &RolloutOptions{Strategy: strategy, Replicas: map[string]int{"app": replicas}, TimeoutSeconds: 20, DrainSeconds: 1, CanaryPercent: 25}
	}
	compose := func(marker string, delay int, healthy bool, check bool) string {
		service := map[string]any{"image": "busybox:1.37", "pull_policy": "never", "environment": map[string]string{"LITERAL": "literal$$value"}, "command": []string{"sh", "-c", fmt.Sprintf("sleep %d; mkdir -p /www; echo %s-$$(cat /etc/hostname) > /www/index.html; exec httpd -f -p 8080 -h /www", delay, marker)}}
		if check {
			test := []string{"CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/"}
			if !healthy {
				test = []string{"CMD", "false"}
			}
			service["healthcheck"] = map[string]any{"test": test, "interval": "1s", "timeout": "1s", "retries": 1, "start_period": "10s"}
		}
		data, _ := json.Marshal(map[string]any{"services": map[string]any{"app": service}})
		return string(data)
	}
	routes := []Route{{Domain: "rollout.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "default"}}}
	run := func(r Request, expected string) Operation {
		t.Helper()
		r.Workload = workload
		r.Kind = "service"
		if r.Action == "deploy" || r.Action == "rollback" {
			r.Routes = routes
		}
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
		if err != nil || operation.State != expected {
			t.Fatal(operation, err, logs)
		}
		return operation
	}
	run(Request{ID: "first", Action: "deploy", Compose: compose("first", 0, true, true), Rollout: options("rolling", 2)}, "succeeded")
	run(Request{ID: "enable-idle", Action: "idle_policy", Idle: &IdlePolicy{Enabled: true, AfterSeconds: 60, RequestDriven: true}}, "succeeded")
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true}}
	request := func() (string, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", "https://127.0.0.1/", nil)
		if err != nil {
			return "", err
		}
		req.Host = "rollout.example.test"
		res, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		data, err := io.ReadAll(io.LimitReader(res.Body, 4096))
		if res.StatusCode != 200 {
			return "", fmt.Errorf("HTTP %d", res.StatusCode)
		}
		return strings.TrimSpace(string(data)), err
	}
	if body, err := request(); err != nil || !strings.HasPrefix(body, "first-") {
		t.Fatal(body, err)
	}
	var failures, requests atomic.Int64
	continuous, stop := context.WithCancel(ctx)
	var traffic sync.WaitGroup
	traffic.Go(func() {
		for continuous.Err() == nil {
			if _, err := request(); err != nil {
				failures.Add(1)
			}
			requests.Add(1)
			time.Sleep(20 * time.Millisecond)
		}
	})
	defer func() {
		stop()
		traffic.Wait()
		if failures.Load() != 0 {
			t.Errorf("traffic failures %d of %d", failures.Load(), requests.Load())
		}
	}()
	run(Request{ID: "second", Action: "deploy", Compose: compose("second", 3, true, true), Rollout: options("rolling", 3)}, "succeeded")
	seen := map[string]bool{}
	for i := 0; i < 60; i++ {
		body, err := request()
		if err != nil || !strings.HasPrefix(body, "second-") {
			t.Fatal(body, err)
		}
		seen[body] = true
	}
	if len(seen) != 3 {
		t.Fatal("replicas not balanced", seen)
	}
	currentPath := filepath.Join(e.Root, "workloads", workload, "current.json")
	before, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	ids := command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)
	run(Request{ID: "unhealthy", Action: "deploy", Compose: compose("unhealthy", 0, false, true), Rollout: options("rolling", 2)}, "failed")
	after, err := os.ReadFile(currentPath)
	if err != nil || string(before) != string(after) {
		t.Fatal("unhealthy deployment changed release", err)
	}
	if current := command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload); current != ids {
		t.Fatal("failed health replaced old containers", ids, current)
	}
	run(Request{ID: "canary", Action: "deploy", Compose: compose("canary", 0, true, true), Rollout: options("canary", 2)}, "succeeded")
	releases, err := e.Releases(workload)
	if err != nil {
		t.Fatal(err)
	}
	for _, release := range releases {
		if release.ID == "canary" {
			t.Fatal("unpromoted candidate was offered as a rollback release")
		}
	}
	counts := map[string]int{}
	for i := 0; i < 80; i++ {
		body, err := request()
		if err != nil {
			t.Fatal(err)
		}
		counts[strings.Split(body, "-")[0]]++
	}
	if counts["canary"] < 10 || counts["canary"] > 30 || counts["second"] < 50 {
		t.Fatal("canary weights not applied", counts)
	}
	run(Request{ID: "abort", Action: "rollout_abort"}, "succeeded")
	if tags := command("docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference=cove-release/"+workload+":canary-*"); tags != "" {
		t.Fatal("aborted candidate retained image tags", tags)
	}
	if body, err := request(); err != nil || !strings.HasPrefix(body, "second-") {
		t.Fatal(body, err)
	}
	run(Request{ID: "green", Action: "deploy", Compose: compose("green", 0, true, true), Rollout: options("blue_green", 2)}, "succeeded")
	for i := 0; i < 15; i++ {
		body, err := request()
		if err != nil || !strings.HasPrefix(body, "second-") {
			t.Fatal("blue-green leaked candidate traffic", body, err)
		}
	}
	run(Request{ID: "promote", Action: "rollout_promote"}, "succeeded")
	if body, err := request(); err != nil || !strings.HasPrefix(body, "green-") {
		t.Fatal(body, err)
	}
	run(Request{ID: "disable-idle", Action: "idle_policy", Idle: &IdlePolicy{}}, "succeeded")
	image := "cove-rollout-health:" + workload
	buildDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte("FROM busybox:1.37\nHEALTHCHECK --interval=1s --timeout=1s --start-period=10s --retries=1 CMD wget -q -O /dev/null http://127.0.0.1:8080/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command("docker", "build", "--pull=false", "--tag", image, buildDir)
	t.Cleanup(func() { e.Run(context.Background(), e.Root, []string{"docker", "image", "rm", image}, nil, io.Discard) })
	var multi map[string]any
	json.Unmarshal([]byte(compose("multi", 1, true, false)), &multi)
	services := multi["services"].(map[string]any)
	app := services["app"].(map[string]any)
	app["image"] = image
	app["depends_on"] = map[string]any{"peer": map[string]any{"condition": "service_healthy"}}
	app["command"] = []string{"sh", "-c", "wget -q -O /dev/null http://peer:8081/; mkdir -p /www; echo multi-$$(cat /etc/hostname) > /www/index.html; exec httpd -f -p 8080 -h /www"}
	services["peer"] = map[string]any{"image": "busybox:1.37", "pull_policy": "never", "command": []string{"sh", "-c", "mkdir -p /www; echo peer > /www/index.html; exec httpd -f -p 8081 -h /www"}, "healthcheck": map[string]any{"test": []string{"CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8081/"}, "interval": "1s", "timeout": "1s", "retries": 1, "start_period": "10s"}}
	multiCompose, _ := json.Marshal(multi)
	multiOptions := options("rolling", 2)
	multiOptions.Replicas["peer"] = 2
	run(Request{ID: "multiple-services", Action: "deploy", Compose: string(multiCompose), Rollout: multiOptions}, "succeeded")
	if body, err := request(); err != nil || !strings.HasPrefix(body, "multi-") {
		t.Fatal("generation service aliases or inherited health failed", body, err)
	}
	if ids := strings.Fields(command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)); len(ids) != 4 {
		t.Fatal("multi-service replicas were not preserved", ids)
	}
	httpOptions := options("rolling", 2)
	httpOptions.HTTPChecks = map[string]HTTPHealthCheck{"app": {Port: 8080, Path: "/"}}
	run(Request{ID: "http-health", Action: "deploy", Compose: compose("http", 1, true, false), Rollout: httpOptions}, "succeeded")
	if body, err := request(); err != nil || !strings.HasPrefix(body, "http-") {
		t.Fatal(body, err)
	}
	if status, err := e.rolloutStatus(workload); err != nil || status != nil {
		t.Fatal(status, err)
	}
	run(Request{ID: "rollback-first", Action: "rollback", Release: "first"}, "succeeded")
	if body, err := request(); err != nil || !strings.HasPrefix(body, "first-") {
		t.Fatal("immutable rollback failed", body, err)
	}
	active := strings.Fields(command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload))
	if len(active) != 2 {
		t.Fatal(active)
	}
	if value := command("docker", "exec", active[0], "printenv", "LITERAL"); value != "literal$value" {
		t.Fatal("literal dollars changed", value)
	}
	if reference := command("docker", "inspect", "--format", `{{index .Config.Labels "dev.cove.image-reference"}}`, active[0]); reference != "busybox:1.37" {
		t.Fatal("rollback lost the configured image reference", reference)
	}
	canceled := Request{ID: "canceled", Workload: workload, Action: "deploy", Kind: "service", Routes: routes, Compose: compose("cancel", 30, true, true), Rollout: options("rolling", 2)}
	if _, err = e.Submit(canceled); err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	if err = e.dispatch(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); ; {
		if ids := command("docker", "ps", "-aq", "--filter", "label=dev.cove.generation=canceled"); ids != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancel candidate did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err = e.Cancel(canceled); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	if operation, _ := e.Status(canceled.ID); operation.State != "cancelled" {
		logs, _ := e.Logs(canceled.ID)
		t.Fatal(operation, logs)
	}
	if ids := strings.Fields(command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)); strings.Join(ids, " ") != strings.Join(active, " ") {
		t.Fatal("cancellation changed old containers", active, ids)
	}
	var daemon *exec.Cmd
	stopDaemon := func() {
		if daemon != nil {
			daemon.Process.Kill()
			daemon.Wait()
			daemon = nil
		}
	}
	t.Cleanup(stopDaemon)
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
	startDaemon := func() {
		t.Helper()
		log, err := os.OpenFile(filepath.Join(e.Root, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		daemon = exec.Command(os.Args[0], "rollout-daemon", e.Root)
		daemon.Stdout, daemon.Stderr = log, log
		if err = daemon.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		log.Close()
		for deadline := time.Now().Add(30 * time.Second); ; {
			if _, err := call(RPCRequest{Method: "status", ID: "first"}); err == nil {
				break
			}
			if time.Now().After(deadline) {
				logs, _ := os.ReadFile(filepath.Join(e.Root, "daemon.log"))
				t.Fatal("daemon unavailable", string(logs))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	await := func(id, state string) {
		t.Helper()
		for deadline := time.Now().Add(90 * time.Second); ; {
			response, err := call(RPCRequest{Method: "status", ID: id})
			if err == nil && response.Operation != nil && response.Operation.State == state {
				return
			}
			if time.Now().After(deadline) {
				logs, _ := e.Logs(id)
				t.Fatal("worker recovery did not settle", id, response, err, logs)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	startDaemon()
	restart := Request{ID: "restart-before-switch", Workload: workload, Action: "deploy", Kind: "service", Routes: routes, Compose: compose("restart", 30, true, true), Rollout: options("rolling", 2)}
	if _, err = call(RPCRequest{Method: "submit_routes_v4", Request: &restart}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); ; {
		if ids := command("docker", "ps", "-aq", "--filter", "label=dev.cove.generation="+restart.ID); ids != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restart candidate did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	stopDaemon()
	startDaemon()
	await(restart.ID, "interrupted")
	if ids := strings.Fields(command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)); strings.Join(ids, " ") != strings.Join(active, " ") {
		t.Fatal("restart before switch changed old containers", active, ids)
	}
	restart.ID = "restart-after-switch"
	restart.Compose = compose("committed", 0, true, true)
	restart.Rollout = options("rolling", 2)
	restart.Rollout.DrainSeconds = 5
	if _, err = call(RPCRequest{Method: "submit_routes_v4", Request: &restart}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(40 * time.Second); ; {
		j, err := e.read(restart.ID)
		if err == nil && j.RolloutChange != nil && j.RolloutChange.Committed {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := e.Logs(restart.ID)
			t.Fatal("deployment did not commit", logs)
		}
		time.Sleep(50 * time.Millisecond)
	}
	stopDaemon()
	startDaemon()
	await(restart.ID, "succeeded")
	if body, err := request(); err != nil || !strings.HasPrefix(body, "committed-") {
		t.Fatal("restart did not finish committed switch", body, err)
	}
	if ids := strings.Fields(command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)); len(ids) != 2 {
		t.Fatal("retired containers survived recovery", ids)
	}
	staged := Request{ID: "restart-staged", Workload: workload, Kind: "service", Action: "deploy", Compose: compose("staged", 0, true, true), Routes: routes, Rollout: options("canary", 2)}
	if _, err := call(RPCRequest{Method: "submit_routes_v4", Request: &staged}); err != nil {
		t.Fatal(err)
	}
	await(staged.ID, "succeeded")
	stopDaemon()
	startDaemon()
	response, err := call(RPCRequest{Method: "containers", ID: workload})
	if err != nil || response.Rollout == nil || response.Rollout.Phase != "waiting" || response.Rollout.CandidatePercent != 25 {
		t.Fatal("staged traffic did not survive worker restart", response.Rollout, err)
	}
	abort := Request{ID: "restart-staged-abort", Workload: workload, Kind: "service", Action: "rollout_abort"}
	if _, err := call(RPCRequest{Method: "submit", Request: &abort}); err != nil {
		t.Fatal(err)
	}
	await(abort.ID, "succeeded")
	if body, err := request(); err != nil || !strings.HasPrefix(body, "committed-") {
		t.Fatal("restart lost the original release", body, err)
	}
	stop()
	traffic.Wait()
	stopDaemon()
	run(Request{ID: "stop-generations", Action: "stop"}, "succeeded")
	if _, err := request(); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatal("stopped generation did not return unavailable", err)
	}
	if err := e.applyIngress(ctx, workload, routes, io.Discard); err != nil {
		t.Fatal("stopped traffic blocked ingress reconciliation", err)
	}
	run(Request{ID: "start-generations", Action: "start"}, "succeeded")
	if body, err := request(); err != nil || !strings.HasPrefix(body, "committed-") {
		t.Fatal("started generation did not refresh ingress", body, err)
	}
	var fallback map[string]any
	json.Unmarshal([]byte(compose("guarded", 0, true, true)), &fallback)
	fallback["services"].(map[string]any)["app"].(map[string]any)["ports"] = []string{"127.0.0.1:28888:8080"}
	fallbackCompose, _ := json.Marshal(fallback)
	fallbackOperation := run(Request{ID: "fixed-port", Action: "deploy", Compose: string(fallbackCompose), Rollout: options("rolling", 1)}, "succeeded")
	if fallbackOperation.Rollout == nil || fallbackOperation.Rollout.Strategy != "guarded" || !strings.Contains(fallbackOperation.Rollout.Reason, "ports") {
		t.Fatal("ineligible replacement did not report guarded fallback", fallbackOperation)
	}
	if body, err := request(); err != nil || !strings.HasPrefix(body, "guarded-") {
		t.Fatal("guarded fallback did not publish the replacement", body, err)
	}
	json.Unmarshal([]byte(compose("failed-guard", 0, true, false)), &fallback)
	fallback["services"].(map[string]any)["app"].(map[string]any)["ports"] = []string{"127.0.0.1:28888:8080"}
	fallbackCompose, _ = json.Marshal(fallback)
	failedGuard := options("rolling", 1)
	failedGuard.TimeoutSeconds = 5
	failedGuard.HTTPChecks = map[string]HTTPHealthCheck{"app": {Port: 8080, Path: "/not-ready"}}
	run(Request{ID: "failed-fixed-port", Action: "deploy", Compose: string(fallbackCompose), Rollout: failedGuard}, "failed")
	if body, err := request(); err != nil || !strings.HasPrefix(body, "guarded-") {
		t.Fatal("guarded HTTP health failure did not restore the release", body, err)
	}
	t.Logf("%d continuous requests, %d failures; replica and weighted-traffic checks passed", requests.Load(), failures.Load())
}
