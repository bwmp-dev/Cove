package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestManagedRegistryControllerProvisionMaintenanceAndIsolation(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('registry-node','Registry','localhost',22,'',$1,'ready','{"managed_registry":true,"file_secrets":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('registry-project','Registry project','default')`)
	in := managedRegistryInput{Name: "Built images", Node: "registry-node", Project: "registry-project", Hostname: "registry.example.test", Policy: &worker.ManagedRegistryRetention{KeepPerRepository: 5, MinimumAgeDays: 7}}
	viewer := f.user("registry-viewer", "registry-viewer@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','registry-viewer','member','[{"scope":"project","scope_id":"registry-project","role":"viewer"}]')`)
	f.call(viewer, "default", "POST", "/api/managed-registries", in, 403)
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='registry-node'`)
	f.call("root", "default", "POST", "/api/managed-registries", in, 409)
	f.sql(`UPDATE nodes SET capabilities='{"managed_registry":true,"file_secrets":true}' WHERE id='registry-node'`)
	f.sql(`INSERT INTO team_quotas(team_id,cpus) VALUES('default',0.5)`)
	f.call("root", "default", "POST", "/api/managed-registries", in, 409)
	f.sql(`DELETE FROM team_quotas WHERE team_id='default'`)
	created := f.call("root", "default", "POST", "/api/managed-registries", in, 202)
	id := jsonString(t, created, "id")
	operation := jsonString(t, created, "operation_id")
	if strings.Contains(created.Body.String(), "password") || strings.Contains(created.Body.String(), "hash") {
		t.Fatal("credentials exposed by provisioning")
	}
	var encrypted []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT credentials FROM managed_registries WHERE id=$1`, id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	var connection connectionConfig
	if err := f.s.unsealJSON(encrypted, &connection); err != nil || connection.Registry == nil || connection.Registry.Password == "" {
		t.Fatal("credentials not sealed", err)
	}
	list := f.call(viewer, "default", "GET", "/api/managed-registries", nil, 200)
	for _, secret := range []string{connection.Registry.Password, "$2a$", "COVE_REGISTRY_AUTH", "credentials"} {
		if strings.Contains(list.Body.String(), secret) {
			t.Fatal("private registry metadata returned", secret)
		}
	}
	if !strings.Contains(list.Body.String(), `"hard_cap_verified":false`) {
		t.Fatal("unsupported hard cap not explicit")
	}
	f.call("root", "default", "PUT", "/api/workloads/"+id, map[string]any{"name": "Hijacked"}, 409)
	f.call("root", "default", "DELETE", "/api/workloads/"+id, nil, 409)
	in.Hostname = "another.example.test"
	f.call("root", "default", "POST", "/api/managed-registries", in, 409)
	var payload []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT payload FROM operations WHERE id=$1`, operation).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var request worker.Request
	if err := f.s.unsealJSON(payload, &request); err != nil || request.Action != "deploy" || len(request.FileSecrets) != 1 || strings.Contains(request.Compose, connection.Registry.Password) {
		t.Fatal("invalid provisioning request", err)
	}
	f.sql(`UPDATE operations SET state='succeeded' WHERE id=$1`, operation)
	probeError := true
	pending := true
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "registry-node" {
			t.Fatal(node)
		}
		switch r.Method {
		case "registry_test", "managed_registry_verify":
			if probeError {
				return worker.RPCResponse{}, errors.New("fixture TLS refusal")
			}
			if r.Registry == nil || r.Registry.Password != connection.Registry.Password {
				t.Fatal("generated connection not verified")
			}
			return worker.RPCResponse{}, nil
		case "managed_registry_references":
			return worker.RPCResponse{RegistryReferences: []string{}}, nil
		case "status", "managed_registry_recover":
			return worker.RPCResponse{Operation: &worker.Operation{ID: r.ID, State: "cancelled", RegistryRecoveryPending: pending}}, nil
		default:
			t.Fatal(r.Method)
			return worker.RPCResponse{}, errors.New("unsupported fixture call")
		}
	}
	if err := f.s.reconcileManagedRegistries(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT state FROM managed_registries WHERE id=$1`, id).Scan(&state); err != nil || state != "failed" {
		t.Fatal("unverified TLS registry activated", state, err)
	}
	retry := f.call("root", "default", "POST", "/api/managed-registries/"+id+"/retry", nil, 202)
	probeError = false
	f.sql(`UPDATE operations SET state='succeeded' WHERE id=$1`, jsonString(t, retry, "operation_id"))
	if err := f.s.reconcileManagedRegistries(context.Background()); err != nil {
		t.Fatal(err)
	}
	var connectionID string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT state,connection_id FROM managed_registries WHERE id=$1`, id).Scan(&state, &connectionID); err != nil || state != "ready" || connectionID == "" {
		t.Fatal(state, err)
	}
	f.call("root", "default", "DELETE", "/api/registries/"+connectionID, nil, 409)
	consumer := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Consumer", NodeID: "registry-node", ProjectID: "registry-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"}, 201), "id")
	f.call("root", "default", "PUT", "/api/workloads/"+consumer+"/registry", map[string]string{"registry_id": connectionID}, 200)
	gc := f.call("root", "default", "POST", "/api/managed-registries/"+id+"/gc", nil, 202)
	if _, err := f.s.DB.Exec(context.Background(), `INSERT INTO operations(id,workload_id,action,payload) VALUES('blocked-publisher',$1,'build',$2)`, consumer, []byte{}); err == nil {
		t.Fatal("publisher admitted during maintenance")
	}
	f.sql(`INSERT INTO teams(id,name) VALUES('registry-other','Other registry team')`)
	foreign := f.call("root", "registry-other", "GET", "/api/managed-registries", nil, 200)
	if strings.Contains(foreign.Body.String(), id) {
		t.Fatal("registry metadata crossed teams")
	}
	f.call("root", "registry-other", "POST", "/api/managed-registries/"+id+"/gc", nil, 404)
	f.sql(`UPDATE operations SET state='cancelled' WHERE id=$1`, jsonString(t, gc, "operation_id"))
	if err := f.s.reconcileManagedRegistries(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(context.Background(), `SELECT state FROM managed_registries WHERE id=$1`, id).Scan(&state); err != nil || state != "maintenance" {
		t.Fatal("unsettled worker recovery released maintenance", state, err)
	}
	pending = false
	if err := f.s.reconcileManagedRegistries(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(context.Background(), `SELECT state FROM managed_registries WHERE id=$1`, id).Scan(&state); err != nil || state != "ready" {
		t.Fatal(state, err)
	}
	f.call("root", "default", "PUT", "/api/managed-registries/"+id, map[string]any{"retention": worker.ManagedRegistryRetention{KeepPerRepository: 0, MinimumAgeDays: 7}}, 400)
	metadata := f.call("root", "default", "GET", "/api/registries", nil, 200)
	if strings.Contains(metadata.Body.String(), connection.Registry.Password) {
		t.Fatal("connection list exposed generated password")
	}
	var decoded any
	if json.Unmarshal(metadata.Body.Bytes(), &decoded) != nil {
		t.Fatal("invalid connection metadata")
	}
}

func TestManagedRegistryMaintenanceRetainsScheduledJobTick(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('registry-node','Registry','localhost',22,'',$1,'ready','{"managed_registry":true,"file_secrets":true,"job_runs":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('registry-project','Registry project','default')`)
	created := f.call("root", "default", "POST", "/api/managed-registries", managedRegistryInput{Name: "Registry", Node: "registry-node", Project: "registry-project", Hostname: "registry.example.test"}, 202)
	registry := jsonString(t, created, "id")
	f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id=$1`, registry)
	job := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Scheduled task", NodeID: "registry-node", ProjectID: "registry-project", Kind: "job", JobService: "task", Compose: "services:\n  task:\n    image: busybox:1.37\n    command: ['true']\n"}, 201), "id")
	f.call("root", "default", "PUT", "/api/workloads/"+job+"/schedule", map[string]any{"cron": "* * * * *", "overlap_policy": "skip"}, 200)
	due := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
	f.sql(`UPDATE workloads SET next_job_at=$2 WHERE id=$1`, job, due)
	f.sql(`UPDATE managed_registries SET state='maintenance' WHERE id=$1`, registry)
	if err := f.s.scheduleJobs(context.Background()); !managedRegistryBusy(err) {
		t.Fatal("scheduled publisher was not held by registry maintenance", err)
	}
	var next time.Time
	var count int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT next_job_at,(SELECT count(*) FROM operations WHERE workload_id=w.id) FROM workloads w WHERE id=$1`, job).Scan(&next, &count); err != nil || !next.Equal(due) || count != 0 {
		t.Fatal("maintenance consumed a due scheduled run", next, count, err)
	}
	f.sql(`UPDATE managed_registries SET state='ready' WHERE id=$1`, registry)
	if err := f.s.scheduleJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM operations WHERE workload_id=$1 AND action='deploy' AND state='queued'`, job).Scan(&count); err != nil || count != 1 {
		t.Fatal("due job did not resume after registry maintenance", count, err)
	}
}

func TestManagedRegistryFailedRemovalRestoresItsPriorState(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('registry-node','Registry','localhost',22,'',$1,'ready','{"managed_registry":true,"file_secrets":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('registry-project','Registry project','default')`)
	in := managedRegistryInput{Name: "Built images", Node: "registry-node", Project: "registry-project", Hostname: "registry.example.test"}
	created := f.call("root", "default", "POST", "/api/managed-registries", in, 202)
	id := jsonString(t, created, "id")
	f.sql(`UPDATE operations SET state='failed' WHERE workload_id=$1`, id)
	f.sql(`UPDATE managed_registries SET state='failed' WHERE id=$1`, id)
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method != "managed_registry_recover" {
			t.Fatal(r.Method)
		}
		return worker.RPCResponse{Operation: &worker.Operation{ID: r.ID, State: "failed"}}, nil
	}
	for _, prior := range []string{"failed", "removed"} {
		f.sql(`UPDATE managed_registries SET state=$2,data_retained=true WHERE id=$1`, id, prior)
		removed := f.call("root", "default", "DELETE", "/api/managed-registries/"+id, map[string]bool{"delete_data": true}, 202)
		f.sql(`UPDATE operations SET state='failed' WHERE id=$1`, jsonString(t, removed, "operation_id"))
		if err := f.s.reconcileManagedRegistries(context.Background()); err != nil {
			t.Fatal(err)
		}
		var state string
		if err := f.s.DB.QueryRow(context.Background(), `SELECT state FROM managed_registries WHERE id=$1`, id).Scan(&state); err != nil || state != prior {
			t.Fatal("failed removal invented a ready registry", prior, state, err)
		}
	}
	f.call("root", "default", "DELETE", "/api/managed-registries/"+id+"/record", nil, 409)
	f.sql(`UPDATE managed_registries SET data_retained=false WHERE id=$1`, id)
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method != "remove_workload" {
			t.Fatal(r.Method)
		}
		return worker.RPCResponse{}, errors.New("fixture worker unavailable")
	}
	f.call("root", "default", "DELETE", "/api/managed-registries/"+id+"/record", nil, 409)
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method != "remove_workload" {
			t.Fatal(r.Method)
		}
		return worker.RPCResponse{}, nil
	}
	f.call("root", "default", "DELETE", "/api/managed-registries/"+id+"/record", nil, 200)
	var exists bool
	if err := f.s.DB.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM workloads WHERE id=$1) OR EXISTS(SELECT 1 FROM managed_registries WHERE id=$1)`, id).Scan(&exists); err != nil || exists {
		t.Fatal("purged record kept workload/node references", exists, err)
	}
}
