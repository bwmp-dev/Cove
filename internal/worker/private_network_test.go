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

func TestPrivateNetworkConfiguration(t *testing.T) {
	p := &PrivateNetwork{Service: "app", Hostname: "private-app", Port: 3000, AuthKey: "tskey-auth-fixture"}
	if err := ValidatePrivateNetwork(p); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"services": map[string]any{"app": map[string]any{"image": "busybox", "networks": map[string]any{"default": nil}}}}
	if err := addPrivateNetwork(config, p, "/fixture/serve.json"); err != nil {
		t.Fatal(err)
	}
	services := config["services"].(map[string]any)
	app := services["app"].(map[string]any)
	sidecar := services["cove-tailscale"].(map[string]any)
	if app["network_mode"] != "service:cove-tailscale" || app["networks"] != nil {
		t.Fatal("app does not share the private network namespace")
	}
	raw, _ := json.Marshal(sidecar)
	if strings.Contains(string(raw), "\"ports\"") || strings.Contains(string(raw), "\"privileged\"") {
		t.Fatal("sidecar publishes ports or is privileged")
	}
	if sidecar["environment"].(map[string]string)["TS_USERSPACE"] != "false" {
		t.Fatal("cross-node traffic is not routed")
	}
	if app["depends_on"].(map[string]any)["cove-tailscale"] == nil {
		t.Fatal("missing sidecar ordering")
	}
}

func TestPrivateNetworkRejectsPublicExposure(t *testing.T) {
	for _, service := range []map[string]any{
		{"ports": []any{map[string]any{"published": "3000"}}},
		{"network_mode": "host"},
		{"hostname": "custom"},
		{"dns": []any{"1.1.1.1"}},
		{"labels": map[string]any{"traefik.enable": "true"}},
	} {
		service["networks"] = map[string]any{"default": nil}
		c := map[string]any{"services": map[string]any{"app": service}}
		if addPrivateNetwork(c, &PrivateNetwork{Service: "app"}, "/fixture/config") == nil {
			t.Fatal("accepted public exposure")
		}
	}
}

func TestPrivateNetworkCompose(t *testing.T) {
	root := os.Getenv("COVE_DATABASE_FIXTURE")
	if root == "" {
		t.Skip("isolated Docker fixture required")
	}
	e := &Engine{Root: root, Run: RunCommand}
	compose := filepath.Join(root, "private-base.yaml")
	if err := os.WriteFile(compose, []byte("services:\n  app:\n    image: busybox:1.37\n    command: [sleep, '300']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := e.output(context.Background(), "docker", "compose", "-p", "cove-private-fixture", "-f", compose, "config", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err = json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	if err = addPrivateNetwork(config, &PrivateNetwork{Service: "app", Hostname: "private-fixture", Port: 3000, AuthKey: "tskey-auth-fixture"}, filepath.Join(root, "serve.json")); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(root, "private-generated.json")
	if err = writeJSON(generated, config); err != nil {
		t.Fatal(err)
	}
	if err = e.Run(context.Background(), root, []string{"docker", "compose", "-p", "cove-private-fixture", "-f", generated, "config", "--quiet"}, nil, io.Discard); err != nil {
		t.Fatal("generated private compose rejected", err)
	}
}
