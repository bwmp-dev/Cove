package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeBundleValidation(t *testing.T) {
	for _, bundle := range []*ComposeBundle{
		{Files: []ComposeFile{{Path: "../outside", Content: "x"}}},
		{Files: []ComposeFile{{Path: "compose.yaml", Content: "x"}}},
		{Assets: map[string]string{".git/config": "x"}},
		{Assets: map[string]string{"a\\b": "x"}},
		{Files: []ComposeFile{{Path: "override.yaml"}}, Assets: map[string]string{"OVERRIDE.yaml": "x"}},
		{Assets: map[string]string{"large.env": strings.Repeat("x", (512<<10)+1)}},
		{Profiles: []string{"*"}},
		{Profiles: []string{"debug", "debug"}},
		{Paths: []string{"override.yaml"}},
	} {
		if ValidateComposeBundle(bundle, nil, false) == nil {
			t.Fatal("invalid bundle accepted")
		}
	}
	bundle := &ComposeBundle{Paths: []string{"production.yaml"}, Profiles: []string{"worker"}}
	source := &GitSource{ComposePath: "compose.yaml"}
	if err := ValidateComposeBundle(bundle, source, false); err != nil {
		t.Fatal(err)
	}
	if ValidateComposeBundle(bundle, source, true) == nil {
		t.Fatal("restricted preview bundle accepted")
	}
}

func TestComposeBundleNativeOrderedOverridesAndProfiles(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose required")
	}
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(engine.Root, "workloads", "app")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	request := Request{ID: "bundle", Workload: "app", Action: "deploy", Compose: "include: [included.yaml]\nservices:\n  web:\n    image: busybox\n    env_file: [base.env,production.env]\n    environment:\n      REMOVE: old\n      LITERAL: '$$KEEP'\n  debug:\n    profiles: [debug]\n    image: busybox\n", ComposeBundle: &ComposeBundle{
		Files:  []ComposeFile{{Path: "production.yaml", Content: "services:\n  web:\n    image: busybox:1.37\n    environment:\n      REMOVE: !reset null\n      MODE: production\n"}},
		Assets: map[string]string{"base.env": "ORDER=first\n", "production.env": "ORDER=last\n", "included.yaml": "services:\n  included:\n    image: busybox\n"}, Profiles: []string{"debug"},
	}}
	resolved, err := engine.prepareComposeBundle(context.Background(), request, dir, "unused", nil)
	if err != nil {
		t.Fatal(err)
	}
	output := &limitedBuffer{Limit: 2 << 20}
	if err = engine.Run(context.Background(), dir, []string{"docker", "compose", "--file", resolved, "config", "--format", "json"}, nil, output); err != nil {
		t.Fatal(err, output.data.String())
	}
	var config struct {
		Services map[string]struct {
			Image       string            `json:"image"`
			Environment map[string]string `json:"environment"`
			Profiles    []string          `json:"profiles"`
		} `json:"services"`
	}
	if json.Unmarshal(output.data.Bytes(), &config) != nil {
		t.Fatal("invalid config")
	}
	web := config.Services["web"]
	if len(config.Services) != 3 || web.Image != "busybox:1.37" || web.Environment["ORDER"] != "last" || web.Environment["MODE"] != "production" || web.Environment["LITERAL"] != "$$KEEP" || len(config.Services["debug"].Profiles) != 0 {
		t.Fatal("Compose bundle changed selection or values", config)
	}
	if _, ok := web.Environment["REMOVE"]; ok {
		t.Fatal("reset variable retained")
	}
}

func TestComposeBundleRejectsRepositorySymlinkEscape(t *testing.T) {
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(engine.Root, "workloads", "app")
	root := filepath.Join(dir, "releases", "bundle")
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err = os.WriteFile(outside, []byte("services: {}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(root, "override.yaml")); err != nil {
		t.Skip("symlinks unavailable", err)
	}
	request := Request{ID: "bundle", Workload: "app", Source: &GitSource{ComposePath: "compose.yaml"}, ComposeBundle: &ComposeBundle{Paths: []string{"override.yaml"}}}
	if _, err = engine.prepareComposeBundle(context.Background(), request, dir, filepath.Join(root, "compose.yaml"), nil); err == nil || !strings.Contains(err.Error(), "escapes repository") {
		t.Fatal("symlink escape accepted", err)
	}
}
