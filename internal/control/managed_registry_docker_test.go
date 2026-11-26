package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestManagedRegistryRealConnectionDocker(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("COVE_TEST_MANAGED_REGISTRY_DOCKER") != "1" {
		t.Skip("owned PostgreSQL and nested daemon required")
	}
	binary := os.Getenv("COVE_TEST_WORKER_BINARY")
	if binary == "" {
		t.Fatal("owned worker binary required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		var output bytes.Buffer
		if err := worker.RunCommand(ctx, "/tmp", append([]string{"docker"}, args...), nil, &output); err != nil {
			t.Fatal(args, err, output.String())
		}
		return strings.TrimSpace(output.String())
	}
	for _, kind := range []string{"container", "network", "volume"} {
		if exec.CommandContext(ctx, "docker", kind, "inspect", "cove-ingress").Run() == nil {
			t.Fatal("owned fixture daemon already contains ingress")
		}
	}
	docker("network", "create", "--label", "cove.managed=ingress", "cove-ingress")
	docker("volume", "create", "--label", "cove.managed=ingress", "cove-ingress")
	docker("create", "--name", "cove-ingress", "--network", "cove-ingress", "--label", "cove.managed=ingress", "--mount", "type=volume,src=cove-ingress,dst=/data", "--entrypoint", "sh", "traefik:v3.7.19", "-ec", `mkdir -p /data/dynamic; exec traefik "$@"`, "traefik", "--entrypoints.web.address=:80", "--entrypoints.websecure.address=:443", "--entrypoints.traefik.address=127.0.0.1:8080", "--api.insecure=true", "--api.dashboard=false", "--providers.file.directory=/data/dynamic", "--providers.file.watch=true", "--certificatesresolvers.cove.acme.storage=/data/traefik-acme.json", "--certificatesresolvers.cove.acme.httpchallenge.entrypoint=web", "--certificatesresolvers.cove.acme.caserver=https://127.0.0.1:9/directory")
	docker("start", "cove-ingress")
	log, err := os.CreateTemp(t.TempDir(), "worker.log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	daemon := exec.CommandContext(ctx, binary, "daemon")
	daemon.Stdout = log
	daemon.Stderr = log
	if err = daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Process.Kill(); _ = daemon.Wait() })
	for i := 0; i < 100; i++ {
		if _, err = os.Stat("/run/cove/worker.sock"); err == nil {
			break
		}
		if i == 99 {
			t.Fatal("owned worker socket did not become ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	f := newAccountFixture(t)
	f.s.workerRPC = func(ctx context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "registry-node" {
			return worker.RPCResponse{}, errors.New("fixture node is unavailable")
		}
		data, err := json.Marshal(r)
		if err != nil {
			return worker.RPCResponse{}, err
		}
		var out bytes.Buffer
		if err = worker.Proxy(ctx, "/run/cove/worker.sock", bytes.NewReader(data), &out); err != nil {
			return worker.RPCResponse{}, err
		}
		var response worker.RPCResponse
		if err = json.Unmarshal(out.Bytes(), &response); err != nil {
			return response, err
		}
		if response.Error != "" {
			return response, &RemoteError{Message: response.Error}
		}
		return response, nil
	}
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('registry-node','Registry','localhost',22,'',$1,'ready','{"managed_registry":true,"file_secrets":true,"route_protocol":6,"image_lifecycle":true,"infrastructure":true}')`, []byte{})
	actualProbe, err := f.s.nodeRPC(ctx, "registry-node", worker.RPCRequest{Method: "probe"})
	if err != nil || actualProbe.Probe == nil || !actualProbe.Probe.Ready || !actualProbe.Probe.ManagedRegistry || !actualProbe.Probe.FileSecrets {
		t.Fatal("real worker is not ready for managed registry acceptance", err)
	}
	capabilities, err := json.Marshal(actualProbe.Probe)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE nodes SET capabilities=$1 WHERE id='registry-node'`, capabilities)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('registry-project','Registry project','default')`)
	host := "registry.example.test"
	cert, key := certificateFixture(t, host)
	proxy := docker("inspect", "--format", `{{(index .NetworkSettings.Networks "cove-ingress").IPAddress}}`, "cove-ingress")
	if err = os.MkdirAll(filepath.Join("/etc/docker/certs.d", host), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join("/etc/docker/certs.d", host, "ca.crt"), []byte(cert), 0600); err != nil {
		t.Fatal(err)
	}
	hosts, err := os.OpenFile("/etc/hosts", os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = hosts.WriteString("\n" + proxy + " " + host + "\n")
	_ = hosts.Close()
	if err != nil {
		t.Fatal(err)
	}
	created := f.call("root", "default", "POST", "/api/managed-registries", managedRegistryInput{Name: "Real generated connection", Node: "registry-node", Project: "registry-project", Hostname: host}, 202)
	id := jsonString(t, created, "id")
	settled := func(id string) {
		t.Helper()
		until := time.Now().Add(90 * time.Second)
		for time.Now().Before(until) {
			if err = f.s.dispatchOperations(ctx); err != nil {
				t.Fatal(err)
			}
			var state string
			if err = f.s.DB.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1`, id).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state == "succeeded" {
				return
			}
			if state != "queued" && state != "running" {
				var message string
				_ = f.s.DB.QueryRow(ctx, `SELECT COALESCE(result->>'error','') FROM operations WHERE id=$1`, id).Scan(&message)
				raw, _ := f.s.nodeRPC(ctx, "registry-node", worker.RPCRequest{Method: "logs", ID: id})
				t.Fatal("real operation failed", state, message, raw.Logs)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("real operation timed out")
	}
	settled(jsonString(t, created, "operation_id"))
	if err = f.s.reconcileManagedRegistries(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = f.s.DB.QueryRow(ctx, `SELECT state FROM managed_registries WHERE id=$1`, id).Scan(&state); err != nil || state != "failed" {
		t.Fatal("untrusted default certificate activated registry", state, err)
	}
	upload := f.call("root", "default", "POST", "/api/workloads/"+id+"/certificates", map[string]string{"name": "Fixture trusted TLS", "certificate_pem": cert, "private_key_pem": key}, 201)
	certificateID := jsonString(t, upload, "id")
	f.call("root", "default", "PUT", "/api/managed-registries/"+id, map[string]any{"retention": worker.ManagedRegistryRetention{KeepPerRepository: 10, MinimumAgeDays: 30}, "certificate_id": certificateID}, 200)
	retry := f.call("root", "default", "POST", "/api/managed-registries/"+id+"/retry", nil, 202)
	settled(jsonString(t, retry, "operation_id"))
	if err = f.s.reconcileManagedRegistries(ctx); err != nil {
		t.Fatal(err)
	}
	var connectionID string
	if err = f.s.DB.QueryRow(ctx, `SELECT state,connection_id FROM managed_registries WHERE id=$1`, id).Scan(&state, &connectionID); err != nil || state != "ready" {
		t.Fatal("trusted generated connection did not activate", state, err)
	}
	connection, err := f.s.readConnection(ctx, connectionID, "default", "registry")
	if err != nil || connection.Registry == nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/managed-registries", "/api/registries", "/api/workloads/" + id} {
		response := f.call("root", "default", "GET", path, nil, 200)
		if strings.Contains(response.Body.String(), connection.Registry.Password) || strings.Contains(response.Body.String(), "$2a$") {
			t.Fatal("generated credentials escaped API")
		}
	}
	auth := t.TempDir()
	credentials := base64.StdEncoding.EncodeToString([]byte(connection.Registry.Username + ":" + connection.Registry.Password))
	raw, _ := json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"auth": credentials}}})
	if err = os.WriteFile(filepath.Join(auth, "config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	docker("tag", "busybox:1.37", host+"/team/app:fixture")
	if err = worker.RunCommand(ctx, "/tmp", []string{"docker", "--config", auth, "push", host + "/team/app:fixture"}, nil, io.Discard); err != nil {
		t.Fatal("fixture publisher using generated sealed connection failed", err)
	}
	t.Run("legacy-release-reference-inventory", func(t *testing.T) {
		testManagedRegistryLegacyWorkerInventory(t, f, id, host)
	})
	consumer := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Managed registry consumer", NodeID: "registry-node", ProjectID: "registry-project", Kind: "service", Compose: "services:\n  app:\n    image: " + host + "/team/app:fixture\n    pull_policy: always\n    command: [sleep, '86400']\n"}, 201), "id")
	f.call("root", "default", "PUT", "/api/workloads/"+consumer+"/registry", map[string]string{"registry_id": connectionID}, 200)
	op := f.call("root", "default", "POST", "/api/workloads/"+consumer+"/operations", map[string]string{"action": "deploy"}, 202)
	settled(jsonString(t, op, "id"))
	containers := docker("ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+consumer)
	if containers == "" {
		t.Fatal("generated registry connection did not pull/deploy image")
	}
	gc := f.call("root", "default", "POST", "/api/managed-registries/"+id+"/gc", nil, 202)
	settled(jsonString(t, gc, "operation_id"))
	if err = f.s.reconcileManagedRegistries(ctx); err != nil {
		t.Fatal(err)
	}
	if docker("ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+consumer) != containers {
		t.Fatal("registry GC changed consumer")
	}
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,host_key,state) VALUES('offline-node','Offline runner','localhost',22,'',$1,'pinned-fixture-identity','unreachable')`, []byte{})
	addRegistryRemovalWorkload(t, f, "offline-consumer", "offline-node")
	f.call("root", "default", "POST", "/api/managed-registries/"+id+"/gc", nil, 409)
	f.call("root", "default", "DELETE", "/api/managed-registries/"+id, map[string]bool{"delete_data": false}, 409)
	f.call("root", "default", "PUT", "/api/workloads/"+consumer+"/registry", map[string]string{"registry_id": ""}, 200)
	f.call("root", "default", "DELETE", "/api/managed-registries/"+id, map[string]bool{"delete_data": true}, 409)
	f.call("root", "default", "DELETE", "/api/managed-registries/"+id, map[string]any{"delete_data": true, "acknowledge_unavailable_nodes": []string{"offline-node"}}, 409)
	remove := f.call("root", "default", "DELETE", "/api/managed-registries/"+id, map[string]bool{"delete_data": false}, 202)
	settled(jsonString(t, remove, "operation_id"))
	if err = f.s.reconcileManagedRegistries(ctx); err != nil {
		t.Fatal(err)
	}
	stop := f.call("root", "default", "POST", "/api/workloads/"+consumer+"/operations", map[string]string{"action": "stop"}, 202)
	settled(jsonString(t, stop, "id"))
	f.call("root", "default", "DELETE", "/api/workloads/"+consumer, nil, 200)
	if docker("volume", "ls", "-q", "--filter", "label=dev.cove.managed-registry="+id) == "" {
		t.Fatal("keep-data removal lost the registry volume")
	}
	preview := f.call("root", "default", "GET", "/api/managed-registries/"+id+"/removal-inventory", nil, 200)
	if !strings.Contains(preview.Body.String(), "Offline runner") {
		t.Fatal("destructive preflight omitted unavailable node name")
	}
	f.call("root", "default", "DELETE", "/api/managed-registries/"+id, map[string]bool{"delete_data": true}, 409)
	purge := f.call("root", "default", "DELETE", "/api/managed-registries/"+id, map[string]any{"delete_data": true, "acknowledge_unavailable_nodes": []string{"offline-node"}}, 202)
	settled(jsonString(t, purge, "operation_id"))
	if err = f.s.reconcileManagedRegistries(ctx); err != nil {
		t.Fatal(err)
	}
	if docker("volume", "ls", "-q", "--filter", "label=dev.cove.managed-registry="+id) != "" {
		t.Fatal("explicit registry purge left data volume")
	}
	if _, err = f.s.readConnection(ctx, connectionID, "default", "registry"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("generated connection retained after removal", err)
	}
	f.call("root", "default", "DELETE", "/api/managed-registries/"+id+"/record", nil, 200)
	var exists bool
	if err = f.s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workloads WHERE id=$1) OR EXISTS(SELECT 1 FROM managed_registries WHERE id=$1)`, id).Scan(&exists); err != nil || exists {
		t.Fatal("purged registry record retained node/project references", exists, err)
	}
}
