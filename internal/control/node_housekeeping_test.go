package control

import (
	"context"
	"cove/internal/worker"
	"strings"
	"testing"
)

func TestDiskThresholdMountSelection(t *testing.T) {
	s := &worker.NodeMetrics{DiskTotal: 100, DiskAvailable: 40, Mounts: []worker.MountMetric{{Path: "/", Known: true, Total: 100, Available: 50}, {Path: "/data", Known: true, Total: 100, Available: 5}, {Path: "/proc"}}}
	for path, want := range map[string]float64{"": 60, "/": 50, "/data": 95, "*": 95} {
		value, known := diskThresholdValue(s, path, 80)
		if !known || value != want {
			t.Fatal(path, value, known)
		}
	}
	s.MountsLimited = true
	if _, known := diskThresholdValue(s, "*", 99); known {
		t.Fatal("incomplete mount inventory recovered")
	}
	if value, known := diskThresholdValue(s, "*", 80); !known || value != 95 {
		t.Fatal("known failure suppressed", value, known)
	}
	if _, known := diskThresholdValue(s, "/missing", 80); known {
		t.Fatal("missing mount treated as healthy")
	}
}

func TestHousekeepingAuthorizationAndAcknowledgedPruning(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('housekeeping-node','Node','localhost',22,'',$1,'ready','{"scheduled_cleanup":true,"journal_retention":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('housekeeping-project','Project','default')`)
	sealed, _ := seal(f.s.Key, []byte(`{}`))
	f.sql(`INSERT INTO workloads(id,name,node_id,project_id,team_id,configuration) VALUES('housekeeping-resource','Resource','housekeeping-node','housekeeping-project','default',$1)`, sealed)
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,created_at,updated_at) SELECT x,'housekeeping-resource','deploy','succeeded',$1,now()-interval '120 days',now()-interval '120 days' FROM unnest(ARRAY['delete-old','retained-old','dependency-old']) x`, sealed)
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES('unfinished','housekeeping-resource','deploy','queued',$1)`, sealed)
	f.sql(`INSERT INTO operation_dependencies(operation_id,prerequisite_id) VALUES('unfinished','dependency-old')`)
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "housekeeping-node" {
			t.Fatal(node)
		}
		if r.Method == "cleanup_control" {
			return worker.RPCResponse{CleanupState: &worker.CleanupState{Policy: worker.CleanupPolicy{IntervalHours: 6, MinimumAgeHours: 168}}}, nil
		}
		if r.Method != "prune_journals" || r.JournalPrune == nil || len(r.JournalPrune.Candidates) != 2 {
			t.Fatal("unsafe prune candidates", r)
		}
		for _, c := range r.JournalPrune.Candidates {
			if c.ID == "dependency-old" || c.ID == "unfinished" {
				t.Fatal("referenced/unfinished operation pruned", c)
			}
		}
		return worker.RPCResponse{JournalPrune: &worker.JournalPruneResult{Pruned: []string{"delete-old"}, Protected: 1}}, nil
	}
	if err := f.s.pruneNodeOperationHistory(ctx, "housekeeping-node", 90, 1000); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM operations`).Scan(&count); err != nil || count != 3 {
		t.Fatal(count, err)
	}
	owner := f.user("housekeeping-owner", "housekeeping-owner@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','housekeeping-owner','owner','[]')`)
	f.call(owner, "default", "GET", "/api/nodes/housekeeping-node/cleanup-policy", nil, 403)
	f.call("root", "default", "GET", "/api/nodes/housekeeping-node/cleanup-policy", nil, 200)
	f.call("root", "default", "PUT", "/api/nodes/housekeeping-node/cleanup-policy", worker.CleanupPolicy{IntervalHours: 0}, 400)
	f.call("root", "default", "POST", "/api/nodes/housekeeping-node/cleanup-run", map[string]string{"revision": "bad"}, 400)
	response := f.call("root", "default", "GET", "/api/settings/operation-retention", nil, 200)
	if !strings.Contains(response.Body.String(), `"protected":1`) {
		t.Fatal(response.Body.String())
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='housekeeping-node'`)
	f.call("root", "default", "GET", "/api/nodes/housekeeping-node/cleanup-policy", nil, 409)
}
