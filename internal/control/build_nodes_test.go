package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestBuildNodesSelectionSubmissionRecoveryAndCleanup(t *testing.T) {
	f := newAccountFixture(t)
	for _, id := range []string{"runtime", "builder", "foreign", "different"} {
		f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES($1,$1,'localhost',22,'',$2,'ready','{"build_publish":true,"remote_builds":true,"os":"linux","arch":"amd64"}')`, id, []byte{})
	}
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('default','builder'),('default','different')`)
	f.sql(`UPDATE nodes SET capabilities=jsonb_set(capabilities,'{arch}','"arm64"') WHERE id='different'`)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('build-project','Build project','default')`)
	config := workloadInput{Name: "App", NodeID: "runtime", ProjectID: "build-project", Kind: "service", Compose: "services:\n  app:\n    build: .\n", Environment: map[string]string{"BUILD_VALUE": "frozen-fixture"}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	path := "/api/workloads/" + id
	registry := connectionConfig{Name: "Build registry", Registry: &worker.Registry{Server: "registry.example.test", Username: "fixture", Password: "first-fixture-password"}}
	rid := jsonString(t, f.call("root", "default", "POST", "/api/registries", registry, 201), "id")
	f.call("root", "default", "PUT", path+"/registry", map[string]string{"registry_id": rid}, 200)
	viewer := f.user("build-node-viewer", "build-node-viewer@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','build-node-viewer','member','[{"scope":"project","scope_id":"build-project","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", path+"/build-node", nil, 200)
	f.call(viewer, "default", "PUT", path+"/build-node", map[string]string{"node_id": "builder"}, 403)
	f.call("root", "default", "PUT", path+"/build-node", map[string]string{"node_id": "foreign"}, 409)
	f.call("root", "default", "PUT", path+"/build-node", map[string]string{"node_id": "different"}, 409)
	f.call("root", "default", "PUT", path+"/build-node", map[string]string{"node_id": "builder"}, 200)
	input := map[string]any{"action": "build", "build": map[string]any{"repository": "team/app", "no_cache": true}}
	op := jsonString(t, f.call("root", "default", "POST", path+"/operations", input, 202), "id")
	var selected string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT execution_node_id FROM operations WHERE id=$1`, op).Scan(&selected); err != nil || selected != "builder" {
		t.Fatal(selected, err)
	}
	f.call("root", "default", "PUT", path+"/build-node", map[string]string{"node_id": ""}, 409)
	var first worker.Request
	// Compare the wire payload hashed by the worker; omitted empty maps need not retain their Go allocation.
	sameInputs := func(a, b worker.Request) bool {
		t.Helper()
		before, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		after, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		return bytes.Equal(before, after)
	}
	submits := 0
	cleanupFails := true
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		switch r.Method {
		case "status":
			return worker.RPCResponse{}, errors.New("fixture unknown status")
		case "submit_build_v2":
			if node != "builder" || r.Request == nil {
				t.Fatal("wrong build dispatch", node, r.Method)
			}
			submits++
			if submits == 1 {
				first = *r.Request
				if first.Registry == nil || first.Registry.Password != "first-fixture-password" {
					t.Fatal("credentials missing")
				}
				return worker.RPCResponse{}, errors.New("fixture lost acknowledgement")
			}
			if !sameInputs(first, *r.Request) {
				t.Fatal("build inputs changed across retry")
			}
			return worker.RPCResponse{Operation: &worker.Operation{ID: op, Workload: id, Action: "build", State: "running"}}, nil
		case "cancel_build":
			if node != "builder" || !sameInputs(first, *r.Request) {
				t.Fatal("cancellation changed worker or request")
			}
			return worker.RPCResponse{Operation: &worker.Operation{ID: op, Workload: id, Action: "build", State: "cancelled"}}, nil
		case "logs":
			if node != "builder" {
				t.Fatal("logs routed to runtime")
			}
			return worker.RPCResponse{Logs: "builder log"}, nil
		case "remove_workload":
			if node != "runtime" {
				t.Fatal("runtime cleanup routed to builder")
			}
			return worker.RPCResponse{}, nil
		case "retire_build":
			if node != "builder" || r.Request == nil || r.Request.ID != op {
				t.Fatal("wrong build cleanup")
			}
			if cleanupFails {
				return worker.RPCResponse{}, errors.New("fixture builder unavailable")
			}
			return worker.RPCResponse{}, nil
		default:
			t.Fatal("unexpected RPC", r.Method)
			return worker.RPCResponse{}, errors.New("unexpected RPC")
		}
	}
	if err := f.s.dispatchOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry.Registry.Password = "changed-fixture-password"
	encrypted, err := f.s.sealJSON(registry)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE infrastructure_connections SET configuration=$2 WHERE id=$1`, rid, encrypted)
	f.sql(`DELETE FROM team_nodes WHERE node_id='builder'`)
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='builder'`)
	if err = f.s.dispatchOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if submits != 2 {
		t.Fatal("retry did not reach original executor")
	}
	logs := f.call("root", "default", "GET", "/api/operations/"+op+"/logs", nil, 200)
	if !strings.Contains(logs.Body.String(), "builder log") {
		t.Fatal(logs.Body.String())
	}
	get := f.call("root", "default", "GET", "/api/operations/"+op, nil, 200)
	if jsonString(t, get, "execution_node_id") != "builder" {
		t.Fatal(get.Body.String())
	}
	f.call("root", "default", "POST", "/api/operations/"+op+"/cancel", nil, 202)
	if err = f.s.dispatchOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.call("root", "default", "PUT", path+"/build-node", map[string]string{"node_id": ""}, 200)
	f.call("root", "default", "GET", "/api/operations/"+op+"/logs", nil, 200)
	f.call("root", "default", "DELETE", "/api/nodes/builder", nil, 409)
	if _, err = f.s.DB.Exec(context.Background(), `UPDATE operations SET execution_node_id='runtime' WHERE id=$1`, op); err == nil {
		t.Fatal("execution node mutated")
	}
	if _, err = f.s.DB.Exec(context.Background(), `UPDATE operations SET payload=$2 WHERE id=$1`, op, []byte("different")); err == nil {
		t.Fatal("prepared build mutated")
	}
	refused := f.call("root", "default", "DELETE", path, nil, 409)
	if !strings.Contains(refused.Body.String(), "runtime cleanup finished") || !strings.Contains(refused.Body.String(), "resource record is kept") {
		t.Fatal(refused.Body.String())
	}
	f.call("root", "default", "GET", path, nil, 200)
	cleanupFails = false
	f.call("root", "default", "DELETE", path, nil, 200)
	var exists bool
	if err = f.s.DB.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM operations WHERE id=$1)`, op).Scan(&exists); err != nil || exists {
		t.Fatal("build history retained after confirmed cleanup", exists, err)
	}
}

func TestBuildNodesPreUpgradeUncertaintyNeverRecreatesInputs(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('old-builder','Old','localhost',22,'',$1,'ready','{}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('old-project','Old project','default')`)
	config := workloadInput{Name: "Old build", NodeID: "old-builder", ProjectID: "old-project", Kind: "service", Compose: "services:\n  app:\n    build: .\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	f.sql(`UPDATE nodes SET state='failed' WHERE id='old-builder'`)
	r := worker.Request{ID: "old-build-operation", Workload: id, Action: "build", Kind: "service", Compose: config.Compose, Build: &worker.BuildOptions{Repository: "team/app"}}
	payload, err := f.s.sealJSON(r)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,execution_node_id,build_legacy) VALUES($1,$2,'build',$3,'old-builder',true)`, r.ID, id, payload)
	available := false
	state := "running"
	f.s.workerRPC = func(_ context.Context, node string, rpc worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "old-builder" {
			t.Fatal("legacy operation rerouted", node)
		}
		if rpc.Method == "cancel_build" || rpc.Method == "cancel" {
			return worker.RPCResponse{}, &RemoteError{Message: "old worker cannot match cancellation inputs"}
		}
		if rpc.Method != "status" {
			t.Fatal("legacy inputs recreated", rpc.Method)
		}
		if !available {
			return worker.RPCResponse{}, errors.New("fixture worker unavailable")
		}
		return worker.RPCResponse{Operation: &worker.Operation{ID: r.ID, Workload: id, Action: "build", State: state}}, nil
	}
	if err = f.s.dispatchOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	var actual string
	if err = f.s.DB.QueryRow(context.Background(), `SELECT state FROM operations WHERE id=$1`, r.ID).Scan(&actual); err != nil || actual != "queued" {
		t.Fatal("uncertain legacy operation released", actual, err)
	}
	available = true
	f.call("root", "default", "POST", "/api/operations/"+r.ID+"/cancel", nil, 202)
	if err = f.s.dispatchOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow(context.Background(), `SELECT state FROM operations WHERE id=$1`, r.ID).Scan(&actual); err != nil || actual != "running" {
		t.Fatal("unconfirmed cancellation released", actual, err)
	}
	state = "interrupted"
	if err = f.s.dispatchOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow(context.Background(), `SELECT state FROM operations WHERE id=$1`, r.ID).Scan(&actual); err != nil || actual != "interrupted" {
		t.Fatal(actual, err)
	}
}

func TestBuildNodesMigrationMarksOnlyExistingActiveBuilds(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('migration-builder','Migration','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('migration-project','Migration project','default')`)
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Migration", NodeID: "migration-builder", ProjectID: "migration-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"}, 201), "id")
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES('pre-upgrade-build',$1,'build',$2)`, id, []byte{})
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES('historical-build',$1,'build','succeeded',$2)`, id, []byte{})
	f.sql(`ALTER TABLE operations DROP COLUMN build_legacy`)
	schema, err := assets.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DB.Exec(context.Background(), string(schema)); err != nil {
		t.Fatal("upgrade migration", err)
	}
	var legacy bool
	var execution string
	if err = f.s.DB.QueryRow(context.Background(), `SELECT build_legacy,execution_node_id FROM operations WHERE id='pre-upgrade-build'`).Scan(&legacy, &execution); err != nil || !legacy || execution != "migration-builder" {
		t.Fatal(legacy, execution, err)
	}
	history := f.call("root", "default", "GET", "/api/operations/historical-build", nil, 200)
	if jsonString(t, history, "execution_node_id") != "" {
		t.Fatal("historical worker was invented", history.Body.String())
	}
	f.sql(`UPDATE operations SET state='cancelled' WHERE id='pre-upgrade-build'`)
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES('post-upgrade-build',$1,'build',$2)`, id, []byte{})
	if _, err = f.s.DB.Exec(context.Background(), string(schema)); err != nil {
		t.Fatal("repeated migration", err)
	}
	if err = f.s.DB.QueryRow(context.Background(), `SELECT build_legacy FROM operations WHERE id='post-upgrade-build'`).Scan(&legacy); err != nil || legacy {
		t.Fatal("schema reload reclassified a new build", legacy, err)
	}
}
