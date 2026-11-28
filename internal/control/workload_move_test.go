package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func moveFixture(t *testing.T, phase string) (*accountFixture, workloadMoveIntent) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('move-project','Project','default')`)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('source','Source','source.invalid',22,'',$1,'ready','{"workload_moves":true}'),('target','Target','target.invalid',22,'',$1,'ready','{"workload_moves":true}')`, []byte{})
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('default','source'),('default','target')`)
	configuration, _ := seal(f.s.Key, []byte(`{"name":"App","node_id":"source","kind":"service","environment":{"PRIVATE":"never-return-this"}}`))
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('resource','source','App',$1,'move-project','default')`, configuration)
	intent := workloadMoveIntent{Action: "move", Configuration: configuration, SourceDaemon: "source-daemon", TargetDaemon: "target-daemon"}
	data, _ := json.Marshal(intent)
	payload, _ := seal(f.s.Key, data)
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES('move-operation','resource','move','running',$1)`, payload)
	f.sql(`INSERT INTO workload_moves(operation_id,source_node,target_node,phase) VALUES('move-operation','source','target',$1)`, phase)
	return f, intent
}

func TestWorkloadMoveNeverRestartsSourceUntilDestinationFenceConfirmed(t *testing.T) {
	f, intent := moveFixture(t, "rolling_back")
	calls := []string{}
	reachable := false
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Move.Daemon != node+"-daemon" {
			t.Fatal("move recovery did not bind the original Docker identity")
		}
		calls = append(calls, node+":"+r.Move.Action)
		if !reachable {
			return worker.RPCResponse{}, errors.New("unreachable destination")
		}
		phase := "started"
		if node == "source" {
			phase = "exported"
		}
		return worker.RPCResponse{Move: &worker.MoveState{ID: r.Move.ID, Workload: r.Move.Workload, Phase: phase}}, nil
	}
	if err := f.s.processWorkloadMove(context.Background(), "move-operation", intent); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"target:status"}) {
		t.Fatal("unreachable destination allowed source restart", calls)
	}
	f.call("root", "default", "PUT", "/api/nodes/target", map[string]any{"name": "Moved endpoint", "address": "replacement.invalid", "port": 22}, 409)
	f.call("root", "default", "DELETE", "/api/nodes/target", nil, 409)
	reachable = true
	calls = nil
	if err := f.s.processWorkloadMove(context.Background(), "move-operation", intent); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"target:status", "target:fence", "source:status", "source:rollback"}) {
		t.Fatal("source rollback preceded destination fencing", calls)
	}
	if err := f.s.processWorkloadMove(context.Background(), "move-operation", intent); err != nil {
		t.Fatal(err)
	}
	var node, state string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT w.node_id,o.state FROM workloads w JOIN operations o ON o.workload_id=w.id WHERE o.id='move-operation'`).Scan(&node, &state); err != nil {
		t.Fatal(err)
	}
	if node != "source" || state != "failed" {
		t.Fatal("failed move changed placement or reported success")
	}
}

func TestWorkloadMoveCommitAndCleanupResumeWithoutRollingBackCommittedPlacement(t *testing.T) {
	f, intent := moveFixture(t, "committing")
	departures := 0
	calls := []string{}
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Move.Daemon != node+"-daemon" {
			t.Fatal("move recovery did not bind the original Docker identity")
		}
		calls = append(calls, node+":"+r.Move.Action)
		if r.Move.Action == "depart" {
			departures++
			if departures == 1 {
				return worker.RPCResponse{}, errors.New("source temporarily unreachable")
			}
		}
		return worker.RPCResponse{Move: &worker.MoveState{ID: r.Move.ID, Workload: r.Move.Workload, Phase: "started"}}, nil
	}
	for i := 0; i < 3; i++ {
		if err := f.s.processWorkloadMove(context.Background(), "move-operation", intent); err != nil {
			t.Fatal(err)
		}
	}
	var node, state, phase string
	var configuration []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT w.node_id,w.configuration,o.state,m.phase FROM workloads w JOIN operations o ON o.workload_id=w.id JOIN workload_moves m ON m.operation_id=o.id WHERE o.id='move-operation'`).Scan(&node, &configuration, &state, &phase); err != nil {
		t.Fatal(err)
	}
	if node != "target" || state != "succeeded" || phase != "succeeded" {
		t.Fatal("committed move did not resume cleanup", node, state, phase)
	}
	data, err := unseal(f.s.Key, configuration)
	if err != nil {
		t.Fatal(err)
	}
	var saved workloadInput
	if json.Unmarshal(data, &saved) != nil || saved.NodeID != "target" || saved.Environment["PRIVATE"] != "never-return-this" {
		t.Fatal("move lost or exposed the protected resource configuration")
	}
	for _, call := range calls {
		if call == "source:rollback" || call == "target:fence" {
			t.Fatal("committed placement was rolled back", calls)
		}
	}
}
