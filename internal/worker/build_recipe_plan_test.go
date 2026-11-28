package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const recipePlanFixture = `{"steps":[{"name":"packages","inputs":[{"image":"ghcr.io/railwayapp/railpack-builder:mise-2026.11.12"}]},{"name":"build","inputs":[{"step":"packages"},{"local":true,"include":["."]}],"commands":[{"cmd":"sh -c 'printf built > /app/artifact'"}],"secrets":["*"]}],"deploy":{"base":{"image":"ghcr.io/railwayapp/railpack-runtime:mise-2026.11.12"},"inputs":[{"step":"build","include":["."]}],"startCommand":"sh start.sh"}}`

func TestBuildRecipePlanPinsAndSecrets(t *testing.T) {
	data, err := validateRecipePlan([]byte(recipePlanFixture), []string{"SELECTED"})
	if err != nil {
		t.Fatal(err)
	}
	var plan recipePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Steps[0].Inputs[0].Image != RailpackBuilderImage || plan.Deploy.Base.Image != RailpackRuntimeImage || strings.Join(plan.Steps[1].Secrets, ",") != "SELECTED" || strings.Join(plan.Secrets, ",") != "SELECTED" {
		t.Fatalf("pins or secret selection lost: %s", data)
	}
	for name, replace := range map[string][2]string{
		"image override":    {"ghcr.io/railwayapp/railpack-runtime:mise-2026.11.12", "example.invalid/escape:latest"},
		"unselected secret": {"[\"*\"]", "[\"OTHER\"]"},
		"host mount":        {"\"cmd\":\"sh -c 'printf built > /app/artifact'\"", "\"cmd\":\"true\",\"mounts\":[\"/var/run/docker.sock\"]"},
		"unknown input":     {"\"local\":true", "\"local\":true,\"host\":\"/\""},
		"cycle":             {"\"step\":\"packages\"", "\"step\":\"build\""},
		"unknown step":      {"\"step\":\"packages\"", "\"step\":\"missing\""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateRecipePlan([]byte(strings.Replace(recipePlanFixture, replace[0], replace[1], 1)), []string{"SELECTED"}); err == nil {
				t.Fatal("unsafe plan accepted")
			}
		})
	}
	if _, err := validateRecipePlan([]byte(recipePlanFixture+` {}`), nil); err == nil {
		t.Fatal("trailing output accepted")
	}
	if _, err := validateRecipePlan(make([]byte, 2<<20+1), nil); err == nil {
		t.Fatal("oversized output accepted")
	}
}

func TestBuildRecipeSecretAndNoCacheExecIdentityChanges(t *testing.T) {
	validated, err := validateRecipePlan([]byte(recipePlanFixture), []string{"SELECTED"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := invalidateRecipeCommands(validated, "aaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	second, err := invalidateRecipeCommands(validated, "bbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	var a, b recipePlan
	if json.Unmarshal(first, &a) != nil || json.Unmarshal(second, &b) != nil {
		t.Fatal("invalid rewritten plan")
	}
	if string(a.Steps[1].Commands[0]) == string(b.Steps[1].Commands[0]) || a.Deploy.Variables["COVE_BUILD_CACHE_BUSTER"] != "" {
		t.Fatal("exec cache was reused or runtime environment changed")
	}
	if a.Deploy.Base.Image != RailpackRuntimeImage || a.Steps[0].Inputs[0].Image != RailpackBuilderImage || strings.Join(a.Secrets, ",") != "SELECTED" {
		t.Fatal("cache invalidation changed pins or secrets")
	}
}

func TestBuildRecipeCopyContainsPublicOutput(t *testing.T) {
	root := t.TempDir()
	for name, value := range map[string]string{"public/index.html": "public", "public/.env": "fixture-private", "private.txt": "fixture-private", "public/nested/app.css": "body{}"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	public, err := recipeDirectory(root, "public")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "output")
	if err := copyRecipeTree(context.Background(), public, output, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".env", "private.txt"} {
		if _, err := os.Stat(filepath.Join(output, name)); !os.IsNotExist(err) {
			t.Fatalf("private input %s copied", name)
		}
	}
	if data, err := os.ReadFile(filepath.Join(output, "index.html")); err != nil || string(data) != "public" {
		t.Fatal("public output missing", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyRecipeTree(ctx, public, filepath.Join(t.TempDir(), "cancelled"), false); err == nil {
		t.Fatal("cancelled copy succeeded")
	}
	if _, err := recipeDirectory(root, "../"); err == nil {
		t.Fatal("checkout escape accepted")
	}
}

func TestBuildRecipeNetworkAndProxyPolicy(t *testing.T) {
	for _, network := range []string{"host", "panel-network", "container:worker"} {
		if _, _, err := recipeBuildPolicy(map[string]any{"network": network}); err == nil {
			t.Fatal("unsafe network accepted", network)
		}
	}
	if _, _, err := recipeBuildPolicy(map[string]any{"args": map[string]any{"HTTP_PROXY": "http://one.invalid", "http_proxy": "http://two.invalid"}}); err == nil {
		t.Fatal("ambiguous proxy accepted")
	}
	network, proxies, err := recipeBuildPolicy(map[string]any{"network": "none", "args": map[string]any{"ALL_PROXY": "http://proxy.invalid:3128", "UNSELECTED_SECRET": "fixture-private"}})
	if err != nil || network != "none" || proxies["HTTP_PROXY"] != "http://proxy.invalid:3128" || len(proxies) != 3 {
		t.Fatalf("policy changed: %s %#v %v", network, proxies, err)
	}
}

func TestBuildRecipeRefusesUnsupportedFrontendPolicies(t *testing.T) {
	for _, build := range []map[string]any{
		{"network": "none"},
		{"args": map[string]any{"HTTP_PROXY": "http://proxy.example.test:3128"}},
		{"args": map[string]any{"https_proxy": "${BUILD_PROXY}"}},
		{"args": map[string]any{"ALL_PROXY": nil}},
		{"args": []any{"HTTP_PROXY"}},
		{"args": []any{"HTTPS_PROXY=http://proxy.example.test"}},
	} {
		if err := validateRailpackBuildPolicy(build); err == nil || !strings.Contains(err.Error(), "pinned Railpack frontend does not apply") {
			t.Fatalf("unsafe frontend policy accepted: %#v %v", build, err)
		}
	}
	for _, build := range []map[string]any{nil, {}, {"network": "default"}, {"args": map[string]any{"HTTP_PROXY": ""}}, {"args": []any{"HTTP_PROXY="}}} {
		if err := validateRailpackBuildPolicy(build); err != nil {
			t.Fatal("default/no-proxy policy refused", err)
		}
	}
	r := recipeRequest()
	for _, definition := range []string{"network: none", "args: {HTTP_PROXY: 'http://proxy.example.test'}"} {
		r.Compose = "services:\n  app:\n    image: busybox:1.37\n    build:\n      context: .\n      " + definition + "\n"
		if err := ValidateRequest(r); err == nil {
			t.Fatal("worker admission dropped the policy", definition)
		}
	}
}
