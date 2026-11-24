package worker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeDriftReportsChangesWithoutValues(t *testing.T) {
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	container := map[string]any{"Id": id, "Image": "sha256:" + strings.Repeat("b", 64), "Config": map[string]any{"Env": []string{"TOKEN=runtime-private-secret"}, "Labels": map[string]string{"com.docker.compose.project": "cove-app", "com.docker.compose.service": "web"}}, "HostConfig": map[string]any{"Memory": 0}, "Mounts": []any{}, "NetworkSettings": map[string]any{"Networks": map[string]any{}}}
	engine.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		if args[1] == "ps" {
			_, err := io.WriteString(out, id+"\n")
			return err
		}
		if args[1] != "inspect" {
			t.Fatal("drift issued a mutating command")
		}
		return json.NewEncoder(out).Encode([]any{container})
	}
	directory := filepath.Join(engine.Root, "workloads", "app")
	if err = os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	request := Request{ID: "baseline", Workload: "app"}
	if err = engine.recordDriftBaseline(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	report, err := engine.Drift(context.Background(), "app")
	if err != nil || !report.Baseline || len(report.Changes) != 0 {
		t.Fatal("unchanged runtime reported drift", err)
	}
	container["HostConfig"] = map[string]any{"Memory": 512 << 20}
	report, err = engine.Drift(context.Background(), "app")
	encoded, _ := json.Marshal(report)
	if err != nil || len(report.Changes) != 1 || len(report.Changes[0].Fields) != 1 || report.Changes[0].Fields[0] != "host" || strings.Contains(string(encoded), "runtime-private-secret") {
		t.Fatal("drift missed a manual change or exposed a value", err)
	}
	engine.active["app"] = true
	report, err = engine.Drift(context.Background(), "app")
	if err != nil || !report.Busy || report.Baseline {
		t.Fatal("active operation inspection was not deferred", err)
	}
}

func TestRuntimeDriftDockerAcceptance(t *testing.T) {
	if os.Getenv("COVE_DRIFT_DOCKER") != "1" {
		t.Skip("set COVE_DRIFT_DOCKER=1 for an isolated local Docker container")
	}
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workload := "drift-" + strings.ToLower(rand.Text())
	created, err := exec.Command("docker", "run", "--detach", "--network", "none", "--label", "cove.test=true", "--label", "com.docker.compose.project=cove-"+workload, "--label", "com.docker.compose.service=web", "--label", "com.docker.compose.oneoff=False", "postgres:17-alpine", "sleep", "300").CombinedOutput()
	if err != nil {
		t.Fatal("could not start isolated fixture", err, string(created))
	}
	container := strings.TrimSpace(string(created))
	if len(container) != 64 {
		t.Fatal("unexpected fixture identity")
	}
	t.Cleanup(func() {
		if output, err := exec.Command("docker", "rm", "--force", "--volumes", container).CombinedOutput(); err != nil {
			t.Error("could not remove owned fixture", err, string(output))
		}
	})
	if err = os.MkdirAll(filepath.Join(engine.Root, "workloads", workload), 0700); err != nil {
		t.Fatal(err)
	}
	if err = engine.recordDriftBaseline(context.Background(), Request{ID: "baseline", Workload: workload}); err != nil {
		t.Fatal(err)
	}
	report, err := engine.Drift(context.Background(), workload)
	if err != nil || len(report.Changes) != 0 {
		t.Fatal("new baseline differs from actual Docker", err, report)
	}
	if output, err := exec.Command("docker", "update", "--restart", "always", container).CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	report, err = engine.Drift(context.Background(), workload)
	if err != nil || len(report.Changes) != 1 || !strings.Contains(strings.Join(report.Changes[0].Fields, ","), "host") {
		t.Fatal("native Docker update was not detected", err, report)
	}
	if err = engine.recordDriftBaseline(context.Background(), Request{ID: "accepted-policy", Workload: workload}); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("docker", "stop", "--time", "1", container).CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	report, err = engine.Drift(context.Background(), workload)
	if err != nil || len(report.Changes) != 1 || !strings.Contains(strings.Join(report.Changes[0].Fields, ","), "state") {
		t.Fatal("manual container stop was not detected", err, report)
	}
	if err = engine.recordDriftBaseline(context.Background(), Request{ID: "intentional-stop", Workload: workload, Action: "stop"}); err != nil {
		t.Fatal(err)
	}
	report, err = engine.Drift(context.Background(), workload)
	if err != nil || len(report.Changes) != 0 {
		t.Fatal("intentional stop baseline reported drift", err, report)
	}

}

func TestRuntimeDriftEmptyPreviewRequiresEverySavedServiceDisabled(t *testing.T) {
	for _, tc := range []struct {
		name      string
		preview   *EnvironmentPreview
		services  map[string]any
		wantError bool
	}{
		{"disabled preview", &EnvironmentPreview{Disabled: []string{"worker"}}, map[string]any{"worker": map[string]any{"scale": 0}}, false},
		{"ordinary resource", nil, map[string]any{"worker": map[string]any{"scale": 0}}, true},
		{"enabled preview", &EnvironmentPreview{}, map[string]any{"worker": map[string]any{"scale": 0}}, true},
		{"missing enabled service", &EnvironmentPreview{Disabled: []string{"worker"}}, map[string]any{"worker": map[string]any{"scale": 0}, "web": map[string]any{"scale": 1}}, true},
		{"disabled name with running scale", &EnvironmentPreview{Disabled: []string{"worker"}}, map[string]any{"worker": map[string]any{"scale": 1}}, true},
		{"disabled name without saved scale", &EnvironmentPreview{Disabled: []string{"worker"}}, map[string]any{"worker": map[string]any{}}, true},
		{"empty saved services", &EnvironmentPreview{Disabled: []string{"worker"}}, map[string]any{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			engine.Run = func(_ context.Context, _ string, args, _ []string, _ io.Writer) error {
				if len(args) < 2 || args[0] != "docker" || args[1] != "ps" {
					t.Fatalf("unexpected runtime command: %v", args)
				}
				return nil
			}
			dir := filepath.Join(engine.Root, "workloads", "app")
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			compose := filepath.Join(dir, "saved.compose.json")
			if err = writeJSON(compose, map[string]any{"services": tc.services}); err != nil {
				t.Fatal(err)
			}
			if err = writeJSON(filepath.Join(dir, "current.json"), deployedState{ComposePath: compose}); err != nil {
				t.Fatal(err)
			}
			request := Request{ID: "preview-baseline", Workload: "app", Action: "deploy", EnvironmentPreview: tc.preview}
			err = engine.recordDriftBaseline(context.Background(), request)
			if (err != nil) != tc.wantError {
				t.Fatalf("baseline error = %v, want error %v", err, tc.wantError)
			}
			if !tc.wantError {
				report, err := engine.Drift(context.Background(), "app")
				if err != nil || !report.Baseline || len(report.Changes) != 0 {
					t.Fatalf("disabled preview baseline: %+v, %v", report, err)
				}
			}
		})
	}
}
