package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestManagedRegistryLegacyProvenanceIsScopedAndDurable(t *testing.T) {
	f, id := registryRemovalFixture(t)
	addRegistryRemovalWorkload(t, f, "legacy-consumer", "registry-node")
	addRegistryRemovalWorkload(t, f, "other-consumer", "registry-node")
	f.sql(`UPDATE managed_registries SET created_at=now()-interval '1 hour' WHERE id=$1`, id)
	for _, operation := range []struct{ id, workload, state, time string }{
		{"old-release", "legacy-consumer", "succeeded", "2 hours"},
		{"new-release", "legacy-consumer", "succeeded", "30 minutes"},
		{"old-failed", "legacy-consumer", "failed", "2 hours"},
		{"other-release", "other-consumer", "succeeded", "2 hours"},
	} {
		f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,created_at,updated_at) VALUES($1,$2,'deploy',$3,$4,now()-interval '3 hours',now()-$5::interval)`, operation.id, operation.workload, operation.state, []byte{}, operation.time)
	}
	seen := map[string]bool{}
	f.s.workerRPC = func(_ context.Context, node string, request worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "registry-node" || request.Method != "managed_registry_references" {
			t.Fatal("wrong reference scope", node, request.Method)
		}
		seen[request.ID] = true
		want := []string{"old-release"}
		if request.ID == "other-consumer" {
			want = []string{"other-release"}
		}
		if !slices.Equal(request.RegistryPrevious, want) {
			t.Fatal("unproven or foreign release was exempted", request.ID, request.RegistryPrevious)
		}
		return worker.RPCResponse{}, nil
	}
	f.call("root", "default", "POST", "/api/managed-registries/"+id+"/gc", nil, 202)
	if len(seen) != 2 {
		t.Fatal("not every consumer was inventoried", seen)
	}
}

func testManagedRegistryLegacyWorkerInventory(t *testing.T, f *accountFixture, registry, host string) {
	t.Helper()
	const workload, release = "legacy-consumer", "legacy-release"
	addRegistryRemovalWorkload(t, f, workload, "registry-node")
	f.sql(`UPDATE workloads SET name='Legacy retained service' WHERE id=$1`, workload)
	var created time.Time
	if err := f.s.DB.QueryRow(t.Context(), `SELECT created_at FROM managed_registries WHERE id=$1`, registry).Scan(&created); err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,updated_at) VALUES($1,$2,'deploy','succeeded',$3,$4)`, release, workload, []byte{}, created.Add(-time.Hour))
	dir := filepath.Join("/var/lib/cove/workloads", workload, "release-states")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, release+".compose.json")
	state, _ := json.Marshal(map[string]any{"compose_path": path, "environment": map[string]string{}, "revision": "legacy"})
	if err := os.WriteFile(filepath.Join(dir, release+".json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, name := range []string{release + ".json", release + ".compose.json", "rollback-alias.json"} {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				t.Error(err)
			}
		}
		f.sql(`DELETE FROM operations WHERE workload_id=$1`, workload)
		f.sql(`DELETE FROM workloads WHERE id=$1`, workload)
	})
	for _, test := range []struct {
		name, compose              string
		newer, referenced, refused bool
	}{
		{name: "other-registry", compose: `{"services":{"disabled":{"image":"other.example.test/app:legacy","scale":0}}}`},
		{name: "named-this-registry", compose: `{"services":{"disabled":{"image":"` + host + `/team/app:fixture","scale":0}}}`, referenced: true},
		{name: "unreadable-before-registry", compose: `{"services":`},
		{name: "unreadable-after-registry", compose: `{"services":`, newer: true, refused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(test.compose), 0600); err != nil {
				t.Fatal(err)
			}
			finished := created.Add(-time.Hour)
			if test.newer {
				finished = created.Add(time.Minute)
			}
			f.sql(`UPDATE operations SET updated_at=$2 WHERE id=$1`, release, finished)
			if test.refused {
				response := f.call("root", "default", "POST", "/api/managed-registries/"+registry+"/gc", nil, 409)
				for _, text := range []string{"registry_inventory_incomplete", "Legacy retained service", "Registry", "Retained Compose snapshot is unreadable"} {
					if !strings.Contains(response.Body.String(), text) {
						t.Fatal("real worker refusal omitted its safe named reason", response.Body.String())
					}
				}
				return
			}
			tx, err := f.s.DB.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			refs, unavailable, err := f.s.managedRegistryReferences(t.Context(), tx, "default", host, created)
			if err != nil || len(unavailable) != 0 || (len(refs) > 0) != test.referenced {
				t.Fatal("real legacy worker inventory", refs, unavailable, err)
			}
		})
	}
	t.Run("rollback-alias", func(t *testing.T) {
		if err := os.WriteFile(path, []byte(`{"services":{"disabled":{"image":"`+host+`/team/app:fixture","scale":0}}}`), 0600); err != nil {
			t.Fatal(err)
		}
		alias, _ := json.Marshal(map[string]string{"compose_path": path})
		if err := os.WriteFile(filepath.Join(dir, "rollback-alias.json"), alias, 0600); err != nil {
			t.Fatal(err)
		}
		tx, err := f.s.DB.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		refs, unavailable, err := f.s.managedRegistryReferences(t.Context(), tx, "default", host, created)
		if err != nil || len(unavailable) != 0 || len(refs) != 1 {
			t.Fatal("real rollback alias inventory", refs, unavailable, err)
		}
	})
	// Remove only this synthetic state before the existing real GC lifecycle.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, release+".json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "rollback-alias.json")); err != nil {
		t.Fatal(err)
	}
}

