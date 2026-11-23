package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestIdlePolicyQueuesImmutableChangesAndCommitsOnlySuccess(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('idle-node','Node','localhost',22,'',$1,'ready','{"idle_sleep":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('idle-project','Project','default')`)
	input := workloadInput{Name: "App", NodeID: "idle-node", ProjectID: "idle-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	path := "/api/workloads/" + id + "/idle-policy"
	if body := f.call("root", "default", "GET", path, nil, 200).Body.String(); !strings.Contains(body, `"enabled":false`) {
		t.Fatal(body)
	}
	viewer := f.user("idle-viewer", "idle-viewer@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','idle-viewer','member','[{"scope":"project","scope_id":"idle-project","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", path, nil, 200)
	policy := worker.IdlePolicy{Enabled: true, AfterSeconds: 60, RequestDriven: true}
	f.call(viewer, "default", "PUT", path, policy, 403)
	f.call("root", "default", "PUT", path, map[string]bool{}, 400)
	f.call("root", "default", "PUT", path, worker.IdlePolicy{Enabled: true, AfterSeconds: 60}, 400)
	f.call("root", "default", "PUT", path, worker.IdlePolicy{Enabled: true, AfterSeconds: 59, RequestDriven: true}, 400)
	f.call("root", "default", "PUT", path, worker.IdlePolicy{Enabled: true, AfterSeconds: 86401, RequestDriven: true}, 400)
	op := jsonString(t, f.call("root", "default", "PUT", path, policy, 202), "id")
	f.call("root", "default", "PUT", path, policy, 409)
	if body := f.call("root", "default", "GET", path, nil, 200).Body.String(); !strings.Contains(body, `"enabled":false`) {
		t.Fatal("desired policy changed before worker confirmation", body)
	}
	var payload []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE id=$1", op).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	data, err := unseal(f.s.Key, payload)
	if err != nil {
		t.Fatal(err)
	}
	var request worker.Request
	if err = json.Unmarshal(data, &request); err != nil || request.Idle == nil || *request.Idle != policy || request.Action != "idle_policy" {
		t.Fatal(request, err)
	}

	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{}, &RemoteError{Message: "acknowledgement unavailable"}
	}
	if err = f.s.processOperation(context.Background(), op, "idle-node", payload); err != nil {
		t.Fatal(err)
	}
	var pendingState string
	if err = f.s.DB.QueryRow(context.Background(), "SELECT state FROM operations WHERE id=$1", op).Scan(&pendingState); err != nil || pendingState != "queued" {
		t.Fatal("ambiguous reply released the policy lock", pendingState, err)
	}
	f.call("root", "default", "PUT", path, policy, 409)
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "idle-node" || r.Request == nil || r.Request.ID != op || r.Request.Idle == nil || *r.Request.Idle != policy {
			t.Fatalf("unexpected worker request %+v", r)
		}
		return worker.RPCResponse{Operation: &worker.Operation{ID: op, Workload: id, Action: "idle_policy", State: "succeeded"}}, nil
	}
	if err = f.s.processOperation(context.Background(), op, "idle-node", payload); err != nil {
		t.Fatal(err)
	}
	if body := f.call("root", "default", "GET", path, nil, 200).Body.String(); !strings.Contains(body, `"enabled":true`) {
		t.Fatal(body)
	}
	if err = f.s.processOperation(context.Background(), op, "idle-node", payload); err != nil {
		t.Fatal("replay failed", err)
	}

	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{Suspension: &worker.SuspensionStatus{Reason: "idle", Phase: "paused"}}, nil
	}
	f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "deploy"}, 409)
	f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "rollback", "release": "released"}, 409)
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{}, errors.New("worker unavailable")
	}
	f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "deploy"}, 502)
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{Suspension: &worker.SuspensionStatus{Reason: "manual_stop", Phase: "paused"}}, nil
	}
	repairID := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "deploy"}, 202), "id")
	f.sql(`UPDATE operations SET state='cancelled' WHERE id=$1`, repairID)
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='idle-node'`)
	f.call("root", "default", "PUT", path, policy, 409)
	f.sql(`UPDATE nodes SET capabilities='{"idle_sleep":true}',state='offline' WHERE id='idle-node'`)
	f.call("root", "default", "PUT", path, policy, 409)
	f.sql(`UPDATE nodes SET state='ready' WHERE id='idle-node'`)
	f.sql(`UPDATE workloads SET manually_stopped=true WHERE id=$1`, id)
	f.call("root", "default", "PUT", path, policy, 409)
	policy.Enabled = false
	disable := jsonString(t, f.call("root", "default", "PUT", path, policy, 202), "id")
	f.sql(`UPDATE operations SET state='failed' WHERE id=$1`, disable)
	if body := f.call("root", "default", "GET", path, nil, 200).Body.String(); !strings.Contains(body, `"enabled":true`) {
		t.Fatal("failed operation changed policy", body)
	}

	f.sql(`UPDATE workloads SET manually_stopped=false WHERE id=$1`, id)
	f.sql(`UPDATE nodes SET capabilities='{"idle_sleep":true,"private_links":true}' WHERE id='idle-node'`)
	input.Name = "Peer"
	peer := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	connection := map[string]any{"source_id": peer, "source_service": "app", "target_id": id, "target_service": "app", "port": 8080, "alias": "app.internal"}
	f.call("root", "default", "POST", "/api/private-connections", connection, 409)
	f.sql(`UPDATE workloads SET idle_policy='{"enabled":false}' WHERE id=$1`, id)
	connectionID := jsonString(t, f.call("root", "default", "POST", "/api/private-connections", connection, 201), "id")
	policy.Enabled = true
	f.call("root", "default", "PUT", path, policy, 409)
	f.sql(`DELETE FROM resource_connections WHERE id=$1`, connectionID)
	policy.Enabled = false
	f.sql(`UPDATE workloads SET kind='job' WHERE id=$1`, id)
	f.call("root", "default", "PUT", path, policy, 400)
}

func TestIdlePolicyIntentionalStopsDoNotRaiseHealthAlerts(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('idle-health-node','Node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('idle-health-project','Project','default')`)
	input := workloadInput{Name: "App", NodeID: "idle-health-node", ProjectID: "idle-health-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	payload, err := f.s.sealJSON(worker.Request{ID: "released", Workload: id, Action: "deploy", Compose: input.Compose})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,state) VALUES('released',$1,'deploy',$2,'succeeded')`, id, payload)
	for _, reason := range []string{"idle", "manual_stop", ""} {
		f.sql(`UPDATE workloads SET health_checked_at=NULL WHERE id=$1`, id)
		f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
			response := worker.RPCResponse{Containers: []worker.Container{{State: "exited", Status: "Exited (137)"}}}
			if reason != "" {
				response.Suspension = &worker.SuspensionStatus{Reason: reason, Phase: "paused"}
			}
			return response, nil
		}
		if err = f.s.checkResourceHealth(context.Background()); err != nil {
			t.Fatal(err)
		}
		var count int
		if err = f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM health_alerts WHERE subject=$1 AND kind='resource_unhealthy'`, id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 0
		if reason == "" {
			want = 1
		}
		if count != want {
			t.Fatal("worker intent was confused with an unexpected failure", reason, count, want)
		}
	}
}
