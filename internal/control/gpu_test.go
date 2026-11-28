package control

import (
	"context"
	"cove/internal/worker"
	"testing"
	"time"
)

func TestGPUInventoryEditorScopeAndUpgradeGate(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('gpu-project','Project','default')`)
	f.sql(`INSERT INTO environments(id,name,project_id,team_id) VALUES('gpu-environment','Prod','gpu-project','default')`)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('gpu-node','GPU','localhost',22,'',$1,'ready','{"gpu_assignments":true}')`, []byte{})
	configuration, _ := seal(f.s.Key, []byte(`{"name":"App","kind":"service"}`))
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,environment_id,project_id,team_id) VALUES('gpu-resource','gpu-node','App',$1,'gpu-environment','gpu-project','default')`, configuration)
	user := f.user("gpu-editor", "gpu-editor@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','gpu-editor','member','[{"scope":"environment","scope_id":"gpu-environment","role":"viewer"}]')`)
	path := "/api/workloads/gpu-resource/gpus"
	f.call(user, "default", "GET", path, nil, 403)
	f.sql(`UPDATE team_members SET grants='[{"scope":"environment","scope_id":"gpu-environment","role":"editor"}]' WHERE user_id='gpu-editor'`)
	calls := 0
	f.s.workerRPC = func(_ context.Context, node string, request worker.RPCRequest) (worker.RPCResponse, error) {
		calls++
		if node != "gpu-node" || request.Method != "gpus" {
			t.Fatal("GPU inventory crossed resource placement")
		}
		return worker.RPCResponse{GPUs: &worker.GPUInventory{CheckedAt: time.Now(), Devices: []worker.GPU{}}}, nil
	}
	response := f.call(user, "default", "GET", path, nil, 200)
	if response.Header().Get("Cache-Control") != "no-store" || calls != 1 {
		t.Fatal("GPU inventory cached or retried")
	}
	f.call(user, "default", "GET", "/api/nodes/gpu-node/gpus", nil, 403)
	f.call("root", "default", "GET", "/api/nodes/gpu-node/gpus", nil, 200)
	selection := []string{"GPU-12345678-1234-1234-1234-123456789abc"}
	request := worker.Request{Workload: "gpu-resource", Action: "deploy", Limits: map[string]worker.ServiceLimits{"app": {GPUs: &selection}}}
	if err := f.s.prepareGPUAssignments(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='gpu-node'`)
	f.call(user, "default", "GET", path, nil, 409)
	if err := f.s.prepareGPUAssignments(context.Background(), &request); err == nil {
		t.Fatal("old worker silently discarded GPU assignment")
	}
	request.Action = "stop"
	if err := f.s.prepareGPUAssignments(context.Background(), &request); err != nil {
		t.Fatal("GPU capability gated stopping", err)
	}
	f.call("root", "default", "PUT", "/api/resource-defaults", worker.ServiceLimits{GPUs: &selection}, 400)
}
