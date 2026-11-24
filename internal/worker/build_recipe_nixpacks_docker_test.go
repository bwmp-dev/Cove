package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNixpacksDockerPlannerAndBuildNetworkBoundary(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("COVE_TEST_NIXPACKS_DOCKER") != "1" {
		t.Skip("requires the owned offline Nixpacks daemon")
	}
	e := &Engine{Root: "/var/lib/cove", Run: RunCommand}
	identity, err := e.output(context.Background(), "docker", "info", "--format", "{{.ID}}")
	if err != nil || identity == "" || identity != os.Getenv("COVE_TEST_NIXPACKS_DAEMON_ID") {
		t.Fatal("owned daemon identity mismatch", err)
	}
	owner, err := os.ReadFile("/fixture/.nixpacks-owner")
	if err != nil || string(owner) != os.Getenv("COVE_TEST_NIXPACKS_OWNER") {
		t.Fatal("owned fixture marker mismatch")
	}
	gateway := os.Getenv("COVE_TEST_NIXPACKS_GATEWAY")
	if gateway == "" {
		t.Fatal("owned sentinel gateway required")
	}
	fixture, err := os.ReadFile("/tmp/fixture-server")
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{"none", "proxy-bypass-control", "detect-node"} {
		t.Run(policy, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			r := recipeRequest()
			r.ID, r.BuildRecipe = "nixpacks-"+policy, &BuildRecipe{Engine: "nixpacks", Service: "app", Directory: "."}
			r.Environment = map[string]string{"COVE_UNSELECTED_VALUE": "fixture-value-not-in-planner"}
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "fixture-server"), fixture, 0755); err != nil {
				t.Fatal(err)
			}
			configuration := "providers = []\n[phases.build]\ncmds = [\"./fixture-server network-check " + policy + " http://" + gateway + ":16000/fixture/network-sentinel\"]\n[start]\ncmd = \"./fixture-server\"\n"
			if policy == "detect-node" {
				for name, data := range map[string]string{"package.json": `{"name":"offline-planner-fixture","version":"1.0.0","scripts":{"start":"node index.js"}}`, "index.js": "console.log('fixture')\n"} {
					if err := os.WriteFile(filepath.Join(source, name), []byte(data), 0644); err != nil {
						t.Fatal(err)
					}
				}
			} else if err := os.WriteFile(filepath.Join(source, "nixpacks.toml"), []byte(configuration), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(e.recipeInputs(r), 0700); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := e.cleanupRecipeResources(context.Background(), r); err != nil {
					t.Error(err)
				}
				if err := os.RemoveAll(e.recipeInputs(r)); err != nil {
					t.Error(err)
				}
			}()
			original := map[string]any{}
			if policy == "none" {
				original["network"] = "none"
				wrong := r
				wrong.BuildPlatform = "linux/arm64"
				if runtime.GOARCH == "arm64" {
					wrong.BuildPlatform = "linux/amd64"
				}
				if _, err := e.prepareNixpacksBuild(ctx, wrong, source, original); err == nil || !strings.Contains(err.Error(), "matching build-worker") {
					t.Fatal("mismatched runtime platform was not refused before planning", err)
				}
			}
			if policy == "proxy-bypass-control" {
				refused := map[string]any{"args": map[string]any{"HTTP_PROXY": "http://" + gateway + ":16001"}}
				if _, err := e.prepareNixpacksBuild(ctx, r, source, refused); err == nil || !strings.Contains(err.Error(), "proxy-only") {
					t.Fatal("unhonored proxy policy was not refused before planning", err)
				}
				if _, err := os.Stat(filepath.Join(e.recipeInputs(r), "source")); !os.IsNotExist(err) {
					t.Fatal("refused proxy policy started preparing source")
				}
			}
			build, err := e.prepareNixpacksBuild(ctx, r, source, original)
			if err != nil {
				t.Fatal(err)
			}
			var planner struct {
				Config struct {
					User string
					Env  []string
				}
				HostConfig struct {
					NetworkMode    string
					ReadonlyRootfs bool
					CapDrop        []string
					Memory         int64
					MemorySwap     int64
					PidsLimit      int
				}
				Mounts []struct {
					Destination string
					RW          bool
				}
			}
			if err := e.imageJSON(ctx, nil, &planner, "docker", "inspect", "--format", "{{json .}}", recipeResourceName(r, "planner")); err != nil {
				t.Fatal(err)
			}
			if planner.Config.User != "65534:65534" || !planner.HostConfig.ReadonlyRootfs || planner.HostConfig.NetworkMode != "none" || strings.Join(planner.HostConfig.CapDrop, ",") != "ALL" || planner.HostConfig.Memory != 512<<20 || planner.HostConfig.MemorySwap != 512<<20 || planner.HostConfig.PidsLimit != 128 || len(planner.Mounts) != 1 || planner.Mounts[0].Destination != "/inputs" || planner.Mounts[0].RW || strings.Contains(strings.Join(planner.Config.Env, "\n"), "fixture-value") {
				t.Fatal("planner confinement changed", planner)
			}
			if policy == "detect-node" {
				generated, err := os.ReadFile(filepath.Join(e.recipeInputs(r), "source", ".nixpacks", "Dockerfile"))
				if err != nil || !strings.Contains(string(generated), "nix-env") || !strings.Contains(string(generated), "npm") || strings.Contains(string(generated), "fixture-value-not-in-planner") {
					t.Fatal("offline Node detection did not produce validated package build inputs", err)
				}
				t.Log("real offline Node detection and generated input validation passed; no Node dependency build executed")
				return
			}
			if policy == "proxy-bypass-control" {
				// This deliberately bypasses product admission to pin the shared builder's upstream limitation.
				build["args"] = map[string]string{"HTTP_PROXY": "http://" + gateway + ":16001", "HTTPS_PROXY": "http://" + gateway + ":16001"}
			}
			tag := "fixture/nixpacks-" + policy + ":owned"
			compose := filepath.Join(e.recipeInputs(r), "boundary.compose.json")
			if err := writeJSON(compose, literalCompose(map[string]any{"services": map[string]any{"app": map[string]any{"image": tag, "build": build}}})); err != nil {
				t.Fatal(err)
			}
			var buildLog bytes.Buffer
			if err := e.buildImages(ctx, e.Root, []string{"docker", "compose", "--project-name", "cove-nixpacks-boundary", "--file", compose}, nil, &buildLog, true); err != nil {
				tail := buildLog.String()
				if len(tail) > 8000 {
					tail = tail[len(tail)-8000:]
				}
				t.Fatal("generated build network sentinel failed", err, tail)
			}
			proof, err := e.output(ctx, "docker", "run", "--rm", "--pull", "never", "--network", "none", "--read-only", "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--memory", "32m", "--cpus", ".25", "--pids-limit", "16", "--entrypoint", "/bin/cat", tag, "/app/network-proof")
			if err != nil || proof != policy {
				t.Fatal("network sentinel proof missing", proof, err)
			}
			buildOwner, err := e.buildOwner()
			if err != nil {
				t.Fatal(err)
			}
			label, err := e.output(ctx, "docker", "image", "inspect", "--format", `{{index .Config.Labels "dev.cove.build-owner"}}`, tag)
			if err != nil || label != buildOwner {
				t.Fatal("network proof image ownership changed", label, err)
			}
			if err := e.Run(ctx, e.Root, []string{"docker", "image", "rm", tag}, nil, io.Discard); err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(planner.HostConfig)
			t.Log("real pinned planner and generated BuildKit network proof", policy, string(encoded))
		})
	}
}
