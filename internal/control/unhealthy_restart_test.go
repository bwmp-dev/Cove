package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestUnhealthyRestartPolicyAndAdmission(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('health-node','Node','localhost',22,'',$1,'ready','{"unhealthy_restart":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('health-project','Project','default')`)
	input := workloadInput{Name: "App", NodeID: "health-node", ProjectID: "health-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	path := "/api/workloads/" + id + "/restart-policy"
	if !strings.Contains(f.call("root", "default", "GET", path, nil, 200).Body.String(), `"enabled":false`) {
		t.Fatal("health restarts defaulted on")
	}
	viewer := f.user("health-viewer", "health-viewer@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','health-viewer','member','[{"scope":"project","scope_id":"health-project","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", path, nil, 200)
	f.call(viewer, "default", "PUT", path, map[string]bool{"enabled": true}, 403)
	f.call("root", "default", "PUT", path, map[string]bool{}, 400)
	f.call("root", "default", "PUT", path, map[string]bool{"enabled": true}, 200)
	ctx := context.Background()
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "health-node" || r.Method != "container_health" || r.ID != id {
			t.Errorf("unexpected health read %s %+v", node, r)
		}
		return worker.RPCResponse{Containers: []worker.Container{{ID: strings.Repeat("a", 64), State: "running", Health: "unhealthy"}}}, nil
	}
	count := func(want int) {
		t.Helper()
		var got int
		if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM operations WHERE workload_id=$1 AND action='restart_unhealthy'`, id).Scan(&got); err != nil || got != want {
			t.Fatal(got, want, err)
		}
	}
	check := func() {
		t.Helper()
		if err := f.s.scheduleUnhealthyRestarts(ctx); err != nil {
			t.Fatal(err)
		}
	}
	due := func() {
		f.sql(`UPDATE workloads SET last_unhealthy_restart_at=now()-interval '6 minutes',next_unhealthy_check_at=now() WHERE id=$1`, id)
	}
	check()
	count(1)
	check()
	count(1)
	var payload []byte
	if err := f.s.DB.QueryRow(ctx, `SELECT payload FROM operations WHERE workload_id=$1 AND action='restart_unhealthy'`, id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	raw, err := unseal(f.s.Key, payload)
	var request worker.Request
	if err != nil || json.Unmarshal(raw, &request) != nil || request.Action != "restart_unhealthy" || request.Workload != id || worker.ValidateRequest(request) != nil {
		t.Fatal(request, err)
	}
	for want := 2; want <= 3; want++ {
		f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id=$1`, id)
		due()
		check()
		count(want)
	}
	f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id=$1`, id)
	due()
	check()
	count(3)
	f.sql(`UPDATE operations SET created_at=now()-interval '2 hours' WHERE id=(SELECT id FROM operations WHERE workload_id=$1 ORDER BY created_at LIMIT 1)`, id)
	due()
	check()
	count(4)
	f.call("root", "default", "PUT", path, map[string]bool{"enabled": false}, 200)
	var cancelled bool
	if err := f.s.DB.QueryRow(ctx, `SELECT cancel_requested FROM operations WHERE workload_id=$1 AND state='queued'`, id).Scan(&cancelled); err != nil || !cancelled {
		t.Fatal("pending automatic restart survived disabling", err)
	}
	f.sql(`UPDATE operations SET state='succeeded',created_at=now()-interval '2 hours' WHERE workload_id=$1`, id)
	f.call("root", "default", "PUT", path, map[string]bool{"enabled": true}, 200)
	f.sql(`UPDATE workloads SET manually_stopped=true WHERE id=$1`, id)
	due()
	check()
	count(4)
	f.sql(`UPDATE workloads SET manually_stopped=false WHERE id=$1`, id)
	sealed, _ := f.s.sealJSON(worker.Request{ID: "held", Workload: id, Action: "deploy"})
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,state,result) VALUES('held',$1,'deploy',$2,'succeeded','{"rollout":{"phase":"waiting"}}')`, id, sealed)
	due()
	check()
	count(4)
	f.sql(`UPDATE operations SET result='{"rollout":{"phase":"aborted"}}' WHERE id='held'`)
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{}, errors.New("private diagnostic")
	}
	due()
	check()
	count(4)
	data := f.call("root", "default", "GET", path, nil, 200).Body.String()
	if !strings.Contains(data, "Could not inspect Docker health") || strings.Contains(data, "private diagnostic") {
		t.Fatal(data)
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='health-node'`)
	f.call("root", "default", "PUT", path, map[string]bool{"enabled": true}, 409)
	f.call("root", "default", "PUT", path, map[string]bool{"enabled": false}, 200)
	f.sql(`UPDATE workloads SET kind='job' WHERE id=$1`, id)
	f.call("root", "default", "PUT", path, map[string]bool{"enabled": true}, 400)
	input.Name = "Deleted"
	deleted := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	f.sql(`DELETE FROM workloads WHERE id=$1`, deleted)
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{Containers: []worker.Container{{State: "running", Health: "unhealthy"}}}, nil
	}
	if err := f.s.checkUnhealthyRestart(ctx, deleted, "health-node"); err != nil {
		t.Fatal("deletion during a health read stopped the monitor", err)
	}
}
