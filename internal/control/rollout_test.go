package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRolloutConfigurationPermissionsCapabilityAndAdmission(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('rollout-node','Node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('rollout-project','Project','default')`)
	config := workloadInput{Name: "App", NodeID: "rollout-node", ProjectID: "rollout-project", Kind: "service", Compose: "services: {}", Rollout: &worker.RolloutOptions{Strategy: "canary", Replicas: map[string]int{"app": 2}}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	path := "/api/workloads/" + id
	var saved workloadInput
	if err := json.Unmarshal(f.call("root", "default", "GET", path, nil, 200).Body.Bytes(), &saved); err != nil || saved.Rollout == nil || saved.Rollout.TimeoutSeconds != 120 || saved.Rollout.CanaryPercent != 10 {
		t.Fatal(saved.Rollout, err)
	}
	saved.Rollout = nil
	saved.ProjectID = ""
	saved.Name = "Renamed"
	f.call("root", "default", "PUT", path, saved, 200)
	if err := json.Unmarshal(f.call("root", "default", "GET", path, nil, 200).Body.Bytes(), &saved); err != nil || saved.Rollout == nil || saved.Rollout.Replicas["app"] != 2 {
		t.Fatal(saved.Rollout, err)
	}
	for _, action := range []string{"deploy", "rollout_promote", "rollout_abort"} {
		f.call("root", "default", "POST", path+"/operations", map[string]string{"action": action}, 409)
	}
	f.sql(`UPDATE nodes SET capabilities='{"rollouts":true}' WHERE id='rollout-node'`)
	viewer := f.user("rollout-viewer", "rollout-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','rollout-viewer','member','[{"scope":"project","scope_id":"rollout-project","role":"viewer"}]')`)
	f.call(viewer, "default", "POST", path+"/operations", map[string]string{"action": "rollout_promote"}, 403)
	f.call(viewer, "default", "PUT", path, saved, 403)
	f.call("root", "default", "POST", path+"/operations", map[string]any{"action": "rollout_promote", "pull_latest": true}, 400)
	op := jsonString(t, f.call("root", "default", "POST", path+"/operations", map[string]string{"action": "rollout_promote"}, 202), "id")
	var payload []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE id=$1", op).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	data, err := unseal(f.s.Key, payload)
	var request worker.Request
	if err != nil || json.Unmarshal(data, &request) != nil || request.Compose != "" || request.Rollout != nil || request.Action != "rollout_promote" {
		t.Fatal(request, err)
	}
	from, until := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	policyPath := "/api/projects/rollout-project/deployment-policy"
	f.call("root", "default", "PUT", policyPath, deploymentPolicy{Approval: true, FreezeFrom: &from, FreezeUntil: &until}, 200)
	if admitted, err := f.s.admitDeployment(context.Background(), op); err != nil || admitted {
		t.Fatal("promote bypassed approval", admitted, err)
	}
	f.call("root", "default", "POST", "/api/operations/"+op+"/approve", map[string]any{}, 200)
	if admitted, err := f.s.admitDeployment(context.Background(), op); err != nil || admitted {
		t.Fatal("promote bypassed freeze", admitted, err)
	}
	f.sql(`UPDATE operations SET state='cancelled' WHERE id=$1`, op)
	abort := jsonString(t, f.call("root", "default", "POST", path+"/operations", map[string]string{"action": "rollout_abort"}, 202), "id")
	if admitted, err := f.s.admitDeployment(context.Background(), abort); err != nil || !admitted {
		t.Fatal("abort blocked by deployment freeze", admitted, err)
	}
	f.sql(`UPDATE operations SET state='succeeded' WHERE id=$1`, abort)
	saved.ProjectID = ""
	saved.Rollout = &worker.RolloutOptions{Strategy: "disabled"}
	f.call("root", "default", "PUT", path, saved, 200)
	if err := json.Unmarshal(f.call("root", "default", "GET", path, nil, 200).Body.Bytes(), &saved); err != nil || saved.Rollout.Strategy != "disabled" {
		t.Fatal(saved.Rollout, err)
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='rollout-node'`)
	deploy := jsonString(t, f.call("root", "default", "POST", path+"/operations", map[string]string{"action": "deploy"}, 202), "id")
	if err := f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE id=$1", deploy).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	data, err = unseal(f.s.Key, payload)
	request = worker.Request{}
	if err != nil || json.Unmarshal(data, &request) != nil || request.Rollout != nil {
		t.Fatal("disabled settings were sent to an older worker", request, err)
	}
	request.Rollout = &worker.RolloutOptions{Strategy: "disabled"}
	if err := f.s.prepareInfrastructure(context.Background(), &request); err != nil || request.Rollout != nil {
		t.Fatal("disabled settings blocked dispatcher", err)
	}
}

func TestRolloutResolutionPreservesActualReleaseMetadata(t *testing.T) {
	for _, phase := range []string{"completed", "aborted"} {
		t.Run(phase, func(t *testing.T) {
			f := newAccountFixture(t)
			f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('rollout-node','Node','localhost',22,'',$1,'ready','{"rollouts":true}')`, []byte{})
			f.sql(`INSERT INTO projects(id,name,team_id) VALUES('rollout-project','Project','default')`)
			id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "App", NodeID: "rollout-node", ProjectID: "rollout-project", Kind: "service", Compose: "services: {}"}, 201), "id")
			f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES('candidate',$1,'deploy','succeeded',$2,'{"rollout":{"phase":"waiting"}}')`, id, []byte{})
			action := "rollout_promote"
			if phase == "aborted" {
				action = "rollout_abort"
			}
			op := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": action}, 202), "id")
			var payload []byte
			if err := f.s.DB.QueryRow(context.Background(), `SELECT payload FROM operations WHERE id=$1`, op).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
				return worker.RPCResponse{Operation: &worker.Operation{ID: r.Request.ID, State: "succeeded", Rollout: &worker.RolloutStatus{Release: "candidate", Phase: phase}}}, nil
			}
			if err := f.s.processOperation(context.Background(), op, "rollout-node", payload); err != nil {
				t.Fatal(err)
			}
			var result string
			if err := f.s.DB.QueryRow(context.Background(), `SELECT result->'rollout'->>'phase' FROM operations WHERE id='candidate'`).Scan(&result); err != nil || result != phase {
				t.Fatal(result, err)
			}
			if body := f.call("root", "default", "GET", "/api/operations/"+op, nil, 200).Body.String(); !strings.Contains(body, phase) {
				t.Fatal(body)
			}
		})
	}
}

func TestRolloutReplicaLimitsCountTowardTeamQuotas(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('replica-node','Node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('replica-project','Project','default')`)
	f.call("root", "default", "PUT", "/api/teams/default/quota", teamQuota{CPUs: 2, MemoryMB: 256}, 200)
	config := workloadInput{Name: "App", NodeID: "replica-node", ProjectID: "replica-project", Compose: "services:\n  app:\n    image: busybox:1.37\n", Rollout: &worker.RolloutOptions{Strategy: "rolling", Replicas: map[string]int{"app": 2}}, Limits: map[string]worker.ServiceLimits{"app": {CPUs: 1, MemoryMB: 128}}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	config.Rollout.Replicas["app"] = 3
	f.call("root", "default", "POST", "/api/workloads", config, 409)
	config.ProjectID = ""
	f.call("root", "default", "PUT", "/api/workloads/"+id, config, 409)
	var saved workloadInput
	if err := json.Unmarshal(f.call("root", "default", "GET", "/api/workloads/"+id, nil, 200).Body.Bytes(), &saved); err != nil || saved.Rollout.Replicas["app"] != 2 {
		t.Fatal("quota rejection changed saved replica count", saved.Rollout, err)
	}
	config.Rollout.Strategy = "disabled"
	f.call("root", "default", "PUT", "/api/workloads/"+id, config, 200)
	var cpu float64
	var memory int64
	if err := f.s.DB.QueryRow(context.Background(), `SELECT configured_cpu,configured_memory_mb FROM workloads WHERE id=$1`, id).Scan(&cpu, &memory); err != nil || cpu != 1 || memory != 128 {
		t.Fatal("disabled overrides affected configured usage", cpu, memory, err)
	}
}
