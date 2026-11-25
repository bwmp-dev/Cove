package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestBuiltinVariablesReachRuntimeAndBuildPreservingOverrides(t *testing.T) {
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := Request{ID: "builtin-release", Workload: "builtin-app", Action: "deploy", Compose: "services:\n  app:\n    image: busybox:1.37\n", BuiltinDefaults: map[string]string{CoveProject: "p", CoveRelease: "builtin-release", CoveCommitSHA: "commit", CoveURL: "https://app.example"}, Environment: map[string]string{CoveURL: "user-$literal", "TOKEN": "secret-value"}}
	if _, err = engine.Submit(request); err != nil {
		t.Fatal(err)
	}
	checked := false
	engine.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		if len(args) >= 2 && args[len(args)-2] == "--format" && args[len(args)-1] == "json" && !strings.Contains(strings.Join(args, " "), "images") {
			if !checked {
				if !strings.Contains(strings.Join(env, "\n"), CoveRelease+"=builtin-release") || !strings.Contains(strings.Join(env, "\n"), CoveURL+"=user-$literal") {
					t.Fatal("builtins missing from Compose interpolation environment")
				}
				_, err := io.WriteString(out, `{"services":{"app":{"image":"busybox:1.37","environment":{"COVE_PROJECT":"compose-project","TOKEN":"secret-value"},"build":{"context":".","args":{"COVE_RELEASE":"compose-release"}}}}}`)
				checked = true
				return err
			}
		}
		if handled, err := releaseFixture(args, out); handled {
			return err
		}
		if strings.Contains(strings.Join(args, " "), " up ") {
			raw, err := os.ReadFile(filepath.Join(engine.Root, "workloads", request.Workload, "builtin-"+request.ID+".json"))
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
			if json.Unmarshal(raw, &config) != nil {
				t.Fatal("invalid builtin compose file")
			}
			app := config.Services["app"]
			if app.Environment[CoveProject] != "compose-project" || app.Environment[CoveRelease] != "builtin-release" || app.Environment[CoveURL] != "user-$$literal" || app.Build.Args[CoveRelease] != "compose-release" || app.Build.Args[CoveProject] != "p" {
				t.Fatal("builtins/overrides did not reach runtime/build")
			}
			io.WriteString(out, "p public metadata, secret-value and user-$literal\n")
		}
		return nil
	}
	var jobs sync.WaitGroup
	if err = engine.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	op, err := engine.Status(request.ID)
	if err != nil || op.State != "succeeded" {
		t.Fatal(op, err)
	}
	log, err := engine.Logs(request.ID)
	if err != nil || strings.Contains(log, "secret-value") || strings.Contains(log, "user-$literal") || !strings.Contains(log, "p public metadata") {
		t.Fatal("public builtin/secret redaction failed", log, err)
	}
	raw, err := os.ReadFile(filepath.Join(engine.Root, "workloads", request.Workload, "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	var current deployedState
	if json.Unmarshal(raw, &current) != nil || current.PublicBuiltins[CoveProject] != "p" || current.Environment[CoveRelease] != "builtin-release" {
		t.Fatal("builtin release state missing")
	}
	if _, ok := current.PublicBuiltins[CoveURL]; ok {
		t.Fatal("user builtin override marked public")
	}
}

func TestBuiltinDefaultsRejectInvalidMetadata(t *testing.T) {
	for _, values := range []map[string]string{{"PATH": "bad"}, {CoveURL: "null\x00byte"}, {CoveProject: strings.Repeat("x", 65537)}} {
		if validateBuiltins(values) == nil {
			t.Fatal("invalid metadata allowed")
		}
	}
}

func TestBuiltinCommitUsesCheckoutAndPreflightPreservesOverride(t *testing.T) {
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	source := &GitSource{Repository: "https://example.com/team/repository.git", Branch: "main", ComposePath: "compose.yaml"}
	request := Request{ID: "builtin-git", Workload: "git-app", Action: "deploy", Source: source, BuiltinDefaults: map[string]string{CoveCommitSHA: "", CoveBranch: "main"}}
	if _, err = engine.Submit(request); err != nil {
		t.Fatal(err)
	}
	checked := false
	engine.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		if strings.Contains(strings.Join(args, " "), " clone ") {
			directory := args[len(args)-1]
			if err := os.MkdirAll(directory, 0700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(directory, "compose.yaml"), []byte("services: {}"), 0600)
		}
		if args[0] == "git" && args[1] == "rev-parse" {
			_, err := io.WriteString(out, commit)
			return err
		}
		if args[0] == "docker" && strings.Contains(strings.Join(args, " "), " config ") && !checked {
			if !strings.Contains(strings.Join(env, "\n"), CoveCommitSHA+"="+commit) {
				t.Fatal("actual checkout commit not exposed to Compose")
			}
			checked = true
			_, err := io.WriteString(out, `{"services":{"app":{"image":"nginx:alpine"}}}`)
			return err
		}
		if handled, err := releaseFixture(args, out); handled {
			return err
		}
		return nil
	}
	var jobs sync.WaitGroup
	if err = engine.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	result, _ := engine.Status(request.ID)
	if result.State != "succeeded" || !checked || result.Revision != commit {
		t.Fatal("Git builtin failed", result)
	}
	request.Environment = map[string]string{CoveCommitSHA: ""}
	checked = false
	engine.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		if strings.Contains(strings.Join(args, " "), " clone ") {
			directory := args[len(args)-1]
			os.MkdirAll(directory, 0700)
			return os.WriteFile(filepath.Join(directory, "compose.yaml"), []byte("services: {}"), 0600)
		}
		if args[0] == "git" && args[1] == "rev-parse" {
			_, err := io.WriteString(out, commit)
			return err
		}
		if strings.Contains(strings.Join(args, " "), " config ") {
			if strings.Contains(strings.Join(env, "\n"), CoveCommitSHA+"="+commit) {
				t.Fatal("preflight overwrote explicit empty variable")
			}
			checked = true
			_, err := io.WriteString(out, `{"services":{"app":{"image":"nginx:alpine"}}}`)
			return err
		}
		return nil
	}
	engine.Preflight(context.Background(), request)
	if !checked {
		t.Fatal("preflight did not validate Compose")
	}
}

