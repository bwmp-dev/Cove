package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDockerWorkerEndToEnd(t *testing.T) {
	if os.Getenv("COVE_E2E") != "1" {
		t.Skip("COVE_E2E=1 required; test creates disposable Docker resources")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	command := func(input string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Dir = root
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	name := "cove-e2e-" + resourceID()
	image := name + ":test"
	workload := "e2e-" + resourceID()
	if out, err := command("", "build", "-t", image, "-f", "test/worker/Dockerfile", "."); err != nil {
		t.Fatalf("worker fixture build: %v\n%s", err, out)
	}
	defer func() {
		if out, err := command("", "image", "rm", image); err != nil {
			t.Errorf("fixture image cleanup: %v %s", err, out)
		}
	}()
	if out, err := command("", "run", "-d", "--name", name, "--label", "cove.test=true", "-p", "127.0.0.1::22", "-v", "/var/run/docker.sock:/var/run/docker.sock", image); err != nil {
		t.Fatalf("fixture start: %v %s", err, out)
	}
	defer func() {
		if out, err := command("", "rm", "-f", "-v", name); err != nil {
			t.Errorf("fixture cleanup: %v %s", err, out)
		}
	}()
	pub, seed, err := keyPair()
	if err != nil {
		t.Fatal(err)
	}
	auth := fmt.Sprintf("restrict,command=\"/usr/local/bin/cove-worker rpc\" %s\n", pub)
	if out, err := command(auth, "exec", "-i", name, "sh", "-c", "cat > /var/lib/cove/.ssh/authorized_keys && chown cove:cove /var/lib/cove/.ssh/authorized_keys && chmod 600 /var/lib/cove/.ssh/authorized_keys"); err != nil {
		t.Fatalf("fixture identity: %v %s", err, out)
	}
	host, err := command("", "exec", name, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		t.Fatal(err)
	}
	published, err := command("", "port", name, "22/tcp")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimSpace(published), ":")
	port, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		t.Fatal(err)
	}
	call := func(request worker.RPCRequest) (worker.RPCResponse, error) {
		return callSSH(context.Background(), "127.0.0.1", port, host, seed, request)
	}
	for attempt := 0; attempt < 30; attempt++ {
		p, err := probeSSH(context.Background(), "127.0.0.1", port, host, seed)
		if err == nil && p.Ready {
			break
		}
		if attempt == 29 {
			logs, _ := command("", "logs", name)
			t.Fatalf("worker not ready: %v %s", err, logs)
		}
		time.Sleep(time.Second)
	}
	compose := "services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'while true; do sleep 10; done']\n    healthcheck:\n      test: [CMD, 'true']\n      interval: 1s\n"
	defer func() {
		out, err := command("", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+workload)
		if err != nil {
			t.Errorf("workload discovery cleanup: %v", err)
			return
		}
		for _, id := range strings.Fields(out) {
			if out, err = command("", "rm", "-f", "-v", id); err != nil {
				t.Errorf("workload cleanup: %v %s", err, out)
			}
		}
		out, err = command("", "network", "ls", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)
		if err != nil {
			t.Errorf("network discovery cleanup: %v", err)
			return
		}
		for _, id := range strings.Fields(out) {
			if out, err = command("", "network", "rm", id); err != nil {
				t.Errorf("network cleanup: %v %s", err, out)
			}
		}
		out, err = command("", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference=cove-release/"+workload+":*")
		if err != nil {
			t.Errorf("release image discovery: %v", err)
			return
		}
		for _, tag := range strings.Fields(out) {
			if out, err = command("", "image", "rm", tag); err != nil {
				t.Errorf("release image cleanup: %v %s", err, out)
			}
		}
	}()
	request := worker.Request{ID: resourceID(), Workload: workload, Action: "deploy", Compose: compose}
	response, err := call(worker.RPCRequest{Method: "submit", Request: &request})
	if err != nil || response.Operation == nil {
		t.Fatalf("submit: %v", err)
	}
	wait := func(id, expected string) {
		for attempt := 0; attempt < 90; attempt++ {
			response, err := call(worker.RPCRequest{Method: "status", ID: id})
			if err != nil {
				t.Fatal(err)
			}
			if response.Operation.State == expected {
				return
			}
			if response.Operation.State == "failed" || response.Operation.State == "interrupted" || response.Operation.State == "succeeded" {
				logs, _ := call(worker.RPCRequest{Method: "logs", ID: id})
				t.Fatalf("operation failed: %s", logs.Logs)
			}
			time.Sleep(time.Second)
		}
		t.Fatal("operation timed out")
	}
	wait(request.ID, "succeeded")
	runtime, err := call(worker.RPCRequest{Method: "containers", ID: workload})
	if err != nil || len(runtime.Containers) != 1 || runtime.Containers[0].State != "running" {
		t.Fatalf("runtime inspection failed: %v %+v", err, runtime)
	}
	first, err := command("", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)
	if err != nil || strings.TrimSpace(first) == "" {
		t.Fatal("container not running")
	}
	if out, err := command("", "restart", name); err != nil {
		t.Fatalf("restart worker: %v %s", err, out)
	}
	published, err = command("", "port", name, "22/tcp")
	if err != nil {
		t.Fatal(err)
	}
	parts = strings.Split(strings.TrimSpace(published), ":")
	port, err = strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 30; attempt++ {
		response, err = call(worker.RPCRequest{Method: "submit", Request: &request})
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil || response.Operation == nil || response.Operation.State != "succeeded" {
		logs, _ := command("", "logs", name)
		t.Fatalf("operation not retained after restart: %v %+v %s", err, response, logs)
	}
	second, err := command("", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)
	if err != nil || !bytes.Equal([]byte(first), []byte(second)) {
		t.Fatal("worker restart/replay changed deployed container")
	}
	rollback := worker.Request{ID: resourceID(), Workload: workload, Action: "rollback", Release: request.ID}
	if _, err = call(worker.RPCRequest{Method: "submit", Request: &rollback}); err != nil {
		t.Fatal(err)
	}
	wait(rollback.ID, "succeeded")
	runtime, err = call(worker.RPCRequest{Method: "containers", ID: workload})
	if err != nil || len(runtime.Containers) != 1 || runtime.Containers[0].State != "running" {
		t.Fatal("rollback did not restore running service", err)
	}
	stop := worker.Request{ID: resourceID(), Workload: workload, Action: "stop"}
	if _, err = call(worker.RPCRequest{Method: "submit", Request: &stop}); err != nil {
		t.Fatal(err)
	}
	wait(stop.ID, "succeeded")
	running, err := command("", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)
	if err != nil || strings.TrimSpace(running) != "" {
		t.Fatal("container still running after stop")
	}
	job := worker.Request{ID: resourceID(), Workload: workload, Action: "deploy", Kind: "job", JobService: "app", Compose: "services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'echo job-completed; exit 0']\n"}
	if _, err = call(worker.RPCRequest{Method: "submit", Request: &job}); err != nil {
		t.Fatal(err)
	}
	wait(job.ID, "succeeded")
	jobLogs, err := call(worker.RPCRequest{Method: "container_logs", ID: workload})
	if err != nil || !strings.Contains(jobLogs.Logs, "job-completed") {
		t.Fatalf("container logs failed: %v %s", err, jobLogs.Logs)
	}
	job.ID = resourceID()
	job.Compose = strings.Replace(job.Compose, "exit 0", "exit 7", 1)
	if _, err = call(worker.RPCRequest{Method: "submit", Request: &job}); err != nil {
		t.Fatal(err)
	}
	wait(job.ID, "failed")
	job.ID = resourceID()
	job.Compose = "services:\n  app:\n    image: busybox:1.37\n    command: [sh, -c, 'sleep 300']\n"
	if _, err = call(worker.RPCRequest{Method: "submit", Request: &job}); err != nil {
		t.Fatal(err)
	}
	wait(job.ID, "running")
	if _, err = call(worker.RPCRequest{Method: "cancel", Request: &job}); err != nil {
		t.Fatal(err)
	}
	wait(job.ID, "cancelled")
	response, err = call(worker.RPCRequest{Method: "submit", Request: &job})
	if err != nil || response.Operation == nil || response.Operation.State != "cancelled" {
		t.Fatal("cancelled job replayed", err)
	}
}
