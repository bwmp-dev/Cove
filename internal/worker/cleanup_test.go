package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupOwnershipAndFailure(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		commands = append(commands, strings.Join(args, " "))
		if args[1] == "buildx" && !strings.Contains(strings.Join(env, " "), "BUILDX_CONFIG="+filepath.Join(e.Root, "buildx")) {
			t.Fatal("shared builder configuration")
		}
		return nil
	}
	if err := e.cleanupBuilds(context.Background(), io.Discard); err != nil || len(commands) != 0 {
		t.Fatal("legacy cache touched", err)
	}
	owner, err := e.buildOwner()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(e.Root, "builder-ready.json"), true); err != nil {
		t.Fatal(err)
	}
	if err := e.cleanupBuilds(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[0] != "docker image prune --force --filter label=dev.cove.build-owner="+owner+" --filter until=168h" || commands[1] != "docker buildx prune --builder cove-"+owner+" --force --filter until=168h --keep-storage 2GB" {
		t.Fatal(commands)
	}
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		return errors.New("docker unavailable")
	}
	if err := e.cleanupBuilds(context.Background(), io.Discard); err == nil {
		t.Fatal("lost cleanup failure")
	}
}

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
		if err := e.buildImages(context.Background(), e.Root, []string{"docker", "compose", "-f", "compose.yaml"}, []string{"DOCKER_CONFIG=registry"}, io.Discard, false); err != nil {
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
	if err := e.buildImages(ctx, e.Root, args, nil, os.Stderr, false); err != nil {
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
	if err := e.buildImages(ctx, e.Root, args, nil, os.Stderr, false); err != nil {
		t.Fatal(err)
	}
	// Docker's containerd store ages imported images by import time, not SOURCE_DATE_EPOCH.
	// Shorten only this disposable owner's image grace period to exercise real deletion.
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		args = append([]string{}, args...)
		if len(args) > 2 && args[1] == "image" && args[2] == "prune" {
			for i, arg := range args {
				if arg == "until=168h" {
					args[i] = "until=0s"
				}
			}
		}
		return RunCommand(ctx, dir, args, env, out)
	}
	if err := e.cleanupBuilds(ctx, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if _, err := e.output(ctx, "docker", "image", "inspect", strings.TrimSpace(first)); err == nil {
		t.Fatal("expired dangling image retained")
	}
	if _, err := e.output(ctx, "docker", "image", "inspect", project+"-app"); err != nil {
		t.Fatal("tagged image removed", err)
	}
}
