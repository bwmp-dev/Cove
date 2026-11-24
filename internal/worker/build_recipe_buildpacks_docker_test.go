package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBuildpacksDockerNetworkBoundary(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("COVE_TEST_CNB_DOCKER") != "1" {
		t.Skip("requires the owned offline CNB daemon and metadata/proxy sentinels")
	}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	identity, err := e.output(ctx, "docker", "info", "--format", "{{.ID}}")
	if err != nil || identity == "" || identity != os.Getenv("COVE_TEST_CNB_DAEMON_ID") {
		t.Fatal("owned daemon identity mismatch", err)
	}
	proxy := os.Getenv("COVE_TEST_CNB_PROXY")
	gateway := os.Getenv("COVE_TEST_CNB_GATEWAY")
	if proxy == "" || gateway == "" {
		t.Fatal("owned policy sentinels required")
	}
	for _, policy := range []string{"none", "proxy"} {
		t.Run(policy, func(t *testing.T) {
			r := Request{ID: "cnb-network-" + policy, Workload: "cnb-policy", BuildRecipe: &BuildRecipe{Engine: "buildpacks"}}
			if err := os.MkdirAll(filepath.Join(e.recipeInputs(r), "cnb-docker-config"), 0700); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := e.cleanupRecipeResources(context.WithoutCancel(ctx), r); err != nil {
					t.Error(err)
				}
			}()
			if err := e.createBuildpacksState(ctx, r); err != nil {
				t.Fatal(err)
			}
			for _, role := range []string{"cnb-inputs", "cnb-secrets"} {
				if err := e.recipeCreateVolume(ctx, r, role); err != nil {
					t.Fatal(err)
				}
			}
			// Create the same verified run-layout subdirectory used by phase mounts.
			stage := []string{"docker", "create", "--pull", "never", "--name", recipeResourceName(r, "cnb-stage"), "--network", "none", "--read-only", "--cap-drop", "ALL", "--mount", "type=volume,src=" + recipeResourceName(r, "cnb-inputs") + ",dst=/inputs", "--entrypoint", "/bin/true"}
			stage = append(stage, recipeResourceLabels(r)...)
			if err := e.runBuildpacks(ctx, r, append(stage, BuildpacksBuilderImage), io.Discard); err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(e.recipeInputs(r), "run")
			if err := os.MkdirAll(directory, 0755); err != nil {
				t.Fatal(err)
			}
			if err := e.runBuildpacks(ctx, r, []string{"docker", "cp", directory, recipeResourceName(r, "cnb-stage") + ":/inputs/run"}, io.Discard); err != nil {
				t.Fatal(err)
			}
			network, managedProxy := "none", ""
			if policy == "proxy" {
				var err error
				network, managedProxy, err = e.prepareRecipeEgress(ctx, r, "default", map[string]string{"HTTP_PROXY": proxy, "HTTPS_PROXY": proxy})
				if err != nil {
					t.Fatal(err)
				}
			}
			arguments, err := buildpacksPhaseArguments(r, "builder", network, managedProxy, 2<<30)
			if err != nil {
				t.Fatal(err)
			}
			entrypoint := -1
			for index, value := range arguments {
				if value == "--entrypoint" {
					entrypoint = index
				}
			}
			if entrypoint < 0 {
				t.Fatal("missing phase entrypoint")
			}
			command := "if curl --max-time 2 --noproxy '*' -fsS http://" + gateway + ":2375/_ping; then exit 20; fi; if curl --max-time 2 --noproxy '*' -fsS http://169.254.169.254/; then exit 21; fi; "
			if policy == "proxy" {
				command += `test -n "$HTTP_PROXY"; result=$(curl --max-time 5 --noproxy '' -sS -w ' %{http_code}' http://1.1.1.1/); printf '%s\n' "$result"; test "${result##* }" = 502`
			} else {
				command += `if curl --max-time 2 -fsS http://1.1.1.1/; then exit 22; fi`
			}
			arguments = append(arguments[:entrypoint], "--entrypoint", "/bin/bash", BuildpacksBuilderImage, "-ec", command)
			if err := e.runBuildpacks(ctx, r, arguments, io.Discard); err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			if err := e.runBuildpacks(ctx, r, []string{"docker", "start", "--attach", recipeResourceName(r, "cnb-builder")}, &output); err != nil {
				t.Fatal(err, output.String())
			}
			var inspected struct {
				Config struct {
					User string
					Env  []string
				}
				HostConfig struct {
					ReadonlyRootfs, Privileged bool
					CapDrop                    []string
					Memory, MemorySwap         int64
					NetworkMode                string
				}
				Mounts []struct {
					Destination, Type string
					RW                bool
				}
			}
			if err := e.imageJSON(ctx, nil, &inspected, "docker", "inspect", "--format", "{{json .}}", recipeResourceName(r, "cnb-builder")); err != nil {
				t.Fatal(err)
			}
			if inspected.Config.User != "1002:1001" || !inspected.HostConfig.ReadonlyRootfs || inspected.HostConfig.Privileged || strings.Join(inspected.HostConfig.CapDrop, ",") != "ALL" || inspected.HostConfig.Memory != 2<<30 || inspected.HostConfig.MemorySwap != 2<<30 || inspected.HostConfig.NetworkMode != network {
				t.Fatal("CNB phase confinement changed", inspected)
			}
			for _, mount := range inspected.Mounts {
				if mount.Type == "bind" || strings.Contains(mount.Destination, "sock") || strings.HasPrefix(mount.Destination, "/var/lib/cove") {
					t.Fatal("host state reached third-party phase", mount)
				}
				if mount.Destination != "/work" && mount.RW {
					t.Fatal("frozen inputs or platform secrets were writable", mount)
				}
			}
			fmt.Fprintln(os.Stdout, "CNB network boundary passed:", policy)
		})
	}
}
