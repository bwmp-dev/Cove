package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCPUScaleCannotFallBackToGuardedReplacement(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		t.Fatal("guarded replacement was attempted")
		return nil
	}
	if err := e.guardedRolloutFallback(context.Background(), Request{Scale: &CPUScaleOptions{}}, e.Root, nil, nil, io.Discard, "ineligible storage"); err == nil {
		t.Fatal("CPU scaling accepted guarded fallback")
	}
}

func TestCPUScalingRechecksLiveHealthBeforeStarting(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", "app")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "release.json")
	if err = os.WriteFile(path, []byte(`{"services":{"app":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	state := deployedState{Generation: "previous", ComposePath: path, Rollout: &RolloutOptions{Strategy: "rolling", Replicas: map[string]int{"app": 1}}, Traffic: trafficState{"app": {{Service: "generation", Weight: 100}}}}
	if err = writeJSON(filepath.Join(dir, "current.json"), state); err != nil {
		t.Fatal(err)
	}
	health := "unhealthy"
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		switch args[1] {
		case "ps":
			return json.NewEncoder(out).Encode(map[string]string{"ID": strings.Repeat("1", 64), "Names": "app", "State": "running"})
		case "inspect":
			return json.NewEncoder(out).Encode(map[string]any{"id": strings.Repeat("1", 64), "service": "app", "state": map[string]any{"Status": "running", "Health": map[string]string{"Status": health}}})
		default:
			t.Fatal("unexpected mutation", args)
			return nil
		}
	}
	r := Request{Workload: "app", Action: "deploy", Scale: &CPUScaleOptions{Generation: "previous", Before: map[string]int{"app": 1}, Replicas: map[string]int{"app": 2}}}
	if _, err = e.prepareCPUScale(context.Background(), r); err == nil {
		t.Fatal("queued scale ignored new unhealthy state")
	}
	health = "healthy"
	next, err := e.prepareCPUScale(context.Background(), r)
	if err != nil || next.Action != "rollback" || next.Release != "previous" {
		t.Fatal(next, err)
	}
}

func TestCPUScaleUsesDedicatedRPCAndValidatesObservations(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "scale", Workload: "app", Kind: "service", Action: "deploy", Compose: "services: {}", Rollout: &RolloutOptions{Strategy: "rolling"}, Scale: &CPUScaleOptions{Generation: "previous", Before: map[string]int{"app": 1}, Replicas: map[string]int{"app": 2}, CPUPercent: map[string]float64{"app": 80}}}
	if err := ValidateRequest(r); err != nil {
		t.Fatal(err)
	}
	if result := e.RPC(context.Background(), RPCRequest{Method: "submit", Request: &r}); result.Error == "" {
		t.Fatal("ordinary RPC accepted CPU scale", result)
	}
	if result := e.RPC(context.Background(), RPCRequest{Method: "submit_cpu_scale", Request: &r}); result.Error != "" || result.Operation == nil || result.Operation.Scale == nil {
		t.Fatal(result)
	}
	r.Scale.Before = nil
	if err := ValidateRequest(r); err == nil {
		t.Fatal("missing source counts accepted")
	}
	r.Scale.Before = map[string]int{"app": 1}
	r.Scale.Replicas = nil
	if err := ValidateRequest(r); err == nil {
		t.Fatal("empty scale accepted")
	}
}

func TestCPUScalingSnapshotsOmitOnlyEmptyComposePlacement(t *testing.T) {
	value := map[string]any{"services": map[string]any{"app": map[string]any{"deploy": map[string]any{"placement": map[string]any{}, "resources": map[string]any{"limits": map[string]any{"cpus": 0.5}}}}, "other": map[string]any{"deploy": map[string]any{"placement": map[string]any{"constraints": []any{"node.role==manager"}}}}}}
	config := literalCompose(value).(map[string]any)
	services := config["services"].(map[string]any)
	if _, ok := services["app"].(map[string]any)["deploy"].(map[string]any)["placement"]; ok {
		t.Fatal("empty placement retained")
	}
	if services["other"].(map[string]any)["deploy"].(map[string]any)["placement"] == nil {
		t.Fatal("nonempty placement lost")
	}
}
