package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"testing"
)

func TestManagedRegistryRecordDeletionReturnsCleanup(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained-warning", true: "worker-failure"}[fail], func(t *testing.T) {
			f, id := registryRemovalFixture(t)
			f.sql(`UPDATE managed_registries SET state='removed',data_retained=false WHERE id=$1`, id)
			f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
				if node != "registry-node" || r.Method != "remove_workload" || r.ID != id {
					t.Fatal("wrong registry cleanup boundary", node, r.Method)
				}
				if fail {
					return worker.RPCResponse{}, errors.New("fixture cleanup unavailable")
				}
				return worker.RPCResponse{Removal: &worker.WorkloadRemovalResult{WorkerFilesRetained: true, CleanupWarning: "Worker files were kept because the worker could not remove them with its current permissions."}}, nil
			}
			want := 200
			if fail {
				want = 409
			}
			response := f.call("root", "default", "DELETE", "/api/managed-registries/"+id+"/record", nil, want)
			var count int
			if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM workloads WHERE id=$1`, id).Scan(&count); err != nil || (count == 1) != fail {
				t.Fatal("cleanup failure lost the registry record", count, err)
			}
			if !fail {
				var body struct {
					OK      bool                         `json:"ok"`
					Cleanup worker.WorkloadRemovalResult `json:"cleanup"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || !body.OK || !body.Cleanup.WorkerFilesRetained || body.Cleanup.CleanupWarning == "" {
					t.Fatal("registry cleanup warning was lost", body, err)
				}
			}
		})
	}
}
