package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func rolloutFixtureConfig(t *testing.T) map[string]any {
	t.Helper()
	var config map[string]any
	if err := json.Unmarshal([]byte(`{"services":{"app":{"image":"busybox:1.37","healthcheck":{"test":["CMD","true"]},"networks":{"default":null},"environment":{"VALUE":"literal$$value"}}},"networks":{"default":{"name":"cove-app_default","ipam":{}}}}`), &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestRolloutEligibilityAndGenerationIsolation(t *testing.T) {
	r := Request{ID: "new", Workload: "app", Action: "deploy", Kind: "service", Routes: []Route{{Domain: "app.example.test", Service: "app", Port: 8080}}, Rollout: &RolloutOptions{Strategy: "rolling", TimeoutSeconds: 120, CanaryPercent: 10, Replicas: map[string]int{"app": 3}}}
	if err := validateRollout(r); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"ports", "container_name", "network_mode", "privileged", "post_start", "devices"} {
		config := rolloutFixtureConfig(t)
		config["services"].(map[string]any)["app"].(map[string]any)[field] = "unsafe"
		if reason := rolloutEligibility(r, config); !strings.Contains(reason, field) {
			t.Fatal(field, reason)
		}
	}
	config := rolloutFixtureConfig(t)
	app := config["services"].(map[string]any)["app"].(map[string]any)
	app["volumes"] = []any{map[string]any{"source": "data", "target": "/data", "type": "volume"}}
	if reason := rolloutEligibility(r, config); !strings.Contains(reason, "writable storage") {
		t.Fatal(reason)
	}
	r.Rollout.ParallelVolumes = true
	if reason := rolloutEligibility(r, config); reason != "" {
		t.Fatal(reason)
	}
	delete(app, "healthcheck")
	r.Rollout.HTTPChecks = map[string]HTTPHealthCheck{"app": {Port: 8080, Path: "/ready"}}
	if reason := rolloutEligibility(r, config); reason != "" {
		t.Fatal(reason)
	}
	traffic, networks, err := generationConfig(r, config)
	if err != nil {
		t.Fatal(err)
	}
	key := generationName(r.ID, "app")
	services := config["services"].(map[string]any)
	if len(services) != 1 || services["app"] != nil || services[key] == nil || len(networks) != 1 || traffic["app"][0].Service != key {
		t.Fatal(config, traffic, networks)
	}
	generated := services[key].(map[string]any)
	if generated["scale"] != 3 || generated["labels"].(map[string]any)["dev.cove.service"] != "app" {
		t.Fatal(generated)
	}
	for network := range generated["networks"].(map[string]any) {
		if network == "default" || config["networks"].(map[string]any)[network] == nil {
			t.Fatal("generation reused old network key", network)
		}
	}
	if err = restoreServiceNames(config, traffic); err != nil || config["services"].(map[string]any)["app"] == nil {
		t.Fatal(config, err)
	}
}

func TestResolvedComposeDollarsRoundTrip(t *testing.T) {
	raw := []byte(`{"services":{"app":{"command":["sh","-c","echo $$VALUE; echo $$(hostname)"],"environment":{"VALUE":"literal$$$$pair"},"labels":{"literal$$key":"value$$literal"}}}}`)
	var config map[string]any
	if err := decodeResolvedCompose(raw, &config); err != nil {
		t.Fatal(err)
	}
	service := config["services"].(map[string]any)["app"].(map[string]any)
	if service["environment"].(map[string]any)["VALUE"] != "literal$$pair" {
		t.Fatal(service)
	}
	if _, ok := service["labels"].(map[string]any)["literal$$key"]; !ok {
		t.Fatal("label key changed")
	}
	encoded, err := json.Marshal(literalCompose(config))
	if err != nil {
		t.Fatal(err)
	}
	var original any
	json.Unmarshal(raw, &original)
	want, _ := json.Marshal(original)
	if string(encoded) != string(want) {
		t.Fatal(string(encoded), string(want))
	}
}

func TestHTTPHealthRejectsRedirectAndRemoteTargets(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect target was contacted") }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			w.WriteHeader(204)
			return
		}
		http.Redirect(w, r, target.URL, 302)
	}))
	defer server.Close()
	if err := HTTPHealth(context.Background(), server.URL+"/ready"); err != nil {
		t.Fatal(err)
	}
	if HTTPHealth(context.Background(), server.URL+"/redirect") == nil {
		t.Fatal("redirect accepted")
	}
	for _, address := range []string{"https://127.0.0.1:80/", "http://169.254.169.254:80/", "http://localhost:80/", "http://user@127.0.0.1:80/"} {
		if HTTPHealth(context.Background(), address) == nil {
			t.Fatal(address)
		}
	}
}

