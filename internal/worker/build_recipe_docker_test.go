package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runBuildRecipeTestProcess() bool {
	mise := filepath.Base(os.Args[0]) == "mise-"+RailpackMiseVersion
	if !mise && (len(os.Args) < 2 || os.Args[1] != "build-recipe-plan" && os.Args[1] != "build-recipe-output" && os.Args[1] != "build-recipe-egress") {
		return false
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch {
	case mise:
		err = RunRailpackMise(ctx, os.Args[1:], os.Stdout, os.Stderr)
	case os.Args[1] == "build-recipe-plan":
		err = RunRailpackPlanner(ctx)
	case os.Args[1] == "build-recipe-egress":
		err = RunBuildRecipeEgress(ctx)
	case len(os.Args) == 3:
		err = ReadRailpackOutput(os.Args[2], os.Stdout)
	default:
		err = fmt.Errorf("planner output kind required")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
	return true
}

func TestRailpackPlannerDockerPolicy(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("COVE_TEST_RECIPE_DOCKER") != "1" {
		t.Skip("requires the owned offline recipe daemon")
	}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := e.output(context.Background(), "docker", "info", "--format", "{{.ID}}")
	if err != nil || identity == "" || identity != os.Getenv("COVE_TEST_RECIPE_DAEMON_ID") {
		t.Fatal("owned daemon identity mismatch", err)
	}
	owner, err := os.ReadFile("/fixture/.recipe-owner")
	if err != nil || string(owner) != os.Getenv("COVE_TEST_RECIPE_OWNER") {
		t.Fatal("owned recipe fixture marker mismatch")
	}
	proxy := os.Getenv("COVE_TEST_RECIPE_PROXY")
	if proxy == "" {
		t.Fatal("owned metadata sinkhole required")
	}
	for _, policy := range []string{"offline-shell", "none", "default"} {
		t.Run(policy, func(t *testing.T) {
			r := recipeRequest()
			r.ID = "planner-" + policy
			inputs := e.recipeInputs(r)
			if err := os.MkdirAll(filepath.Join(inputs, "source"), 0755); err != nil {
				t.Fatal(err)
			}
			files := map[string]string{"package.json": `{"name":"fixture","version":"1.0.0","engines":{"node":"24.0.0"},"scripts":{"start":"node index.js"}}`, "index.js": "console.log('fixture')"}
			if policy == "offline-shell" {
				files = map[string]string{"start.sh": "#!/bin/sh\nprintf fixture\\n\n", "railpack.json": `{"provider":"shell"}`}
			}
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(inputs, "source", name), []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			defer func() {
				if err := e.cleanupRecipeResources(context.Background(), r); err != nil {
					t.Error(err)
				}
			}()
			network := "none"
			proxies := map[string]string{}
			if policy == "default" {
				network = "default"
				proxies["HTTP_PROXY"] = proxy
				proxies["HTTPS_PROXY"] = proxy
			}
			ctx, cancel := context.WithTimeout(context.Background(), 115*time.Second)
			defer cancel()
			plan, err := e.prepareRailpackPlan(ctx, r, network, proxies, nil)
			if policy == "offline-shell" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := validateRecipePlan(plan, nil); err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "metadata") {
				t.Fatalf("metadata policy did not fail clearly: %v", err)
			}
			if policy == "default" {
				var launch struct {
					Config struct {
						User string
						Env  []string
					}
					HostConfig struct {
						ReadonlyRootfs bool
						CapDrop        []string
						PidsLimit      int
					}
					NetworkSettings struct{ Networks map[string]any }
					Mounts          []struct {
						Destination string
						RW          bool
					}
				}
				resource := recipeResourceName(r, "planner")
				if err := e.imageJSON(context.Background(), nil, &launch, "docker", "inspect", "--format", "{{json .}}", resource); err != nil {
					t.Fatal(err)
				}
				if launch.Config.User != "65534:65534" || !launch.HostConfig.ReadonlyRootfs || strings.Join(launch.HostConfig.CapDrop, ",") != "ALL" || launch.HostConfig.PidsLimit != 64 || len(launch.NetworkSettings.Networks) != 1 {
					t.Fatal("planner confinement changed")
				}
				for _, mount := range launch.Mounts {
					if mount.Destination != "/inputs" || mount.RW {
						t.Fatal("unexpected writable or host mount", mount)
					}
				}
				for _, value := range launch.Config.Env {
					if strings.Contains(value, "fixture-") {
						t.Fatal("fixture credentials reached planner")
					}
				}
				gateway, err := e.output(context.Background(), "docker", "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := e.output(context.Background(), "docker", "exec", resource, "/bin/busybox", "nc", "-w", "1", gateway, "2375"); err == nil {
					t.Fatal("planner reached node-local Docker service")
				}
				if _, err := e.output(context.Background(), "docker", "exec", "-e", "http_proxy=", "-e", "https_proxy=", "-e", "HTTP_PROXY=", "-e", "HTTPS_PROXY=", "-e", "ALL_PROXY=", "-e", "all_proxy=", "-e", "NO_PROXY=*", "-e", "no_proxy=*", resource, "/bin/busybox", "wget", "-T", "2", "-qO-", "http://169.254.169.254/"); err == nil {
					t.Fatal("planner reached owned metadata sentinel")
				}
				data, err := os.ReadFile("/fixture/state/proxy.jsonl")
				if err != nil {
					t.Fatal("no owned proxy request evidence", err)
				}
				for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
					var event map[string]string
					if json.Unmarshal([]byte(line), &event) != nil || event["method"] != "CONNECT" {
						t.Fatalf("unexpected metadata event: %s", line)
					}
				}
			}
		})
	}
}