func TestBuiltinComposeLocalConfigPreservesLiteralValues(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker Compose executable required")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose plugin required")
	}
	directory := t.TempDir()
	compose := filepath.Join(directory, "compose.yaml")
	content := "services:\n  app:\n    image: busybox:1.37\n    environment:\n      EXPLICIT: ${VALUE}\n    labels:\n      literal$$key: value$$literal\n    command: [printf, '$$SHELL']\n    build:\n      context: .\n      args:\n        COVE_PROJECT: explicit\n"
	if err := os.WriteFile(compose, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{Run: RunCommand}
	defaults := map[string]string{CoveProject: "project", CoveURL: "https://app.example/$literal", CoveRelease: "release"}
	environment, public := mergeBuiltinDefaults(map[string]string{"VALUE": "dollar-$NOT_EXPANDED"}, defaults)
	if len(public) != 3 {
		t.Fatal("missing public defaults")
	}
	request := Request{ID: "local-config", Environment: environment, BuiltinDefaults: defaults}
	env := []string{}
	for key, value := range environment {
		env = append(env, key+"="+value)
	}
	path, err := engine.prepareBuiltinEnvironment(context.Background(), request, directory, []string{"docker", "compose", "--file", compose}, env)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := RunCommand(context.Background(), directory, []string{"docker", "compose", "--file", path, "config", "--format", "json"}, env, &output); err != nil {
		t.Fatal(err, output.String())
	}
	var config struct {
		Services map[string]struct {
			Environment map[string]string `json:"environment"`
			Labels      map[string]string `json:"labels"`
			Command     []string          `json:"command"`
			Build       struct {
				Args map[string]string `json:"args"`
			} `json:"build"`
		} `json:"services"`
	}
	if json.Unmarshal(output.Bytes(), &config) != nil {
		t.Fatal("invalid Compose output")
	}
	app := config.Services["app"]
	if len(app.Labels) != 1 || app.Labels["literal$$$$key"] != "value$$literal" || len(app.Command) != 2 || app.Command[1] != "$$SHELL" {
		t.Fatalf("Compose reread changed literal label keys or command tokens: %#v", app)
	}
	if app.Environment["EXPLICIT"] != "dollar-$$NOT_EXPANDED" || app.Environment[CoveURL] != "https://app.example/$$literal" || app.Build.Args[CoveProject] != "explicit" || app.Build.Args[CoveRelease] != "release" {
		t.Fatalf("Compose reread changed literal values or overrides: %#v", app)
	}
}

func TestBuiltinFailureDoesNotExposeResolvedConfiguration(t *testing.T) {
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := Request{ID: "failed-builtin", Workload: "app", Action: "deploy", Compose: "services: {}", BuiltinDefaults: map[string]string{CoveProject: "project"}, Environment: map[string]string{"PASSWORD": "secret-config-value"}}
	if _, err = engine.Submit(request); err != nil {
		t.Fatal(err)
	}
	engine.Run = func(_ context.Context, _ string, _, _ []string, out io.Writer) error {
		io.WriteString(out, `{"environment":{"PASSWORD":"secret-config-value"}}`)
		return errors.New("sensitive provider output")
	}
	var jobs sync.WaitGroup
	if err = engine.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	result, _ := engine.Status(request.ID)
	log, err := engine.Logs(request.ID)
	if err != nil || result.State != "failed" || !strings.Contains(log, "Could not prepare built-in variables") || strings.Contains(log, "secret-config-value") || strings.Contains(log, "sensitive provider output") {
		t.Fatal("builtin failure was unclear or exposed config", err)
	}
}
