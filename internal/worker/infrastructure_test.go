package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteStorageValidation(t *testing.T) {
	for _, protocol := range []string{"s3", "webdav", "sftp", "ftps"} {
		s := &StorageLocation{Protocol: protocol, Endpoint: "https://example.com", Path: "backups", Username: "user", Password: "secret", AccessKey: "access", SecretKey: "secret"}
		if protocol == "sftp" || protocol == "ftps" {
			if ValidateStorage(s) == nil {
				t.Fatal("accepted invalid transport", protocol)
			}
			continue
		}
		if err := ValidateStorage(s); err != nil {
			t.Fatal(err)
		}
		s.Endpoint = "http://example.com"
		if ValidateStorage(s) == nil {
			t.Fatal("accepted plaintext storage")
		}
	}
}

func TestS3StorageUsesExistingBucket(t *testing.T) {
	env, err := storageEnvironment(&StorageLocation{Protocol: "s3", Endpoint: "https://storage.example.com", Path: "existing-bucket/backups", AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(env, "\n"), "RCLONE_CONFIG_COVE_NO_CHECK_BUCKET=true") {
		t.Fatal("backup uploads must not require bucket creation permissions")
	}
}

func TestRemoteStorageRoundTrip(t *testing.T) {
	fixture := os.Getenv("COVE_STORAGE_FIXTURE")
	if fixture == "" {
		t.Skip("isolated remote storage fixture not configured")
	}
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var locations []StorageLocation
	if err = json.Unmarshal(data, &locations); err != nil {
		t.Fatal(err)
	}
	for _, location := range locations {
		t.Run(location.Protocol, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			root := filepath.Join(filepath.Dir(fixture), "worker-"+location.Protocol)
			if err = os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			e := &Engine{Root: root, Run: RunCommand}
			if err = e.TestStorage(ctx, &location); err != nil {
				t.Fatal(err)
			}
			request := Request{ID: "fixture" + location.Protocol, Workload: "fixture", Backup: &BackupConfig{Remote: &location, Repository: "rclone:cove:" + location.Path + "/roundtrip", Password: "fixture-encryption-password", KeepLast: 1}}
			if err = e.restic(ctx, request, nil, false, os.Stdout, "init"); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "workloads", "fixture", "backups")
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			marker := "encrypted round trip " + location.Protocol
			if err = os.WriteFile(filepath.Join(dir, request.ID+".source.json"), []byte(marker), 0600); err != nil {
				t.Fatal(err)
			}
			if err = e.restic(ctx, request, nil, false, os.Stdout, "backup", "/cove-manifest.json"); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err = e.restic(ctx, request, nil, false, &output, "dump", "latest", "/cove-manifest.json"); err != nil {
				t.Fatal(err)
			}
			if output.String() != marker {
				t.Fatal("restored content mismatch")
			}
			if err = e.restic(ctx, request, nil, false, io.Discard, "check"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTerminalShell(t *testing.T) {
	if os.Getenv("COVE_STORAGE_FIXTURE") == "" {
		t.Skip("Linux fixture not configured")
	}
	e := &Engine{Root: t.TempDir()}
	opened, err := e.terminal("terminal_open", &TerminalRequest{Columns: 100, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer e.terminal("terminal_close", &TerminalRequest{ID: opened.ID})
	if _, err = e.terminal("terminal_write", &TerminalRequest{ID: opened.ID, Data: []byte("printf 'terminal-%s\\n' accepted\n")}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	offset := int64(0)
	var output strings.Builder
	for time.Now().Before(deadline) {
		result, err := e.terminal("terminal_read", &TerminalRequest{ID: opened.ID, Offset: offset})
		if err != nil {
			t.Fatal(err)
		}
		offset = result.Offset
		output.Write(result.Data)
		if strings.Contains(output.String(), "terminal-accepted") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("shell did not execute input")
}

func TestPrivateRegistryDeployment(t *testing.T) {
	fixture := os.Getenv("COVE_STORAGE_FIXTURE")
	if fixture == "" {
		t.Skip("isolated registry fixture not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	suffix := ""
	if os.Getenv("COVE_TEST_REGISTRY_FAILURE") == "1" {
		suffix = "-failure"
	}
	root := filepath.Join(filepath.Dir(fixture), "registry-worker"+suffix)
	e, err := NewEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	registry := &Registry{Server: "127.0.0.1:22184", Username: "fixture", Password: "fixture-password"}
	if err = e.TestRegistry(ctx, registry); err != nil {
		t.Fatal(err)
	}
	wrong := *registry
	wrong.Password = "wrong-password"
	if e.TestRegistry(ctx, &wrong) == nil {
		t.Fatal("registry accepted incorrect password")
	}
	config, err := registryConfig(root, registry)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(config)
	image := registry.Server + "/cove-fixture:acceptance"
	for _, args := range [][]string{{"docker", "pull", "busybox:1.37"}, {"docker", "tag", "busybox:1.37", image}, {"docker", "push", image}} {
		if err = e.Run(ctx, root, args, []string{"DOCKER_CONFIG=" + config}, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	workload := "registry-" + filepath.Base(filepath.Dir(fixture)) + suffix
	project := "cove-" + workload
	if !strings.HasPrefix(project, "cove-registry-cove-infra-") {
		t.Fatal("registry fixture requires its own random infrastructure project")
	}
	assertRegistryProjectRemoved(t, project)
	if t.Failed() {
		t.FailNow()
	}
	t.Cleanup(func() { cleanupRegistryProject(t, project) })
	request := Request{ID: "registrydeploy", Workload: workload, Action: "deploy", Registry: registry, Kind: "service", Compose: "services:\n  app:\n    image: " + image + "\n    pull_policy: always\n    command: [sleep, '60']\n"}
	if _, err = e.Submit(request); err != nil {
		t.Fatal(err)
	}
	if err = e.perform(ctx, request); err != nil {
		logs, _ := e.Logs(request.ID)
		t.Fatal(err, logs)
	}
	if suffix != "" {
		t.Fatal("intentional registry fixture failure after deployment")
	}
	entries, err := filepath.Glob(filepath.Join(root, "workloads", workload, "registry-*"))
	if err != nil || len(entries) > 0 {
		t.Fatal("deployment credentials were not removed", err)
	}
}

func TestPrivateRegistryDeploymentFailureCleanup(t *testing.T) {
	fixture := os.Getenv("COVE_STORAGE_FIXTURE")
	if fixture == "" {
		t.Skip("isolated registry fixture not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.v", "-test.run=^TestPrivateRegistryDeployment$")
	command.Env = append(os.Environ(), "COVE_TEST_REGISTRY_FAILURE=1")
	output, err := command.CombinedOutput()
	if err == nil || command.ProcessState == nil || command.ProcessState.ExitCode() != 1 || !bytes.Contains(output, []byte("intentional registry fixture failure after deployment")) {
		t.Fatalf("expected the deployed child test to fail: %v\n%s", err, output)
	}
	if bytes.Contains(output, []byte("registry cleanup failed")) {
		t.Fatalf("failed child could not clean its project: %s", output)
	}
	assertRegistryProjectRemoved(t, "cove-registry-"+filepath.Base(filepath.Dir(fixture))+"-failure")
	t.Log("Real child test exited 1 after deployment; its containers and network are absent")
}

func registryProjectResources(t *testing.T, ctx context.Context, project, kind string) []string {
	t.Helper()
	args := []string{"ps", "-aq"}
	if kind == "network" {
		args = []string{"network", "ls", "-q"}
	}
	output, err := exec.CommandContext(ctx, "docker", append(args, "--filter", "label=com.docker.compose.project="+project)...).CombinedOutput()
	if err != nil {
		t.Fatalf("registry cleanup failed listing %s: %v: %s", kind, err, output)
	}
	return strings.Fields(string(output))
}

func assertRegistryProjectRemoved(t *testing.T, project string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, kind := range []string{"container", "network"} {
		if remaining := registryProjectResources(t, ctx, project, kind); len(remaining) != 0 {
			t.Errorf("registry cleanup failed: project %s retains %s: %v", project, kind, remaining)
		}
	}
}

func cleanupRegistryProject(t *testing.T, project string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, kind := range []string{"container", "network"} {
		for _, id := range registryProjectResources(t, ctx, project, kind) {
			field := ".Config.Labels"
			if kind == "network" {
				field = ".Labels"
			}
			output, err := exec.CommandContext(ctx, "docker", kind, "inspect", "--format", "{{index "+field+" \"com.docker.compose.project\"}}", id).CombinedOutput()
			if err != nil || strings.TrimSpace(string(output)) != project {
				t.Errorf("registry cleanup failed: ownership could not be confirmed for %s %s", kind, id)
				continue
			}
			args := []string{"rm", "-f", "-v", id}
			if kind == "network" {
				args = []string{"network", "rm", id}
			}
			if output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
				t.Errorf("registry cleanup failed removing %s: %v: %s", kind, err, output)
			}
		}
	}
	assertRegistryProjectRemoved(t, project)
}
