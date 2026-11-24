package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestRegistryWebhookDocker(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("COVE_TEST_REGISTRY_HOOK_DOCKER") != "1" {
		t.Skip("requires an explicitly selected isolated Linux Docker daemon and local PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("fixture Docker command failed: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	server := fmt.Sprintf("127.0.0.1:%d", port)
	f := newRegistryHookFixture(t, "distribution", server)
	reference := server + "/team/app:stable"
	compose := "services:\n  app:\n    image: " + reference + "\n    pull_policy: always\n    command: [sh, -c, 'cat /version > /tmp/seen; sleep 300']\n    healthcheck:\n      test: [CMD, 'true']\n      interval: 1s\n      timeout: 1s\n      retries: 10\n"
	guarded := true
	f.call("root", "default", "PUT", f.path, workloadInput{Name: "App", NodeID: "node", Kind: "service", Compose: compose, GuardedDeploy: &guarded}, 200)
	input := map[string]any{"provider": "distribution", "enabled": false, "filters": []registryWebhookFilter{{Repository: "team/app", Tag: "stable"}}}
	f.call("root", "default", "PUT", f.path+"/registry-webhook", input, 200)
	engine, err := worker.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.s.workerRPC = func(ctx context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "node" {
			return worker.RPCResponse{}, fmt.Errorf("foreign fixture node")
		}
		return engine.RPC(ctx, r), nil
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- engine.Serve(workerCtx) }()
	registry := "cove-registry-hook-" + f.id
	seeds := []string{}
	registryCreated := false
	t.Cleanup(func() {
		stopWorker()
		if err := <-workerDone; err != nil {
			t.Error(err)
		}
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		containers, err := exec.CommandContext(cleanup, "docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+f.id).Output()
		if err != nil {
			t.Error(err)
		}
		for _, container := range strings.Fields(string(containers)) {
			project, err := exec.CommandContext(cleanup, "docker", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, container).Output()
			if err != nil || strings.TrimSpace(string(project)) != "cove-"+f.id {
				t.Error("cleanup container ownership mismatch", err)
				continue
			}
			if out, err := exec.CommandContext(cleanup, "docker", "stop", "--time", "0", container).CombinedOutput(); err != nil {
				t.Error(err, string(out))
			}
		}
		if err := engine.RemoveWorkload(cleanup, f.id); err != nil {
			t.Error(err)
		}
		if registryCreated {
			seeds = append(seeds, registry)
		}
		for _, name := range seeds {
			out, err := exec.CommandContext(cleanup, "docker", "inspect", "--format", `{{index .Config.Labels "cove.registry-webhook-test"}}`, name).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != f.id {
				t.Error("fixture ownership mismatch", name, err)
				continue
			}
			if out, err := exec.CommandContext(cleanup, "docker", "rm", "-f", "-v", name).CombinedOutput(); err != nil {
				t.Error(err, string(out))
			}
		}
	})
	httpListener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewUnstartedServer(f.handler)
	httpServer.Listener.Close()
	httpServer.Listener = httpListener
	httpServer.Start()
	defer httpServer.Close()
	bridge := docker("network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
	endpoint := fmt.Sprintf("http://%s:%d%s", bridge, httpListener.Addr().(*net.TCPAddr).Port, f.endpoint)
	registryDir := t.TempDir()
	hash, err := bcrypt.GenerateFromPassword([]byte("generated-fixture-only"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(registryDir, "htpasswd"), append([]byte("fixture:"), append(hash, '\n')...), 0600); err != nil {
		t.Fatal(err)
	}
	configuration := fmt.Sprintf("version: 0.1\nlog:\n  level: error\nstorage:\n  filesystem:\n    rootdirectory: /var/lib/registry\nhttp:\n  addr: :5000\nauth:\n  htpasswd:\n    realm: fixture\n    path: /config/htpasswd\nnotifications:\n  endpoints:\n    - name: cove-fixture\n      url: %s\n      headers:\n        Authorization: [\"Bearer %s\"]\n      timeout: 5s\n      threshold: 5\n      backoff: 100ms\n", endpoint, f.token)
	if err = os.WriteFile(filepath.Join(registryDir, "config.yml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	docker("run", "--pull", "never", "-d", "--name", registry, "--label", "cove.registry-webhook-test="+f.id, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "256m", "--cpus", "1", "--pids-limit", "64", "--tmpfs", "/var/lib/registry:rw,size=128m", "-v", registryDir+":/config:ro", "-p", fmt.Sprintf("127.0.0.1:%d:5000", port), "registry:2.8.3", "/config/config.yml")
	registryCreated = true
	pushConfig := t.TempDir()
	// Only the fixture pusher gets this config. The worker must receive credentials through normal sealed infrastructure.
	credential := fmt.Sprintf(`{"auths":{%q:{"username":"fixture","password":"generated-fixture-only"}}}`, server)
	if err = os.WriteFile(filepath.Join(pushConfig, "config.json"), []byte(credential), 0600); err != nil {
		t.Fatal(err)
	}
	wait := func(description string, check func() bool) {
		t.Helper()
		for !check() {
			select {
			case <-ctx.Done():
				t.Fatal(description, "timed out")
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	push := func(version string) {
		t.Helper()
		name := "cove-registry-seed-" + f.id + "-" + version
		docker("run", "--pull", "never", "--name", name, "--label", "cove.registry-webhook-test="+f.id, "busybox:1.37", "sh", "-c", "echo "+version+" > /version")
		seeds = append(seeds, name)
		docker("commit", name, reference)
		docker("--config", pushConfig, "push", reference)
	}
	push("first")
	wait("initial native notification", func() bool { return f.status().Received != nil })
	if f.status().Pending != nil {
		t.Fatal("disabled setup notification queued a deployment")
	}
	initial := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]string{"action": "deploy"}, 202), "id")
	settled := func(id string) {
		t.Helper()
		wait("real worker deployment", func() bool {
			if err := f.s.dispatchOperations(ctx); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := f.s.DB.QueryRow(ctx, "SELECT state FROM operations WHERE id=$1", id).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state == "failed" || state == "cancelled" || state == "interrupted" {
				logs, _ := engine.Logs(id)
				var result []byte
				if err := f.s.DB.QueryRow(ctx, "SELECT result FROM operations WHERE id=$1", id).Scan(&result); err != nil {
					t.Fatal(err)
				}
				t.Fatal("real deployment failed", state, string(result), logs)
			}
			return state == "succeeded"
		})
	}
	settled(initial)
	container := docker("ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+f.id)
	if container == "" || docker("exec", container, "cat", "/tmp/seen") != "first" {
		t.Fatal("initial image was not running")
	}
	initialImage := docker("inspect", "--format", "{{.Image}}", container)
	input["enabled"] = true
	f.call("root", "default", "PUT", f.path+"/registry-webhook", input, 200)
	f.call("root", "default", "PUT", "/api/projects/project/deployment-policy", deploymentPolicy{Approval: true}, 200)
	push("second")
	wait("native push requested redeploy", func() bool { return f.status().Pending != nil })
	if err := f.s.dispatchRegistryWebhooks(ctx); err != nil {
		t.Fatal(err)
	}
	last := f.status().LastOperation
	if last == nil {
		t.Fatal("native notification did not create ordinary deploy")
	}
	if err := f.s.dispatchOperations(ctx); err != nil {
		t.Fatal(err)
	}
	if docker("exec", container, "cat", "/tmp/seen") != "first" {
		t.Fatal("unapproved push changed the running image")
	}
	var admitted bool
	if err := f.s.DB.QueryRow(ctx, "SELECT admitted_at IS NOT NULL FROM operations WHERE id=$1", last.ID).Scan(&admitted); err != nil || admitted {
		t.Fatal("native push bypassed approval", err)
	}
	f.call("root", "default", "POST", "/api/operations/"+last.ID+"/approve", map[string]any{}, 200)
	settled(last.ID)
	container = docker("ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+f.id)
	if docker("exec", container, "cat", "/tmp/seen") != "second" || docker("inspect", "--format", "{{.Image}}", container) == initialImage {
		t.Fatal("worker did not pull and deploy the changed registry image")
	}
	var payload []byte
	if err := f.s.DB.QueryRow(ctx, "SELECT payload FROM operations WHERE id=$1", last.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	raw, err := unseal(f.s.Key, payload)
	var request worker.Request
	if err != nil || json.Unmarshal(raw, &request) != nil || !request.PullLatest {
		t.Fatal("native event did not use ordinary pull-and-redeploy")
	}
	logs, _ := engine.Logs(last.ID)
	if strings.Contains(logs, "generated-fixture-only") || strings.Contains(logs, f.token) {
		t.Fatal("credentials leaked into operation logs")
	}
	f.call("root", "default", "DELETE", f.path+"/registry-webhook", nil, 200)
}
