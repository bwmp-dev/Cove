package worker

import (
	"context"
	"cove/internal/outbound"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNixpacksMergeProxyPolicyPrecedesPlanner(t *testing.T) {
	for _, source := range []string{"environment", "saved", "request", "variable"} {
		t.Run(source, func(t *testing.T) {
			for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
				t.Setenv(key, "")
			}
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			r := recipeRequest()
			r.BuildRecipe.Engine = "nixpacks"
			proxy := "http://fixture:private-proxy-value@proxy.example.test:8080"
			switch source {
			case "environment":
				t.Setenv("HTTPS_PROXY", proxy)
			case "saved":
				if err := writeJSON(filepath.Join(e.Root, "outbound-proxy.json"), outbound.Config{HTTPS: proxy}); err != nil {
					t.Fatal(err)
				}
			case "request":
				r.OutboundProxy = &outbound.Config{HTTPS: proxy}
			case "variable":
				r.Environment = map[string]string{"HTTPS_PROXY": proxy}
			}
			e.Run = func(context.Context, string, []string, []string, io.Writer) error {
				t.Fatal("refused recipe reached Docker")
				return nil
			}
			_, err = e.Submit(r)
			if err == nil || !strings.Contains(err.Error(), "shared BuildKit network") || strings.Contains(err.Error(), "private-proxy-value") {
				t.Fatal("proxy policy was bypassed or exposed", err)
			}
			if _, err := os.Stat(e.path(r.ID)); !os.IsNotExist(err) {
				t.Fatal("refused recipe created an operation", err)
			}
			_, err = e.prepareRecipeBuild(t.Context(), r, e.Root, nil, nil, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "shared BuildKit network") {
				t.Fatal("execution did not recheck proxy policy", err)
			}
		})
	}
}

func TestRecipeMergeBuiltinDefaultsRespectEngineArguments(t *testing.T) {
	for _, engine := range []string{"compose", "static", "railpack", "buildpacks", "nixpacks"} {
		t.Run(engine, func(t *testing.T) {
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			r := Request{ID: "builtin-recipe", Workload: "builtin-recipe", BuiltinDefaults: map[string]string{CoveProject: "fixture-project"}, Environment: map[string]string{CoveProject: "fixture-project"}, BuildRecipe: &BuildRecipe{Engine: engine, Service: "app"}}
			e.Run = func(_ context.Context, _ string, _ []string, _ []string, out io.Writer) error {
				_, err := io.WriteString(out, `{"services":{"app":{"build":{"context":".","args":{"EXPLICIT":"kept"}}},"other":{"build":{"context":"."}}}}`)
				return err
			}
			path, err := e.prepareBuiltinEnvironment(t.Context(), r, e.Root, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var config struct {
				Services map[string]struct {
					Environment map[string]string `json:"environment"`
					Build       struct {
						Args map[string]string `json:"args"`
					} `json:"build"`
				} `json:"services"`
			}
			if err := json.Unmarshal(raw, &config); err != nil {
				t.Fatal(err)
			}
			app, other := config.Services["app"], config.Services["other"]
			if app.Environment[CoveProject] != "fixture-project" || other.Environment[CoveProject] != "fixture-project" || other.Build.Args[CoveProject] != "fixture-project" || app.Build.Args["EXPLICIT"] != "kept" {
				t.Fatal("runtime, other-service or explicit values changed", string(raw))
			}
			_, injected := app.Build.Args[CoveProject]
			acceptsArgs := engine != "buildpacks" && engine != "nixpacks"
			if injected != acceptsArgs {
				t.Fatalf("implicit build arguments incompatible with %s: %s", engine, raw)
			}
		})
	}
}
