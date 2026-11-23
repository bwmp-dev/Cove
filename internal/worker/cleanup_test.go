package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildUsesOwnedBuilderAndLabels(t *testing.T) {
	e, _ := NewEngine(t.TempDir())
	var commands []string
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		command := strings.Join(args, " ")
		commands = append(commands, command)
		if strings.Contains(command, "config --format json") {
			_, err := io.WriteString(out, `{"services":{"app":{"build":{"context":"."}},"db":{"image":"postgres"}}}`)
			return err
		}
		if strings.Contains(command, " build --builder ") {
			data, err := os.ReadFile(filepath.Join(e.Root, "build-cleanup.compose.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "dev.cove.build-owner") || strings.Contains(string(data), `"db"`) {
				t.Fatal(string(data))
			}
			if !strings.Contains(strings.Join(env, " "), "DOCKER_CONFIG=registry") {
				t.Fatal("lost registry authentication")
			}
		}
		return nil
	}
	for range 2 {
		if err := e.buildImages(context.Background(), e.Root, []string{"docker", "compose", "-f", "compose.yaml"}, []string{"DOCKER_CONFIG=registry"}, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	creates := 0
	for _, cmd := range commands {
		if strings.Contains(cmd, "buildx create") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatal(commands)
	}
}

func TestCleanupRealDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_CLEANUP_DOCKER") != "1" {
		t.Skip("set COVE_TEST_CLEANUP_DOCKER=1 for isolated Docker build acceptance")
	}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := e.buildOwner()
	if err != nil {
		t.Fatal(err)
	}
	project := "cove-cleanup-" + owner
	ctx := context.Background()
	env := []string{"BUILDX_CONFIG=" + filepath.Join(e.Root, "buildx")}
	t.Cleanup(func() {
		if err := e.Run(ctx, e.Root, []string{"docker", "image", "rm", project + "-app"}, nil, io.Discard); err != nil {
			t.Error(err)
		}
		if err := e.Run(ctx, e.Root, []string{"docker", "buildx", "rm", "--force", "cove-" + owner}, env, io.Discard); err != nil {
			t.Error(err)
		}
	})
	for path, data := range map[string]string{"Dockerfile": "FROM scratch\nCOPY payload /payload\n", "payload": "cleanup acceptance", "compose.yaml": "services:\n  app:\n    build:\n      context: .\n      args:\n        SOURCE_DATE_EPOCH: '1'\n"} {
		if err := os.WriteFile(filepath.Join(e.Root, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"docker", "compose", "-p", project, "-f", filepath.Join(e.Root, "compose.yaml")}
	if err := e.buildImages(ctx, e.Root, args, nil, os.Stderr); err != nil {
		t.Fatal(err)
	}
	label, err := e.output(ctx, "docker", "image", "inspect", "--format", `{{index .Config.Labels "dev.cove.build-owner"}}`, project+"-app")
	if err != nil || strings.TrimSpace(label) != owner {
		t.Fatalf("missing ownership: %s %v", label, err)
	}
	first, err := e.output(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", project+"-app")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Root, "payload"), []byte("second build"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.buildImages(ctx, e.Root, args, nil, os.Stderr); err != nil {
		t.Fatal(err)
	}
	// Override only the disposable image's reported import age; Docker records import time rather than SOURCE_DATE_EPOCH.
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		if len(args) > 4 && args[1] == "image" && args[2] == "inspect" && args[len(args)-1] == strings.TrimSpace(first) && strings.Contains(strings.Join(args, " "), `"created"`) {
			var capture bytes.Buffer
			if err := RunCommand(ctx, dir, args, env, &capture); err != nil {
				return err
			}
			var metadata map[string]any
			if err := json.Unmarshal(capture.Bytes(), &metadata); err != nil {
				return err
			}
			metadata["created"] = time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano)
			metadata["tagged"] = metadata["created"]
			return json.NewEncoder(out).Encode(metadata)
		}

		return RunCommand(ctx, dir, args, env, out)
	}
	_, plan, err := e.CleanupPolicy(ctx, CleanupControl{Action: "preview"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = e.CleanupPolicy(ctx, CleanupControl{Action: "run", Revision: plan.Revision}); err != nil {
		t.Fatal(err)
	}

	if _, err := e.output(ctx, "docker", "image", "inspect", strings.TrimSpace(first)); err == nil {
		t.Fatal("expired dangling image retained")
	}
	if _, err := e.output(ctx, "docker", "image", "inspect", project+"-app"); err != nil {
		t.Fatal("tagged image removed", err)
	}
}
