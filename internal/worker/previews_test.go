package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPreviewRejectsExternalFilesBeforeBuiltinResolution(t *testing.T) {
	for _, compose := range []string{
		"include: /must-not-read.yaml\nservices: {}\n",
		"services:\n  app:\n    image: busybox\n    env_file: /must-not-read.env\n",
	} {
		engine, err := NewEngine(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		request := Request{ID: "unsafe-preview", Workload: "preview", Action: "deploy", Preview: true, Compose: compose, BuiltinDefaults: map[string]string{CoveProject: "project"}}
		if _, err = engine.Submit(request); err != nil {
			t.Fatal(err)
		}
		calls := 0
		engine.Run = func(context.Context, string, []string, []string, io.Writer) error {
			calls++
			return nil
		}
		var jobs sync.WaitGroup
		if err = engine.dispatch(context.Background(), &jobs); err != nil {
			t.Fatal(err)
		}
		jobs.Wait()
		operation, err := engine.Status(request.ID)
		if err != nil || operation.State != "failed" || calls != 0 {
			t.Fatalf("preview resolved before isolation: state=%s calls=%d err=%v", operation.State, calls, err)
		}
	}
}

func TestPreviewComposeIsolation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "compose.yaml")
	for _, bad := range []string{
		"include: /etc/passwd\nservices: {}",
		"services:\n  app:\n    image: busybox\n    env_file: /etc/passwd",
		"services:\n  app:\n    image: busybox\n    environment: [HOME]",
		"services:\n  app:\n    image: busybox\n    environment: {HOME: null}",
		"services:\n  app:\n    image: busybox\n    privileged: true",
		"services:\n  app:\n    image: busybox\n    ports: ['80:80']",
		"services:\n  app:\n    image: busybox\n    volumes: ['/var/run/docker.sock:/socket']",
		"services:\n  app:\n    image: busybox\nvolumes:\n  db: {external: true}",
		"services:\n  app:\n    image: busybox\nnetworks:\n  db: {name: production}",
		"services:\n  app:\n    build: {context: /tmp}",
		"services:\n  app:\n    build: {context: ., ssh: default}",
		"services:\n  app:\n    image: busybox\n    command: '${PRODUCTION_SECRET}'",
	} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := previewCompose(path, root, "preview", map[string]string{}); err == nil {
			t.Fatal("unsafe preview accepted", bad)
		}
	}
	safe := "services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'echo $$TOKEN; sleep 300']\n    environment: {TOKEN: '${TOKEN}'}\n    volumes: [data:/data]\nvolumes:\n  data: {}\n"
	if err := os.WriteFile(path, []byte(safe), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := previewCompose(path, root, "preview", map[string]string{"TOKEN": "preview-only"})
	if err != nil {
		t.Fatal(err)
	}
	service := config["services"].(map[string]any)["app"].(map[string]any)
	if service["cpus"] != 1 || service["mem_limit"] != "512m" {
		t.Fatal("limits missing", service)
	}
	raw, _ := json.Marshal(config)
	if !strings.Contains(string(raw), "cove-preview-preview-data") || !strings.Contains(string(raw), `"internal":true`) || !strings.Contains(string(raw), "preview-only") {
		t.Fatal(string(raw))
	}
}

func TestPreviewIsolationRoundTrip(t *testing.T) {
	fixture := os.Getenv("COVE_DATABASE_FIXTURE")
	if fixture == "" {
		t.Skip("isolated Docker fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	e, err := NewEngine(filepath.Join(fixture, "previews"))
	if err != nil {
		t.Fatal(err)
	}
	id := "preview-" + filepath.Base(fixture)
	dir := filepath.Join(e.Root, "workloads", id)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "compose.yaml")
	if err = os.WriteFile(path, []byte("services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'echo preview > /data/value; sleep 300']\n    environment: {TOKEN: '${TOKEN}'}\n    volumes: [data:/data]\nvolumes:\n  data: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "preview-test", Workload: id, Preview: true, Environment: map[string]string{"TOKEN": "preview-only"}}
	resolved, err := e.preparePreview(r, dir, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.removePreview(context.Background(), r, io.Discard); err != nil {
			t.Error(err)
		}
	})
	var logs bytes.Buffer
	if err = e.Run(ctx, dir, []string{"docker", "compose", "-p", "cove-" + id, "-f", resolved, "up", "-d", "--wait"}, nil, &logs); err != nil {
		t.Fatal(err, logs.String())
	}
	containers, err := e.Containers(ctx, id)
	if err != nil || len(containers) != 1 {
		t.Fatal(containers, err)
	}
	inspect, err := e.output(ctx, "docker", "inspect", "--format", `{{.HostConfig.Memory}} {{.HostConfig.NanoCpus}} {{json .Config.Env}} {{json .HostConfig.PortBindings}}`, containers[0].ID)
	if err != nil || !strings.Contains(inspect, "536870912 1000000000") || !strings.Contains(inspect, "TOKEN=preview-only") || strings.Contains(inspect, "HostPort") {
		t.Fatal(inspect, err)
	}
	network, err := e.output(ctx, "docker", "network", "inspect", "--format", "{{.Internal}}", "cove-"+id+"_default")
	if err != nil || network != "true" {
		t.Fatal(network, err)
	}
	if err = e.removePreview(ctx, r, &logs); err != nil {
		t.Fatal(err, logs.String())
	}
	if err = e.removePreview(ctx, r, &logs); err != nil {
		t.Fatal("cleanup not idempotent", err)
	}
	remaining, err := e.output(ctx, "docker", "volume", "ls", "-q", "--filter", "label=dev.cove.preview="+id)
	if err != nil || remaining != "" {
		t.Fatal("preview storage retained", remaining, err)
	}
}