func TestManagedRegistryInventoryFailureNamesResourceAndNodeSafely(t *testing.T) {
	for _, scenario := range []string{"gc", "delete-data", "rpc-error-field", "unsafe-error"} {
		t.Run(scenario, func(t *testing.T) {
			f, id := registryRemovalFixture(t)
			addRegistryRemovalWorkload(t, f, "legacy-consumer", "registry-node")
			f.sql(`UPDATE workloads SET name='Legacy service' WHERE id='legacy-consumer'`)
			reason := "Retained Compose snapshot is unreadable and its release cannot be proven older than this registry."
			f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
				if scenario == "rpc-error-field" {
					return worker.RPCResponse{Error: reason}, nil
				}
				if scenario == "unsafe-error" {
					return worker.RPCResponse{}, errors.New("secret=fixture-private-password /private/workloads/path")
				}
				return worker.RPCResponse{}, errors.New(reason)
			}
			method, path := "POST", "/api/managed-registries/"+id+"/gc"
			var input any
			if scenario == "delete-data" {
				method, path, input = "DELETE", "/api/managed-registries/"+id, map[string]bool{"delete_data": true}
			}
			response := f.call("root", "default", method, path, input, 409)
			var body struct {
				Error          string `json:"error"`
				Resource, Node managedRegistryUnavailableNode
				Reason         string `json:"reason"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error != "registry_inventory_incomplete" || body.Resource.ID != "legacy-consumer" || body.Resource.Name != "Legacy service" || body.Node.ID != "registry-node" || body.Node.Name != "Registry" {
				t.Fatal("refusal omitted the blocking resource/node", response.Body.String(), err)
			}
			if scenario == "unsafe-error" {
				reason = "Worker reference inventory is unavailable; retry when the worker is ready."
			}
			if body.Reason != reason || strings.Contains(response.Body.String(), "fixture-private-password") || strings.Contains(response.Body.String(), "/private/") {
				t.Fatal("unsafe or missing inventory reason", response.Body.String())
			}
			var state string
			if err := f.s.DB.QueryRow(t.Context(), `SELECT state FROM managed_registries WHERE id=$1`, id).Scan(&state); err != nil || state != "ready" {
				t.Fatal("refused inventory changed registry state", state, err)
			}
		})
	}
}

func TestManagedRegistryLegacyProvenanceIsBounded(t *testing.T) {
	f, id := registryRemovalFixture(t)
	addRegistryRemovalWorkload(t, f, "legacy-consumer", "registry-node")
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,updated_at) SELECT 'old-'||i,'legacy-consumer','deploy','succeeded',$1,now()-interval '1 day' FROM generate_series(1,2049) i`, []byte{})
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if len(r.RegistryPrevious) != 2048 {
			t.Fatal("unbounded or empty provenance", len(r.RegistryPrevious))
		}
		return worker.RPCResponse{}, nil
	}
	f.call("root", "default", "POST", "/api/managed-registries/"+id+"/gc", nil, 202)
}
