package control

import (
	"context"
	"encoding/json"
	"testing"

	"cove/internal/worker"
)

func databaseCopyFixture(t *testing.T) (*accountFixture, string, string, string) {
	f, source := managedDatabaseFixture(t, "17.12-alpine")
	retained := readDatabaseConfig(t, f, source)
	retained.Database.TemplateRevision = "older-catalogue-revision"
	raw, err := f.s.sealJSON(retained)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("UPDATE workloads SET configuration=$2 WHERE id=$1", source, raw)
	f.s.workerRPC = func(_ context.Context, _ string, in worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{Database: &worker.DatabaseInspection{Version: "17.12", Extensions: []string{"plpgsql"}, Arch: "x86_64"}}, nil
	}
	revision := jsonString(t, f.call("root", "default", "GET", "/api/workloads/"+source+"/database", nil, 200), "revision")
	response := f.call("root", "default", "POST", "/api/workloads/"+source+"/database/maintenance", map[string]any{"kind": "upgrade", "revision": revision, "confirm": true, "confirm_writes_stopped": true, "version": "18.7-alpine", "name": "Isolated copy"}, 202)
	return f, source, jsonString(t, response, "resource_id"), jsonString(t, response, "id")
}

func TestDatabaseFailedCopyReleasesSourceAndCanBeExplicitlyDiscarded(t *testing.T) {
	f, source, target, task := databaseCopyFixture(t)
	if err := f.s.finishDatabaseMaintenance(t.Context(), task, "failed", []byte(`{"error":"restore rejected"}`)); err != nil {
		t.Fatal(err)
	}
	if f.s.databaseLocked(t.Context(), source) || !f.s.databaseLocked(t.Context(), target) {
		t.Fatal("failed copy did not restore source availability")
	}
	f.call("root", "default", "GET", "/api/workloads/"+source+"/database/credentials", nil, 200)
	retry := "/api/workloads/" + source + "/database/maintenance/" + task + "/retry"
	f.call("root", "default", "POST", retry, map[string]any{}, 400)
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,state) VALUES('busy-source',$1,'deploy',$2,'queued')`, source, []byte{})
	f.call("root", "default", "POST", retry, map[string]any{"confirm_writes_stopped": true}, 409)
	f.sql("UPDATE operations SET state='cancelled' WHERE id='busy-source'")
	path := "/api/workloads/" + source + "/database/maintenance/" + task + "/discard"
	f.call("root", "default", "POST", path, map[string]any{"confirm": true, "name": "Wrong copy"}, 400)
	f.call("root", "default", "POST", path, map[string]any{"confirm": true, "name": "Isolated copy"}, 200)
	if f.s.databaseLocked(t.Context(), target) {
		t.Fatal("discarded copy remains locked")
	}
	var state string
	if err := f.s.DB.QueryRow(t.Context(), "SELECT state FROM database_maintenance WHERE id=$1", task).Scan(&state); err != nil || state != "discarded" {
		t.Fatal("discard not recorded", state, err)
	}
	f.call("root", "default", "POST", path, map[string]any{"confirm": true, "name": "Isolated copy"}, 200)
	if readDatabaseConfig(t, f, source).Database.Version != "17.12-alpine" {
		t.Fatal("source changed during failed-copy recovery")
	}
}

func TestDatabaseCopyCutoverRequiresConfirmationAndRetainsSource(t *testing.T) {
	f, source, target, task := databaseCopyFixture(t)
	f.sql(`UPDATE nodes SET capabilities='{"managed_databases":true,"private_links":true}' WHERE id='managed-node'`)
	if err := f.s.finishDatabaseMaintenance(t.Context(), task, "succeeded", []byte(`{"id":"verified"}`)); err != nil {
		t.Fatal(err)
	}
	consumer := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Consumer", NodeID: "managed-node", ProjectID: "managed-project", Compose: "services:\n  app:\n    image: busybox:1.37\n"}, 201), "id")
	f.call("root", "default", "PUT", "/api/workloads/"+consumer+"/variable-bindings", resourceVariableBinding{Name: "DATABASE", SourceID: source, SourceKey: "DB_URL"}, 200)
	connection := jsonString(t, f.call("root", "default", "POST", "/api/private-connections", privateConnectionInput{SourceID: consumer, SourceService: "app", TargetID: source, TargetService: "postgres", Port: 5432, Alias: databaseHostname(source)}, 201), "id")
	f.sql("UPDATE operations SET created_at=now()-interval '2 hours',updated_at=now()-interval '2 hours' WHERE id=$1", task)
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,state,created_at,updated_at) VALUES('consumer-before-cutover',$1,'deploy',$2,'succeeded',now()-interval '1 hour',now()-interval '1 hour')`, consumer, []byte{})
	revision := jsonString(t, f.call("root", "default", "GET", "/api/workloads/"+source+"/database", nil, 200), "revision")
	path := "/api/workloads/" + source + "/database/maintenance/" + task + "/cutover"
	assertEligible := func(want bool) {
		t.Helper()
		w := f.call("root", "default", "GET", "/api/workloads/"+source+"/database/maintenance", nil, 200)
		var rows []struct {
			ID         string `json:"id"`
			CanCutover bool   `json:"can_cutover"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == task {
				if row.CanCutover != want {
					t.Fatal("cutover eligibility differs from operation state")
				}
				return
			}
		}
		t.Fatal("copy missing from maintenance history")
	}
	assertEligible(true)
	f.call("root", "default", "POST", path, map[string]any{"revision": revision, "confirm": true}, 400)
	f.call("root", "default", "POST", path, map[string]any{"revision": "stale", "confirm": true, "confirm_writes_stopped": true}, 409)
	f.call("root", "default", "POST", path, map[string]any{"revision": revision, "confirm": true, "confirm_writes_stopped": true}, 200)
	assertEligible(false)
	var bound, linked, alias string
	var version int
	if err := f.s.DB.QueryRow(t.Context(), "SELECT source_id FROM resource_variable_bindings WHERE workload_id=$1", consumer).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(t.Context(), "SELECT target_id,alias,version FROM resource_connections WHERE id=$1", connection).Scan(&linked, &alias, &version); err != nil {
		t.Fatal(err)
	}
	if bound != target || linked != target || alias != databaseHostname(target) || version != 2 {
		t.Fatal("cutover did not move bindings and network intent together")
	}
	if readDatabaseConfig(t, f, source).Database.Version != "17.12-alpine" {
		t.Fatal("cutover changed the retained source")
	}
	assertRedeploy := func(want bool) {
		t.Helper()
		response := f.call("root", "default", "GET", "/api/workloads/"+target+"/database", nil, 200)
		var details struct {
			Dependents []databaseDependent `json:"dependents"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &details); err != nil {
			t.Fatal(err)
		}
		if len(details.Dependents) != 1 || details.Dependents[0].ID != consumer || details.Dependents[0].NeedsRedeploy != want {
			t.Fatal("cutover consumer redeploy status is incorrect", details.Dependents)
		}
	}
	assertRedeploy(true)
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,state,created_at,updated_at) VALUES('consumer-after-cutover',$1,'deploy',$2,'succeeded',now()+interval '1 minute',now()+interval '1 minute')`, consumer, []byte{})
	assertRedeploy(false)
}

func TestDatabaseCopyCompatibilityRejectsMissingExtensionsAndWrongArchitecture(t *testing.T) {
	source := &databaseResource{Engine: "postgresql", Version: "18.7-alpine"}
	actual := &worker.DatabaseInspection{Version: "18.7", Extensions: []string{"plpgsql", "vector"}, Arch: "x86_64"}
	if databaseCopyCompatibility(source, &databaseResource{}, actual) == nil {
		t.Fatal("missing target extension accepted")
	}
	if err := databaseCopyCompatibility(source, &databaseResource{Variant: "vector"}, actual); err != nil {
		t.Fatal(err)
	}
	actual.Extensions = nil
	actual.Arch = "aarch64"
	if databaseCopyCompatibility(source, &databaseResource{Variant: "postgis"}, actual) == nil {
		t.Fatal("unsupported PostGIS architecture accepted")
	}
}
