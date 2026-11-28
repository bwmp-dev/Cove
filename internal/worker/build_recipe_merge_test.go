package worker

import (
	"context"
	"cove/internal/outbound"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildRecipeComposeDollarsRoundTrip(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose executable required")
	}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := recipeRequest()
	r.BuildRecipe = &BuildRecipe{Engine: "static", Service: "app", Directory: ".", Output: "public"}
	r.Environment = map[string]string{"VALUE": "resolved-$VALUE-$$PAIR"}
	r.Compose = "services:\n  app:\n    image: busybox:1.37\n    environment:\n      VALUE: ${VALUE:?required}\n    labels:\n      literal$$key: value$$literal\n    command: ['printf', '$$SHELL']\n"
	if _, err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	public := filepath.Join(dir, "releases", r.ID, "public")
	if err := os.MkdirAll(public, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(public, "index.html"), []byte("public"), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(source, []byte(r.Compose), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"VALUE=" + r.Environment["VALUE"]}
	resolve := func(path string) []byte {
		t.Helper()
		out := &limitedBuffer{Limit: 2 << 20}
		if err := RunCommand(t.Context(), dir, []string{"docker", "compose", "--project-name", "cove-recipe-dollar", "--file", path, "config", "--format", "json"}, env, out); err != nil {
			t.Fatal(err)
		}
		return out.data.Bytes()
	}
	initial := resolve(source)
	snapshot, err := e.prepareRecipeBuild(t.Context(), r, dir, initial, env, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	service := func(data []byte) map[string]any {
		t.Helper()
		var config map[string]any
		if err := unmarshalCompose(data, &config); err != nil {
			t.Fatal(err)
		}
		return config["services"].(map[string]any)["app"].(map[string]any)
	}
	before, after := service(initial), service(resolve(snapshot))
	for _, field := range []string{"environment", "labels", "command"} {
		if !reflect.DeepEqual(before[field], after[field]) {
			a, _ := json.Marshal(before[field])
			b, _ := json.Marshal(after[field])
			t.Errorf("recipe changed literal dollars in %s: before %s, after %s", field, a, b)
		}
	}
}

func TestBuildRecipeMergeProxyPolicyPrecedesPlanner(t *testing.T) {
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
			if err == nil || !strings.Contains(err.Error(), "pinned Railpack frontend") || strings.Contains(err.Error(), "private-proxy-value") {
				t.Fatal("proxy policy was bypassed or exposed", err)
			}
			if _, err := os.Stat(e.path(r.ID)); !os.IsNotExist(err) {
				t.Fatal("refused recipe created an operation", err)
			}
			_, err = e.prepareRecipeBuild(t.Context(), r, e.Root, nil, nil, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "pinned Railpack frontend") {
				t.Fatal("execution did not recheck proxy policy", err)
			}
		})
	}
}
