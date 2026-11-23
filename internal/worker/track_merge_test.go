package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMergedComposeDollarSnapshotRoundTrip(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose executable required")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(source, []byte("services:\n  app:\n    image: busybox:1.37\n    environment:\n      VALUE: ${VALUE:?required}\n    labels:\n      literal$$key: value$$literal\n    command: ['printf', '$$SHELL']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resolve := func(file string) map[string]any {
		t.Helper()
		out := &limitedBuffer{Limit: 1 << 20}
		if err := RunCommand(context.Background(), dir, []string{"docker", "compose", "--file", file, "config", "--format", "json"}, []string{"VALUE=resolved-$VALUE-$$PAIR"}, out); err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := unmarshalCompose(out.data.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	resolved := resolve(source)
	app := resolved["services"].(map[string]any)["app"].(map[string]any)
	if app["environment"].(map[string]any)["VALUE"] != "resolved-$VALUE-$$PAIR" || app["command"].([]any)[1] != "$SHELL" {
		t.Fatal("initial Compose resolution changed dollars", app)
	}
	snapshot := filepath.Join(dir, "snapshot.json")
	if err := writeJSON(snapshot, literalCompose(resolved)); err != nil {
		t.Fatal(err)
	}
	reread := resolve(snapshot)
	left, _ := json.Marshal(resolve(source)["services"])
	right, _ := json.Marshal(reread["services"])
	if string(left) != string(right) {
		t.Fatalf("resolved snapshot changed dollars: before %s after %s", left, right)
	}
	generated := resolve(source)
	request := Request{ID: "dollar-generation", Workload: "app", Rollout: &RolloutOptions{Strategy: "rolling", Replicas: map[string]int{"app": 2}}}
	traffic, _, err := generationConfig(request, generated)
	if err != nil {
		t.Fatal(err)
	}
	generation := filepath.Join(dir, "generation.json")
	if err = writeJSON(generation, literalCompose(generated)); err != nil {
		t.Fatal(err)
	}
	actual := resolve(generation)
	service := actual["services"].(map[string]any)[generationName(request.ID, "app")].(map[string]any)
	if service["environment"].(map[string]any)["VALUE"] != "resolved-$VALUE-$$PAIR" || service["command"].([]any)[1] != "$SHELL" || service["labels"].(map[string]any)["literal$$key"] != "value$literal" {
		t.Fatal("rollout generation changed literal dollars", service)
	}
	if err = restoreServiceNames(actual, traffic); err != nil {
		t.Fatal(err)
	}
	if actual["services"].(map[string]any)["app"] == nil {
		t.Fatal("retained generation could not restore its logical service")
	}
}