func TestInterruptedRolloutKeepsLockUntilRecovery(t *testing.T) {
	e, _ := NewEngine(t.TempDir())
	r := Request{ID: "interrupted", Workload: "app", Action: "deploy", Compose: "services: {}"}
	if _, err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", "app")
	os.MkdirAll(dir, 0700)
	j, _ := e.read(r.ID)
	j.Operation.State = "running"
	j.RolloutChange = &rolloutChange{Origin: r.ID, Phase: "preparing", PreviousTraffic: trafficState{}}
	writeJSON(e.path(r.ID), j)
	writeJSON(filepath.Join(dir, "rollout.json"), r.ID)
	if err := e.Recover(); err != nil {
		t.Fatal(err)
	}
	if !e.busy(r.Workload) {
		t.Fatal("workload lock released before recovery")
	}
	fail := true
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		if fail {
			return errors.New("Docker unavailable")
		}
		return nil
	}
	prior := routeRecoveryRetry
	routeRecoveryRetry = time.Millisecond
	defer func() { routeRecoveryRetry = prior }()
	var jobs sync.WaitGroup
	e.dispatch(context.Background(), &jobs)
	jobs.Wait()
	op, _ := e.Status(r.ID)
	if op.State != "running" || !e.busy(r.Workload) {
		t.Fatal(op)
	}
	fail = false
	time.Sleep(2 * time.Millisecond)
	e.dispatch(context.Background(), &jobs)
	jobs.Wait()
	op, _ = e.Status(r.ID)
	if op.State != "interrupted" || e.busy(r.Workload) {
		t.Fatal(op)
	}
	if _, err := os.Stat(filepath.Join(dir, "rollout.json")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestRolloutTrafficDecisionHonorsCancellationAndRecoversStaging(t *testing.T) {
	e, _ := NewEngine(t.TempDir())
	r := Request{ID: "staging", Workload: "app", Action: "deploy", Compose: "services: {}"}
	if _, err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	j, _ := e.read(r.ID)
	j.Operation.State = "running"
	j.CancelRequested = true
	writeJSON(e.path(r.ID), j)
	change := rolloutChange{Origin: r.ID, Phase: "waiting", Staged: true, Options: RolloutOptions{Strategy: "canary", CanaryPercent: 25}}
	status := RolloutStatus{Operation: r.ID, Release: r.ID, Strategy: "canary", Phase: "waiting", CandidatePercent: 25}
	if err := e.decideRollout(r.ID, change, status); !errors.Is(err, errRolloutCanceled) {
		t.Fatal(err)
	}
	j, _ = e.read(r.ID)
	if j.RolloutChange != nil || j.Operation.Rollout != nil {
		t.Fatal("cancelled traffic decision was persisted", j)
	}
	j.CancelRequested = false
	writeJSON(e.path(r.ID), j)
	if err := e.decideRollout(r.ID, change, status); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	os.MkdirAll(dir, 0700)
	writeJSON(filepath.Join(dir, "rollout.json"), r.ID)
	j, _ = e.read(r.ID)
	j.CancelRequested = true
	writeJSON(e.path(r.ID), j)
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		t.Error("committed staged traffic must survive worker restart without cleanup")
		return nil
	}
	if err := e.settleRollout(context.Background(), r.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	operation, _ := e.Status(r.ID)
	pending, err := e.rolloutStatus(r.Workload)
	if operation.State != "succeeded" || err != nil || pending == nil || pending.Phase != "waiting" || pending.CandidatePercent != 25 {
		t.Fatal(operation, pending, err)
	}
}
