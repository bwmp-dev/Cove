package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestSizingWarmupBoundsAndHeadroom(t *testing.T) {
	c := sizingAdvice{Hours: 20, Samples: 25, LastSample: time.Now(), CPUP95: 40, MemoryP95: 100 << 20}
	suggestSizing(&c)
	if c.SuggestedCPUs == nil || *c.SuggestedCPUs != 0.5 || c.SuggestedMemoryMB == nil || *c.SuggestedMemoryMB != 128 {
		t.Fatal(c)
	}
	c.Hours = 1
	c.SuggestedCPUs = nil
	c.SuggestedMemoryMB = nil
	suggestSizing(&c)
	if c.SuggestedCPUs != nil || !strings.Contains(c.Notice, "20 distinct hours") {
		t.Fatal("replicas bypassed warmup", c)
	}
	c.Hours = 20
	c.MemoryP95 = math.MaxFloat64
	suggestSizing(&c)
	if c.SuggestedMemoryMB != nil {
		t.Fatal("overflowed memory suggestion", c)
	}
}

func TestAdviceCostIsolationAndMergedLiveLogs(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('advice-node','Node','localhost',22,'',$1,'ready','{"capacity_advice":true,"structured_logs":true}')`, []byte{})
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('default','advice-node')`)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('advice-project','Advice','default'),('other-advice-project','Other','default')`)
	config, _ := seal(f.s.Key, []byte(`{"compose":"services:\n  app:\n    image: busybox:1.37\n"}`))
	f.sql(`INSERT INTO workloads(id,name,node_id,project_id,team_id,configuration) VALUES('advice-resource','Advice','advice-node','advice-project','default',$1),('other-advice-resource','Other','advice-node','other-advice-project','default',$1)`, config)
	now := time.Now().UTC()
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "advice-node" {
			t.Fatal(node)
		}
		switch r.Method {
		case "capacity":
			if r.Request == nil || r.Request.Workload != "advice-resource" {
				t.Fatal(r)
			}
			return worker.RPCResponse{Preflight: &worker.PreflightReport{Checks: []worker.PreflightCheck{{Name: "Memory capacity", State: "warning", Message: "Declared memory exceeds free memory."}}}}, nil
		case "live_logs":
			return worker.RPCResponse{Telemetry: &worker.Telemetry{SampledAt: now, Logs: []worker.LogEntry{{ContainerID: "second", Container: "worker", Timestamp: now, Message: "new"}, {ContainerID: "first", Container: "app", Timestamp: now.Add(-time.Second), Message: "old"}}}}, nil
		default:
			t.Fatal(r.Method)
			return worker.RPCResponse{}, nil
		}
	}
	f.call("root", "default", "GET", "/api/workloads/advice-resource/capacity", nil, 200)
	response := f.call("root", "default", "GET", "/api/workloads/advice-resource/logs/live", nil, 200)
	if strings.Index(response.Body.String(), `"message":"old"`) > strings.Index(response.Body.String(), `"message":"new"`) {
		t.Fatal("live view is not merged by time", response.Body.String())
	}
	f.call("root", "default", "GET", "/api/workloads/advice-resource/logs/live?since=bad", nil, 400)
	f.call("root", "default", "GET", "/api/nodes/advice-node/cost", nil, 200)
	f.call("root", "default", "PUT", "/api/nodes/advice-node/cost", nodeCost{MonthlyMinor: 1200, Currency: "USD"}, 200)
	f.call("root", "default", "PUT", "/api/nodes/advice-node/cost", nodeCost{MonthlyMinor: -1, Currency: "USD"}, 400)
	response = f.call("root", "default", "GET", "/api/projects/advice-project/costs", nil, 200)
	if !strings.Contains(response.Body.String(), `"monthly_minor":600`) || !strings.Contains(response.Body.String(), `"node_id":"advice-node"`) {
		t.Fatal("node price not allocated across placements", response.Body.String())
	}
	member := f.user("advice-viewer", "advice-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','advice-viewer','member','[{"scope":"project","scope_id":"advice-project","role":"viewer"}]')`)
	f.call(member, "default", "GET", "/api/projects/advice-project/costs", nil, 200)
	f.call(member, "default", "PUT", "/api/nodes/advice-node/cost", nodeCost{MonthlyMinor: 1, Currency: "USD"}, 403)
	f.call(member, "default", "GET", "/api/projects/other-advice-project/costs", nil, 403)
	payload, _ := json.Marshal(worker.Container{Service: "app", Metrics: &worker.ContainerMetrics{Usage: &worker.ContainerUsage{CPUPercent: 40, MemoryUsed: 100 << 20}}})
	f.sql(`INSERT INTO metric_samples(node_id,workload_id,series,tier,bucket,sampled_at,payload) SELECT 'advice-node','advice-resource','container','hour',date_trunc('hour',now())-i*interval '1 hour',now()-i*interval '1 hour',$1 FROM generate_series(0,24)i`, payload)
	response = f.call(member, "default", "GET", "/api/workloads/advice-resource/sizing", nil, 200)
	if !strings.Contains(response.Body.String(), `"suggested_memory_mb":128`) || !strings.Contains(response.Body.String(), `"hours":25`) {
		t.Fatal(response.Body.String())
	}
	var count int
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM operations`).Scan(&count); err != nil || count != 0 {
		t.Fatal("read-only advice queued operations", count, err)
	}
}
