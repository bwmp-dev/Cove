package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPromotionKeepsTargetSettingsAndNeverBuilds(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	config := map[string]any{"services": map[string]any{"app": map[string]any{"image": "mutable:latest", "build": map[string]any{"context": "."}, "environment": map[string]any{"MODE": "target"}, "volumes": []any{"target:/data"}}}}
	if err := applyImagePins(config, map[string]string{"app": digest}); err != nil {
		t.Fatal(err)
	}
	service := config["services"].(map[string]any)["app"].(map[string]any)
	if service["build"] != nil || service["image"] != digest || service["pull_policy"] != "never" || service["environment"].(map[string]any)["MODE"] != "target" {
		t.Fatal(service)
	}
	if err := applyImagePins(config, map[string]string{"other": digest}); err == nil {
		t.Fatal("service mismatch accepted")
	}
	if err := validateImagePins(map[string]string{"app": "repo:latest"}); err == nil {
		t.Fatal("mutable image accepted")
	}
}

func TestPromotionRegistryRoundTrip(t *testing.T) {
	fixture := os.Getenv("COVE_DATABASE_FIXTURE")
	if fixture == "" {
		t.Skip("isolated Docker fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	e, err := NewEngine(filepath.Join(fixture, "promotion"))
	if err != nil {
		t.Fatal(err)
	}
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		var diagnostic bytes.Buffer
		err := RunCommand(ctx, dir, args, env, io.MultiWriter(out, &diagnostic))
		if err != nil {
			t.Log(diagnostic.String())
		}
		return err
	}
	name := "cove-registry-" + filepath.Base(fixture)
	if err = e.Run(ctx, e.Root, []string{"docker", "run", "-d", "--name", name, "--network", "host", "registry:2.8.3"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Run(context.Background(), e.Root, []string{"docker", "rm", "-f", "-v", name}, nil, io.Discard); err != nil {
			t.Error(err)
		}
	})
	port := "127.0.0.1:5000"
	ready := false
	for i := 0; i < 30; i++ {
		if e.Run(ctx, e.Root, []string{"docker", "run", "--rm", "--network", "container:" + name, "busybox:1.37", "wget", "-qO-", "http://127.0.0.1:5000/v2/"}, nil, io.Discard) == nil {
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	if !ready {
		t.Fatal("registry did not start")
	}
	digest, err := e.output(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", "busybox:1.37")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", "source", "release-states")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(dir, "release.compose.json")
	if err = writeJSON(compose, map[string]any{"services": map[string]any{"app": map[string]string{"image": digest}}}); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(dir, "release.json"), deployedState{ComposePath: compose}); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(e.path("release"), journal{Operation: Operation{Workload: "source", State: "succeeded"}}); err != nil {
		t.Fatal(err)
	}
	tagged := port + "/acceptance:source-release-app"
	t.Cleanup(func() {
		if err := e.Run(context.Background(), e.Root, []string{"docker", "image", "rm", tagged}, nil, io.Discard); err != nil {
			t.Error(err)
		}
	})
	images, err := e.ExportRelease(ctx, PromotionExport{Workload: "source", Release: "release", Portable: true, Repository: "acceptance", Registry: &Registry{Server: port, Username: "fixture", Password: "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	image := images["app"]
	if !strings.HasPrefix(image, port+"/acceptance@sha256:") {
		t.Fatal(image)
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "pull", image}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	expected, err := e.output(ctx, "docker", "image", "inspect", "--platform", "linux/amd64", "--format", "{{.Id}}", digest)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := e.output(ctx, "docker", "image", "inspect", "--platform", "linux/amd64", "--format", "{{.Id}}", image)
	if err != nil || actual != expected {
		t.Fatal("registry changed image", actual, digest, err)
	}
}

func TestExportReleaseOnlyReturnsImageIdentities(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	dir := filepath.Join(e.Root, "workloads", "source", "release-states")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(dir, "release.compose.json")
	if err = writeJSON(compose, map[string]any{"services": map[string]any{"app": map[string]any{"image": digest, "environment": map[string]string{"SECRET": "not-exported"}}}}); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(dir, "release.json"), deployedState{ComposePath: compose, Environment: map[string]string{"SECRET": "not-exported"}}); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(e.path("release"), journal{Operation: Operation{Workload: "source", State: "succeeded"}}); err != nil {
		t.Fatal(err)
	}
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		if strings.Join(args, " ") == "docker image inspect --format {{json .RepoDigests}} "+digest {
			fmt.Fprintf(out, `["registry.example/app@%s"]`, digest)
			return nil
		}
		t.Fatal("unexpected command", args)
		return nil
	}
	local, err := e.ExportRelease(context.Background(), PromotionExport{Workload: "source", Release: "release"})
	if err != nil || local["app"] != digest {
		t.Fatal(local, err)
	}
	portable, err := e.ExportRelease(context.Background(), PromotionExport{Workload: "source", Release: "release", Portable: true})
	if err != nil || portable["app"] != "registry.example/app@"+digest {
		t.Fatal(portable, err)
	}
	raw, _ := json.Marshal(portable)
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "not-exported") {
		t.Fatal("exported configuration")
	}
	e.mu.Lock()
	e.active["source"] = true
	e.mu.Unlock()
	if _, err = e.ExportRelease(context.Background(), PromotionExport{Workload: "source", Release: "release"}); err == nil {
		t.Fatal("export raced active deployment")
	}
}
