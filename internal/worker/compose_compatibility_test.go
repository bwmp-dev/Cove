package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeCompatibilityNative(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker Compose executable required")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose plugin required")
	}
	fixtures := []struct {
		name              string
		files             map[string]string
		extra             []string
		environment       []string
		wantEnvironment   map[string]string
		wantServices      []string
		wantNoEnvironment []string
	}{
		{name: "include", files: map[string]string{"compose.yaml": "include: [included.yaml]\nservices:\n  web:\n    image: busybox:1.37\n", "included.yaml": "services:\n  included:\n    image: busybox:1.37\n"}, wantServices: []string{"web", "included"}},
		{name: "required-variable", files: map[string]string{"compose.yaml": "services:\n  web:\n    image: busybox:1.37\n    environment:\n      VALUE: ${VALUE:?VALUE is required}\n"}, environment: []string{"VALUE=provided"}, wantEnvironment: map[string]string{"VALUE": "provided"}},
		{name: "profiles", files: map[string]string{"compose.yaml": "services:\n  web:\n    image: busybox:1.37\n  debug:\n    profiles: [debug]\n    image: busybox:1.37\n"}, extra: []string{"--profile", "debug"}, wantServices: []string{"web", "debug"}},
		{name: "extends", files: map[string]string{"compose.yaml": "services:\n  web:\n    extends:\n      file: common.yaml\n      service: common\n", "common.yaml": "services:\n  common:\n    image: busybox:1.37\n    environment:\n      EXTENDED: inherited\n"}, wantEnvironment: map[string]string{"EXTENDED": "inherited"}},
		{name: "merge-tags", files: map[string]string{"compose.yaml": "services:\n  web:\n    image: busybox:1.37\n    environment:\n      OLD: base\n      VALUE: base\n    ports: ['8080:80']\n", "override.yaml": "services:\n  web:\n    environment:\n      OLD: !reset null\n      VALUE: !override replacement\n    ports: !override ['9090:90']\n"}, extra: []string{"--file", "override.yaml"}, wantEnvironment: map[string]string{"VALUE": "replacement"}, wantNoEnvironment: []string{"OLD"}},
		{name: "multiple-env-files", files: map[string]string{"compose.yaml": "services:\n  web:\n    image: busybox:1.37\n    env_file: [base.env,override.env]\n", "base.env": "BASE=base\nVALUE=first\n", "override.env": "VALUE=last\n"}, wantEnvironment: map[string]string{"BASE": "base", "VALUE": "last"}},
		{name: "null-network-name", files: map[string]string{"compose.yaml": "services:\n  web:\n    image: busybox:1.37\n    networks: [private]\nnetworks:\n  private:\n    name: null\n"}, wantServices: []string{"web"}},
		{name: "literal-dollar-snapshot", files: map[string]string{"compose.yaml": "services:\n  web:\n    image: busybox:1.37\n    environment:\n      VALUE: ${VALUE}\n    command: ['printf', '$$LITERAL']\n    labels:\n      example.literal: '$$LABEL'\n"}, environment: []string{"VALUE=dollar-$NOT_EXPANDED"}},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(e.Root, "workloads", "app")
			if err = os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			for name, content := range fixture.files {
				if err = os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			request := Request{ID: "compat-" + fixture.name, Workload: "app", Action: "deploy", Compose: fixture.files["compose.yaml"]}
			if _, err = e.Submit(request); err != nil {
				t.Fatal(err)
			}
			primary, err := normalizeComposeFile(filepath.Join(directory, "compose.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"docker", "compose", "--project-name", "cove-app", "--file", primary}
			args = append(args, fixture.extra...)
			resolve := func(arguments []string) map[string]any {
				t.Helper()
				out := &limitedBuffer{Limit: 2 * 1024 * 1024}
				if err := RunCommand(context.Background(), directory, append(append([]string{}, arguments...), "config", "--format", "json"), fixture.environment, out); err != nil {
					t.Fatal("Compose compatibility failed", err, out.data.String())
				}
				var config map[string]any
				if json.Unmarshal(out.data.Bytes(), &config) != nil {
					t.Fatal("invalid Compose JSON")
				}
				return config
			}
			before := resolve(args)
			e.Run = func(ctx context.Context, dir string, arguments, environment []string, out io.Writer) error {
				if len(arguments) > 1 && arguments[1] == "compose" {
					return RunCommand(ctx, dir, arguments, environment, out)
				}
				if len(arguments) > 1 && arguments[1] == "ps" {
					_, err := io.WriteString(out, "aaaaaaaaaaaa\n")
					return err
				}
				if len(arguments) > 1 && arguments[1] == "inspect" {
					_, err := io.WriteString(out, "sha256:"+strings.Repeat("a", 64))
					return err
				}
				if len(arguments) > 2 && arguments[1] == "image" && arguments[2] == "tag" {
					return nil
				}
				return errors.New("unexpected fixture command")
			}
			if fixture.name == "literal-dollar-snapshot" {
				request.Limits = map[string]ServiceLimits{"web": {CPUs: 0.5}}
				prepared, err := e.prepareLimits(context.Background(), request, directory, args, fixture.environment, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				args = []string{"docker", "compose", "--project-name", "cove-app", "--file", prepared}
			}
			snapshot, err := e.captureRelease(context.Background(), request, directory, args, fixture.environment)
			if err != nil {
				t.Fatal("release snapshot failed", err)
			}
			after := resolve([]string{"docker", "compose", "--project-name", "cove-app", "--file", snapshot.ComposePath})
			services := after["services"].(map[string]any)
			for _, name := range fixture.wantServices {
				if services[name] == nil {
					t.Fatalf("service %s lost", name)
				}
			}
			web := services["web"].(map[string]any)
			environment, _ := web["environment"].(map[string]any)
			for name, value := range fixture.wantEnvironment {
				if environment[name] != value {
					t.Fatalf("environment %s changed", name)
				}
			}
			for _, name := range fixture.wantNoEnvironment {
				if _, exists := environment[name]; exists {
					t.Fatalf("reset variable %s retained", name)
				}
			}
			if fixture.name == "null-network-name" {
				networks := after["networks"].(map[string]any)
				if networks["private"].(map[string]any)["name"] != "cove-app_private" {
					t.Fatal("null network name was not resolved consistently")
				}
			}
			if fixture.name == "literal-dollar-snapshot" {
				original := before["services"].(map[string]any)["web"].(map[string]any)
				for _, field := range []string{"environment", "command", "labels"} {
					first, _ := json.Marshal(original[field])
					second, _ := json.Marshal(web[field])
					if string(first) != string(second) {
						t.Errorf("snapshot changed literal %s: before%s after%s", field, first, second)
					}
				}
			}
		})
	}
	t.Run("missing-required-variable", func(t *testing.T) {
		directory := t.TempDir()
		file := filepath.Join(directory, "compose.yaml")
		if err := os.WriteFile(file, []byte("services:\n  web:\n    image: busybox:1.37\n    environment:\n      VALUE: ${COVE_MISSING_COMPAT:?required}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := RunCommand(context.Background(), directory, []string{"docker", "compose", "--file", file, "config", "--format", "json"}, nil, io.Discard); err == nil {
			t.Fatal("missing required variable accepted")
		}
	})
}
