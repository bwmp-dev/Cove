package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportedComposeNativeRoundTrip(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose plugin required")
	}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.PrepareImportedCompose(context.Background(), Request{Compose: "services:\n  app:\n    image: busybox:1.37\n    environment:\n      PASSWORD: ${PASSWORD:?required}\n    command: ['sh', '-c', 'echo $$HOME']\n", Environment: map[string]string{"PASSWORD": "private-$NO_EXPANSION"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.Root, "protected.json")
	if err = os.WriteFile(path, []byte(result.Compose), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{}
	for key, value := range result.Environment {
		env = append(env, key+"="+value)
	}
	out := &limitedBuffer{Limit: 1 << 20}
	if err = RunCommand(context.Background(), e.Root, []string{"docker", "compose", "--project-name", "cove-accepted", "--file", path, "config", "--format", "json"}, env, out); err != nil {
		t.Fatal(err, out.data.String())
	}
	var document map[string]any
	if unmarshalCompose(out.data.Bytes(), &document) != nil {
		t.Fatal("invalid native protected result")
	}
	service := document["services"].(map[string]any)["app"].(map[string]any)
	if service["environment"].(map[string]any)["PASSWORD"] != "private-$NO_EXPANSION" || service["command"].([]any)[2] != "echo $HOME" {
		t.Fatal("native import changed literal values")
	}
}

func TestImportComposeOnlyResolvesAndProtectsValues(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		calls++
		if len(args) != 13 || strings.Join(args, " ") != "docker compose --project-name cove-import --env-file "+os.DevNull+" --file "+args[7]+" --profile * config --format json" {
			t.Fatal("import attempted a mutation", args)
		}
		_, err := io.WriteString(out, `{"name":"cove-import","services":{"app":{"image":"busybox:1.37","environment":{"PASSWORD":"private-import-secret"},"command":["sh","-c","echo $$HOME"],"profiles":["optional"]}},"networks":{"default":{"name":"cove-import_default"}}}`)
		return err
	}
	result, err := e.PrepareImportedCompose(context.Background(), Request{Compose: "services:\n  app:\n    image: busybox:1.37\n", Environment: map[string]string{}})
	if err != nil || calls != 1 || strings.Contains(result.Compose, "private-import-secret") || strings.Contains(result.Compose, "echo") || strings.Contains(result.Compose, "profiles") || strings.Contains(result.Compose, "cove-import_default") {
		t.Fatal("unsafe imported configuration", err)
	}
	found := false
	for _, value := range result.Environment {
		if value == "private-import-secret" {
			found = true
		}
	}
	if !found {
		t.Fatal("private values were lost")
	}
	var document map[string]any
	if json.Unmarshal([]byte(result.Compose), &document) != nil {
		t.Fatal("invalid protected Compose")
	}
}

func TestImportComposeRejectsMissingFilesAndAmbiguousStorage(t *testing.T) {
	for _, content := range []string{
		"include: /private/compose.yaml\nservices: {app: {image: busybox}}",
		"services: {app: {image: busybox, env_file: /private/env}}",
		"services: {app: {image: busybox, build: .}}",
		"services: {app: {image: busybox}}\nvolumes: {data: {}}",
		"services: {app: {image: busybox}}\nsecrets: {password: {environment: HOST_SECRET}}",
	} {
		if validateImportedCompose(content) == nil {
			t.Fatal("unsafe import accepted")
		}
	}
}

func TestImportDoesNotInheritWorkerEnvironmentOrResolveLiteralReferences(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("IMPORT_HOST_SECRET", "host-only-test-value")
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		t.Fatal("missing exported variable reached native resolution")
		return nil
	}
	if _, err = e.PrepareImportedCompose(context.Background(), Request{Compose: "services: {app: {image: '${IMPORT_HOST_SECRET}'}}"}); err == nil {
		t.Fatal("import inherited worker process secrets")
	}
	if _, err = protectImportedCompose(map[string]any{"literal": "{{secret.provider:password}}"}); err == nil {
		t.Fatal("literal imported value could become a live secret reference")
	}
}
