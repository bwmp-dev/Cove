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
	if err := os.WriteFile(source, []byte("services:\n  app:\n    image: busybox:1.37\n    environment:\n      VALUE: ${VALUE:?required}\n    command: ['printf', '$$SHELL']\n"), 0600); err != nil {
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
}
