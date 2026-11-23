package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMergedComposeDollarSnapshotRoundTrip(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose executable required")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(source, []byte("services:\n  app:\n    image: busybox:1.37\n    environment:\n      VALUE: ${VALUE:?required}\n    labels:\n      literal$$key: value$$literal\n    command: ['printf', '$$SHELL']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resolve := func(file string) map[string]any {
		t.Helper()
		out := &limitedBuffer{Limit: 1 << 20}
		if err := RunCommand(context.Background(), dir, []string{"docker", "compose", "--file", file, "config", "--format", "json"}, []string{"VALUE=resolved-$VALUE-$$PAIR"}, out); err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := unmarshalCompose(out.data.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	resolved := resolve(source)
	app := resolved["services"].(map[string]any)["app"].(map[string]any)
	if app["environment"].(map[string]any)["VALUE"] != "resolved-$VALUE-$$PAIR" || app["command"].([]any)[1] != "$SHELL" {
		t.Fatal("initial Compose resolution changed dollars", app)
	}
	if app["labels"].(map[string]any)["literal$$key"] != "value$literal" {
		t.Fatal("Compose mapping key or literal value changed", app["labels"])
	}
	snapshot := filepath.Join(dir, "snapshot.json")
	if err := writeJSON(snapshot, literalCompose(resolved)); err != nil {
		t.Fatal(err)
	}
	reread := resolve(snapshot)
	left, _ := json.Marshal(resolve(source)["services"])
	right, _ := json.Marshal(reread["services"])
	if string(left) != string(right) {
		t.Fatalf("resolved snapshot changed dollars: before %s after %s", left, right)
	}
	generated := resolve(source)
	request := Request{ID: "dollar-generation", Workload: "app", Rollout: &RolloutOptions{Strategy: "rolling", Replicas: map[string]int{"app": 2}}}
	traffic, _, err := generationConfig(request, generated)
	if err != nil {
		t.Fatal(err)
	}
	generation := filepath.Join(dir, "generation.json")
	if err = writeJSON(generation, literalCompose(generated)); err != nil {
		t.Fatal(err)
	}
	actual := resolve(generation)
	service := actual["services"].(map[string]any)[generationName(request.ID, "app")].(map[string]any)
	if service["environment"].(map[string]any)["VALUE"] != "resolved-$VALUE-$$PAIR" || service["command"].([]any)[1] != "$SHELL" || service["labels"].(map[string]any)["literal$$key"] != "value$literal" {
		t.Fatal("rollout generation changed literal dollars", service)
	}
	if err = restoreServiceNames(actual, traffic); err != nil {
		t.Fatal(err)
	}
	if actual["services"].(map[string]any)["app"] == nil {
		t.Fatal("retained generation could not restore its logical service")
	}
}

func TestMergedManagedDatabaseRejectsStatelessWorkerRequests(t *testing.T) {
	for _, mutate := range []func(*Request){
		func(r *Request) { r.Rollout = &RolloutOptions{Strategy: "rolling", Replicas: map[string]int{"db": 2}} },
		func(r *Request) { r.PullLatest = true },
		func(r *Request) { r.ComposeBundle = &ComposeBundle{} },
	} {
		request := Request{ID: "operation", Workload: "database", Action: "deploy", Compose: "services: {}", ManagedDatabase: true}
		mutate(&request)
		if err := ValidateRequest(request); err == nil {
			t.Fatal("database accepted stateless deployment policy")
		}
	}
}

func TestMergedJournalRetentionKeepsUncertainDatabaseMaintenance(t *testing.T) {
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, state := range []string{"failed", "interrupted"} {
		request := Request{ID: state, Workload: "database", DatabaseMaintenance: &DatabaseMaintenance{ID: state}}
		journal := journal{Operation: Operation{ID: state, Workload: "database", State: state, FinishedAt: &now}, Request: request}
		if err := writeJSON(engine.path(state), journal); err != nil {
			t.Fatal(err)
		}
		result, err := engine.PruneJournals(t.Context(), JournalPruneRequest{Candidates: []JournalCandidate{{ID: state, Workload: "database"}}})
		if err != nil || result.Protected != 1 || len(result.Pruned) != 0 {
			t.Fatal("uncertain database journal pruned", result, err)
		}
	}
}

func TestMergedComposeDollarSnapshotWriters(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose executable required")
	}
	for _, generated := range []bool{false, true} {
		scope := "plain"
		if generated {
			scope = "generation"
		}
		for _, writer := range []string{"limits", "links", "networks", "promotion", "guarded", "environment-preview", "release", "secrets"} {
			t.Run(scope+"/"+writer, func(t *testing.T) {
				engine, err := NewEngine(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				request := Request{ID: "dollar-writer", Workload: "dollar-app", Environment: map[string]string{"VALUE": "resolved-$VALUE-$$PAIR", "SECRET": "secret-$TOKEN-$$PAIR"}}
				dir := filepath.Join(engine.Root, "workloads", request.Workload)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				source := filepath.Join(dir, "compose.yaml")
				content := "services:\n  app:\n    image: busybox:1.37\n    pull_policy: never\n    environment:\n      VALUE: ${VALUE:?required}\n    labels:\n      literal$$key: value$$literal\n    command: ['printf', '$$SHELL']\n    healthcheck:\n      test: [CMD, 'true']\n"
				if err := os.WriteFile(source, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				env := []string{"VALUE=" + request.Environment["VALUE"]}
				resolve := func(path string) map[string]any {
					t.Helper()
					output := &limitedBuffer{Limit: 2 << 20}
					if err := RunCommand(t.Context(), dir, []string{"docker", "compose", "--file", path, "config", "--format", "json"}, env, output); err != nil {
						t.Fatal(err)
					}
					var config map[string]any
					if err := unmarshalCompose(output.data.Bytes(), &config); err != nil {
						t.Fatal(err)
					}
					return config
				}
				service := "app"
				if generated {
					config := resolve(source)
					rollout := Request{ID: "dollar-generation", Workload: request.Workload, Rollout: &RolloutOptions{Strategy: "rolling", Replicas: map[string]int{"app": 2}}}
					if _, _, err := generationConfig(rollout, config); err != nil {
						t.Fatal(err)
					}
					service = generationName(rollout.ID, "app")
					source = filepath.Join(dir, "generation.json")
					if err := writeJSON(source, literalCompose(config)); err != nil {
						t.Fatal(err)
					}
				}
				digest := "sha256:" + strings.Repeat("a", 64)
				network := strings.Repeat("b", 32)
				link := linkTargetNetwork(request.Workload, service)
				// Resolve and re-read every snapshot with real Compose; only daemon metadata is a local fixture.
				engine.Run = func(ctx context.Context, working string, args, environment []string, out io.Writer) error {
					if len(args) > 2 && args[0] == "docker" && args[1] == "compose" && slices.Contains(args, "config") {
						return RunCommand(ctx, working, args, environment, out)
					}
					if len(args) > 3 && args[1] == "network" {
						if args[2] == "ls" {
							if slices.Contains(args, "label=cove.managed=link") {
								_, err = io.WriteString(out, "fixture-link")
								return err
							}
							if slices.Contains(args, "--format") {
								_, err = io.WriteString(out, "cove-shared-"+network)
								return err
							}
						}
						if args[2] == "inspect" {
							if args[len(args)-1] == "fixture-link" {
								return json.NewEncoder(out).Encode([]map[string]any{{"Name": link, "Labels": map[string]string{"cove.managed": "link", "cove.link-role": "target", "cove.link-workload": request.Workload}, "Containers": map[string]any{}}})
							}
							return json.NewEncoder(out).Encode([]map[string]any{{"Driver": "bridge", "Internal": true, "Labels": map[string]string{"cove.shared-network": network}, "Containers": map[string]any{}}})
						}
					}
					if len(args) > 2 && args[1] == "image" {
						if args[2] == "tag" {
							return nil
						}
						if args[2] == "inspect" {
							value := digest
							if slices.Contains(args, "{{json .Config.Volumes}}") {
								value = "null"
							}
							_, err = io.WriteString(out, value)
							return err
						}
					}
					if len(args) > 1 && args[1] == "ps" {
						if writer == "release" {
							_, err = io.WriteString(out, "fixture-container")
						}
						return err
					}
					if len(args) > 1 && args[1] == "inspect" {
						value := digest
						if slices.Contains(args, `{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}`) {
							value = "running healthy"
						}
						_, err = io.WriteString(out, value)
						return err
					}
					if len(args) > 1 && args[1] == "compose" && slices.Contains(args, "run") {
						return nil
					}
					return fmt.Errorf("unexpected daemon command in snapshot fixture: %v", args)
				}
				args := []string{"docker", "compose", "--file", source}
				var path string
				switch writer {
				case "limits":
					request.Limits = map[string]ServiceLimits{service: {CPUs: 0.5, MemoryMB: 64}}
					path, err = engine.prepareLimits(t.Context(), request, dir, args, env, io.Discard)
				case "links":
					if err = os.MkdirAll(filepath.Dir(engine.linksDir("desired.json")), 0700); err != nil {
						t.Fatal(err)
					}
					if err = writeJSON(engine.linksDir("desired.json"), LinkConfig{Targets: []LinkTarget{{Workload: request.Workload, Service: service}}}); err != nil {
						t.Fatal(err)
					}
					var changed bool
					path, changed, err = engine.prepareLinks(t.Context(), request, dir, args, env)
					if err == nil && !changed {
						t.Fatal("link writer did not run")
					}
				case "networks":
					request.Networks = []NetworkAttachment{{ID: network, Service: service, Alias: "application"}}
					path, err = engine.prepareNetworks(t.Context(), request, dir, args, env, io.Discard)
				case "promotion":
					request.ImagePins = map[string]string{service: digest}
					path, err = engine.preparePromotion(t.Context(), request, dir, args, env)
				case "guarded":
					path, err = engine.testDeploymentCandidate(t.Context(), request, dir, args, env, io.Discard)
				case "environment-preview":
					request.EnvironmentPreview = &EnvironmentPreview{Preview: "preview", Source: "source"}
					path, err = engine.preparePreviewStorage(t.Context(), request, dir, args, env, io.Discard)
				case "release":
					if err = writeJSON(engine.path(request.ID), journal{Operation: Operation{ID: request.ID, Workload: request.Workload}, Request: request}); err != nil {
						t.Fatal(err)
					}
					var state deployedState
					state, err = engine.captureRelease(t.Context(), request, dir, args, env)
					path = state.ComposePath
				case "secrets":
					request.FileSecrets = []FileSecret{{Name: "secret", Variable: "SECRET", Services: []string{service}}}
					path, err = engine.prepareFileSecrets(t.Context(), request, dir, args, env)
					if err == nil {
						secret, readErr := os.ReadFile(filepath.Join(engine.Root, "file-secrets", request.Workload, request.ID, "secret"))
						if readErr != nil || string(secret) != request.Environment["SECRET"] {
							t.Fatal("file secret dollars changed", readErr)
						}
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				actual := resolve(path)["services"].(map[string]any)[service].(map[string]any)
				if actual["environment"].(map[string]any)["VALUE"] != request.Environment["VALUE"] || actual["command"].([]any)[1] != "$SHELL" || actual["labels"].(map[string]any)["literal$$key"] != "value$literal" {
					t.Fatal("writer changed literal dollars", actual)
				}
			})
		}
	}
}
