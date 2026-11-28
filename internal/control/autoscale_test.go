package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"testing"
	"time"
)

func TestCPUScalingRequiresConfirmedReplicaOutcome(t *testing.T) {
	request := &worker.CPUScaleOptions{Generation: "previous", Replicas: map[string]int{"app": 3}}
	for _, result := range []*worker.Operation{nil, {}, {Scale: request}, {Scale: request, Rollout: &worker.RolloutStatus{Phase: "waiting", Replicas: map[string]int{"app": 3}}}, {Scale: request, Rollout: &worker.RolloutStatus{Phase: "completed", Replicas: map[string]int{"app": 2}}}} {
		if confirmedCPUScale(request, result) {
			t.Fatal("unconfirmed outcome accepted", result)
		}
	}
	if !confirmedCPUScale(request, &worker.Operation{Scale: request, Rollout: &worker.RolloutStatus{Phase: "completed", Replicas: map[string]int{"app": 3}}}) {
		t.Fatal("confirmed scale rejected")
	}
}

func TestCPUScalingPolicyReservesQuotaAndProtectsConfiguration(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('cpu-node','Node','localhost',22,'',$1,'ready','{"cpu_autoscale":true,"rollouts":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('cpu-project','Project','default')`)
	config := workloadInput{Name: "App", NodeID: "cpu-node", ProjectID: "cpu-project", Kind: "service", Compose: "services: {}", Routes: []worker.Route{{Domain: "cpu.example.test", Service: "app", Port: 8080}}, Rollout: &worker.RolloutOptions{Strategy: "rolling", Replicas: map[string]int{"app": 2}}, Limits: map[string]worker.ServiceLimits{"app": {CPUs: 0.5, MemoryMB: 64}}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	path := "/api/workloads/" + id
	f.s.workerRPC = func(ctx context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{CPUScale: &worker.CPUScaleObservation{SampledAt: time.Now().UTC(), Generation: "release", Replicas: map[string]int{"app": 2}, CPUPercent: map[string]float64{"app": 85}}}, nil
	}
	policy := worker.AutoscalePolicy{Enabled: true, Services: map[string]worker.AutoscaleService{"app": {MinReplicas: 1, MaxReplicas: 4, TargetCPUPercent: 70}}}
	f.call("root", "default", "PUT", path+"/autoscale-policy", map[string]any{"services": policy.Services}, 400)
	f.call("root", "default", "PUT", "/api/teams/default/quota", teamQuota{CPUs: 1.5}, 200)
	f.call("root", "default", "PUT", path+"/autoscale-policy", policy, 409)
	f.call("root", "default", "PUT", "/api/teams/default/quota", teamQuota{CPUs: 2}, 200)
	f.call("root", "default", "PUT", path+"/autoscale-policy", policy, 200)
	var cpu float64
	var memory int64
	if err := f.s.DB.QueryRow(context.Background(), `SELECT configured_cpu,configured_memory_mb FROM workloads WHERE id=$1`, id).Scan(&cpu, &memory); err != nil || cpu != 2 || memory != 256 {
		t.Fatal(cpu, memory, err)
	}
	config.ProjectID = ""
	config.Name = "Renamed"
	f.call("root", "default", "PUT", path, config, 200)
	config.Rollout = &worker.RolloutOptions{Strategy: "disabled"}
	f.call("root", "default", "PUT", path, config, 400)
	config.Rollout = &worker.RolloutOptions{Strategy: "rolling", Replicas: map[string]int{"app": 2}}
	config.Limits["app"] = worker.ServiceLimits{}
	f.call("root", "default", "PUT", path, config, 400)
	viewer := f.user("cpu-viewer", "cpu-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','cpu-viewer','member','[{"scope":"project","scope_id":"cpu-project","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", path+"/autoscale-policy", nil, 200)
	f.call(viewer, "default", "PUT", path+"/autoscale-policy", policy, 403)
	policy.Enabled = false
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='cpu-node'`)
	f.call("root", "default", "PUT", path+"/autoscale-policy", policy, 200)
	if err := f.s.DB.QueryRow(context.Background(), `SELECT configured_cpu FROM workloads WHERE id=$1`, id).Scan(&cpu); err != nil || cpu != 1 {
		t.Fatal(cpu, err)
	}
}

func TestCPUScalingQueuesImmutableDeployWithApprovalAndCommitsReplicaCounts(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('cpu-node','Node','localhost',22,'',$1,'ready','{"cpu_autoscale":true,"rollouts":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('cpu-project','Project','default')`)
	config := workloadInput{Name: "App", NodeID: "cpu-node", ProjectID: "cpu-project", Kind: "service", Compose: "services: {}", Routes: []worker.Route{{Domain: "cpu.example.test", Service: "app", Port: 8080}}, Rollout: &worker.RolloutOptions{Strategy: "rolling", Replicas: map[string]int{"app": 2}}, Limits: map[string]worker.ServiceLimits{"app": {CPUs: 0.5, MemoryMB: 64}}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	f.s.workerRPC = func(ctx context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{CPUScale: &worker.CPUScaleObservation{SampledAt: time.Now().UTC(), Generation: "release", Replicas: map[string]int{"app": 2}, CPUPercent: map[string]float64{"app": 85}}}, nil
	}
	policy := worker.AutoscalePolicy{Enabled: true, Services: map[string]worker.AutoscaleService{"app": {MinReplicas: 1, MaxReplicas: 4, TargetCPUPercent: 70}}}
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/autoscale-policy", policy, 200)
	seed := []worker.CPUScaleObservation{}
	for _, ago := range []time.Duration{2 * time.Minute, time.Minute} {
		seed = append(seed, worker.CPUScaleObservation{SampledAt: time.Now().UTC().Add(-ago), Generation: "release", Replicas: map[string]int{"app": 2}, CPUPercent: map[string]float64{"app": 90}})
	}
	raw, _ := json.Marshal(seed)
	f.sql(`UPDATE workloads SET cpu_samples=$2 WHERE id=$1`, id, raw)
	f.call("root", "default", "PUT", "/api/projects/cpu-project/deployment-policy", deploymentPolicy{Approval: true}, 200)
	if err := f.s.checkCPUScale(ctx, id); err != nil {
		t.Fatal(err)
	}
	var op string
	var payload []byte
	if err := f.s.DB.QueryRow(ctx, `SELECT id,payload FROM operations WHERE workload_id=$1`, id).Scan(&op, &payload); err != nil {
		t.Fatal(err)
	}
	data, err := unseal(f.s.Key, payload)
	var request worker.Request
	if err != nil || json.Unmarshal(data, &request) != nil || request.Action != "deploy" || request.Scale == nil || request.Scale.Replicas["app"] != 3 || request.Scale.Generation != "release" {
		t.Fatal(request, err)
	}
	if admitted, err := f.s.admitDeployment(ctx, op); err != nil || admitted {
		t.Fatal("CPU scaling bypassed approval", admitted, err)
	}
	f.call("root", "default", "POST", "/api/operations/"+op+"/approve", map[string]any{}, 200)
	from, until := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	f.call("root", "default", "PUT", "/api/projects/cpu-project/deployment-policy", deploymentPolicy{Approval: true, FreezeFrom: &from, FreezeUntil: &until}, 200)
	if admitted, err := f.s.admitDeployment(ctx, op); err != nil || admitted {
		t.Fatal("CPU scaling bypassed freeze", admitted, err)
	}
	f.call("root", "default", "PUT", "/api/projects/cpu-project/deployment-policy", deploymentPolicy{Approval: true}, 200)
	if admitted, err := f.s.admitDeployment(ctx, op); err != nil || !admitted {
		t.Fatal(admitted, err)
	}
	readCPU := f.s.workerRPC
	f.s.workerRPC = func(ctx context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method == "cpu_scale_sample" {
			return readCPU(ctx, node, r)
		}
		if r.Method != "submit_cpu_scale" || r.Request == nil || r.Request.Scale == nil {
			t.Fatalf("unsafe scaling submission: %+v", r)
		}
		return worker.RPCResponse{Operation: &worker.Operation{ID: op, Action: "deploy", State: "succeeded", Scale: r.Request.Scale, Rollout: &worker.RolloutStatus{Phase: "completed", Replicas: r.Request.Scale.Replicas}}}, nil
	}
	if err := f.s.processOperation(ctx, op, "cpu-node", payload); err != nil {
		t.Fatal(err)
	}
	var encrypted []byte
	if err := f.s.DB.QueryRow(ctx, `SELECT configuration FROM workloads WHERE id=$1`, id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	data, err = unseal(f.s.Key, encrypted)
	var saved workloadInput
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.Rollout.Replicas["app"] != 3 || !saved.Autoscale.Enabled {
		t.Fatal(saved, err)
	}
	if err := f.s.checkCPUScale(ctx, id); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM operations WHERE workload_id=$1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatal("cooldown queued another deploy", count, err)
	}
	f.sql(`UPDATE workloads SET manually_stopped=true,last_cpu_scale_at=NULL,cpu_samples=$2 WHERE id=$1`, id, raw)
	if err := f.s.checkCPUScale(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM operations WHERE workload_id=$1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatal("manual stop bypassed", count, err)
	}
}
