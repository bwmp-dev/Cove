package worker

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCPUScalingDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_CPU_DOCKER") != "1" || runtime.GOOS != "linux" {
		t.Skip("isolated Linux Docker daemon required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	workload := fmt.Sprintf("cpu-%x", time.Now().UnixNano())
	command := func(args ...string) string {
		t.Helper()
		text, err := e.output(ctx, args...)
		if err != nil {
			t.Fatal(args, text, err)
		}
		return text
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		ids, _ := e.output(cleanup, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+workload)
		for _, id := range strings.Fields(ids) {
			e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "-v", id}, nil, io.Discard)
		}
		e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "cove-ingress"}, nil, io.Discard)
	})
	routes := []Route{{Domain: "cpu.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "default"}}}
	options := &RolloutOptions{Strategy: "rolling", Replicas: map[string]int{"app": 1}, TimeoutSeconds: 20, DrainSeconds: 1}
	compose := `services:
  app:
    image: busybox:1.37
    pull_policy: never
    environment:
      MARKER: immutable-original
    command: [sh, -c, 'mkdir -p /www; echo "$$MARKER-$$(cat /etc/hostname)" > /www/index.html; (while test ! -f /tmp/quiet; do :; done) & exec httpd -f -p 8080 -h /www']
    healthcheck:
      test: [CMD, wget, -q, -O, /dev/null, http://127.0.0.1:8080/]
      interval: 1s
      timeout: 1s
      retries: 3
`
	run := func(r Request, state string) Operation {
		t.Helper()
		r.Workload, r.Kind, r.Routes = workload, "service", routes
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
		if err != nil || op.State != state {
			t.Fatal(op, err, logs)
		}
		return op
	}
	run(Request{ID: "initial", Action: "deploy", Compose: compose, Rollout: options, Limits: map[string]ServiceLimits{"app": {CPUs: 0.5, MemoryMB: 32}}}, "succeeded")
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true}}
	request := func() error {
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://127.0.0.1/", nil)
		req.Host = "cpu.example.test"
		response, err := client.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		if err != nil || response.StatusCode != 200 || !strings.HasPrefix(string(body), "immutable-original-") {
			return fmt.Errorf("HTTP %d %s: %v", response.StatusCode, body, err)
		}
		return nil
	}
	if err := request(); err != nil {
		t.Fatal(err)
	}
	var total, failures atomic.Int64
	traffic, stop := context.WithCancel(ctx)
	var clients sync.WaitGroup
	clients.Go(func() {
		for traffic.Err() == nil {
			total.Add(1)
			if request() != nil {
				failures.Add(1)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	defer func() { stop(); clients.Wait() }()
	var sample *CPUScaleObservation
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		sample, err = e.CPUScaleSample(ctx, workload)
		if err == nil && sample.CPUPercent["app"] >= 50 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil || sample == nil || sample.CPUPercent["app"] < 50 {
		t.Fatal("live load did not produce CPU pressure", sample, err)
	}
	policy := AutoscalePolicy{Enabled: true, Services: map[string]AutoscaleService{"app": {MinReplicas: 1, MaxReplicas: 3, TargetCPUPercent: 50}}}
	decide := func(sample *CPUScaleObservation) map[string]int {
		now := time.Now().UTC()
		samples := []CPUScaleObservation{}
		for _, ago := range []time.Duration{2 * time.Minute, time.Minute, 0} {
			copy := *sample
			copy.SampledAt = now.Add(-ago)
			samples = append(samples, copy)
		}
		next, reason := CPUScaleDecision(policy, samples, now, nil)
		if reason != "scale" {
			t.Fatal(sample, reason)
		}
		return next
	}
	next := decide(sample)
	if next["app"] != 2 {
		t.Fatal(next)
	}
	up := run(Request{ID: "scale-up", Action: "deploy", Compose: "services: {not-the-release: {image: invalid.example.test/never}}", Rollout: options, Scale: &CPUScaleOptions{Generation: sample.Generation, Before: sample.Replicas, Replicas: next, CPUPercent: sample.CPUPercent}}, "succeeded")
	if up.Scale == nil || up.Rollout == nil || up.Rollout.Replicas["app"] != 2 {
		t.Fatal(up)
	}
	ids := strings.Fields(command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload))
	if len(ids) != 2 {
		t.Fatal(ids)
	}
	for _, id := range ids {
		command("docker", "exec", id, "touch", "/tmp/quiet")
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		sample, err = e.CPUScaleSample(ctx, workload)
		if err == nil && sample.CPUPercent["app"] < 25 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil || sample.CPUPercent["app"] >= 25 {
		t.Fatal("quiet CPU sample unavailable", sample, err)
	}
	if next = decide(sample); next["app"] != 1 {
		t.Fatal(next)
	}
	run(Request{ID: "stale", Action: "deploy", Compose: compose, Rollout: options, Scale: &CPUScaleOptions{Generation: "initial", Before: sample.Replicas, Replicas: next, CPUPercent: sample.CPUPercent}}, "failed")
	if got := strings.Fields(command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)); strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Fatal("stale request changed containers", got, ids)
	}
	originalRun := e.Run
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		err := originalRun(ctx, dir, args, env, out)
		if err == nil && strings.Contains(strings.Join(args, " "), "bad-health.generation.compose.json up ") {
			candidates, err := e.output(ctx, "docker", "ps", "-q", "--filter", "label=dev.cove.generation=bad-health")
			if err != nil {
				return err
			}
			for _, id := range strings.Fields(candidates) {
				if err := originalRun(ctx, dir, []string{"docker", "stop", "-t", "0", id}, nil, out); err != nil {
					return err
				}
			}
		}
		return err
	}
	run(Request{ID: "bad-health", Action: "deploy", Compose: compose, Rollout: options, Scale: &CPUScaleOptions{Generation: sample.Generation, Before: sample.Replicas, Replicas: next, CPUPercent: sample.CPUPercent}}, "failed")
	e.Run = originalRun
	if got := strings.Fields(command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)); strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Fatal("failed health did not preserve old replicas", got, ids)
	}
	run(Request{ID: "scale-down", Action: "deploy", Compose: compose, Rollout: options, Scale: &CPUScaleOptions{Generation: sample.Generation, Before: sample.Replicas, Replicas: next, CPUPercent: sample.CPUPercent}}, "succeeded")
	if got := strings.Fields(command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)); len(got) != 1 {
		t.Fatal(got)
	}
	stop()
	clients.Wait()
	if failures.Load() != 0 {
		t.Fatal("traffic failures during CPU scale", failures.Load(), total.Load())
	}
	t.Log("continuous HTTPS requests", total.Load(), "failures", failures.Load())
	current, _, err := readCurrent(filepath.Join(e.Root, "workloads", workload, "current.json"))
	if err != nil || current.Generation != "scale-down" {
		t.Fatal(current, err)
	}
}
