package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"fmt"
	"testing"
)

func TestDeletionReturnsRetainedWorkerStateWarning(t *testing.T) {
	f := newRegistryHookFixture(t, "distribution", "registry.example.test")
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method != "remove_workload" || r.ID != f.id {
			return worker.RPCResponse{}, fmt.Errorf("unexpected cleanup request")
		}
		return worker.RPCResponse{Removal: &worker.WorkloadRemovalResult{WorkerFilesRetained: true, CleanupWarning: "Worker files and images were kept because the last deployed snapshot is unavailable."}}, nil
	}
	response := f.call("root", "default", "DELETE", f.path, nil, 200)
	var result struct {
		OK      bool                         `json:"ok"`
		Cleanup worker.WorkloadRemovalResult `json:"cleanup"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result.OK || !result.Cleanup.WorkerFilesRetained || result.Cleanup.CleanupWarning == "" {
		t.Fatal(result, err)
	}
	var count int
	if err := f.s.DB.QueryRow(t.Context(), "SELECT count(*) FROM workloads WHERE id=$1", f.id).Scan(&count); err != nil || count != 0 {
		t.Fatal("dashboard resource not deleted", count, err)
	}
}

func TestDeletionReturnsBindRetentionReason(t *testing.T) {
	f := newRegistryHookFixture(t, "distribution", "registry.example.test")
	const reason = "Bind-mounted data inside this resource's state directory was kept."
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method != "remove_workload" || r.ID != f.id {
			return worker.RPCResponse{}, fmt.Errorf("unexpected cleanup request")
		}
		return worker.RPCResponse{Removal: &worker.WorkloadRemovalResult{WorkerFilesRetained: true, WorkerFilesRetainedReason: reason}}, nil
	}
	response := f.call("root", "default", "DELETE", f.path, nil, 200)
	var result struct {
		Cleanup worker.WorkloadRemovalResult `json:"cleanup"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result.Cleanup.WorkerFilesRetained || result.Cleanup.WorkerFilesRetainedReason != reason || result.Cleanup.CleanupWarning != "" {
		t.Fatal("normal bind retention lost its explanation or became a cleanup failure", result, err)
	}
}
