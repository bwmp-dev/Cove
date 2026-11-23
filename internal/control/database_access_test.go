package control

import (
	"context"
	"encoding/json"
	"testing"

	"cove/internal/worker"
)

func TestDatabaseDependentsRespectViewGrantsAndToolsRequireSecrets(t *testing.T) {
	f, source := managedDatabaseFixture(t, "")
	f.sql(`UPDATE nodes SET capabilities='{"managed_databases":true,"database_access":true,"private_links":true}' WHERE id='managed-node'`)
	visible := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Visible consumer", NodeID: "managed-node", ProjectID: "managed-project", Compose: "services:\n  app:\n    image: busybox:1.37\n"}, 201), "id")
	hidden := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Hidden consumer", NodeID: "managed-node", ProjectID: "managed-project", Compose: "services:\n  app:\n    image: busybox:1.37\n"}, 201), "id")
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('hidden-project','Private consumer','default')`)
	f.sql(`UPDATE workloads SET project_id='hidden-project' WHERE id=$1`, hidden)
	for _, consumer := range []string{visible, hidden} {
		f.call("root", "default", "PUT", "/api/workloads/"+consumer+"/variable-bindings", resourceVariableBinding{Name: "DATABASE", SourceID: source, SourceKey: "DB_URL"}, 200)
	}
	session := f.user("db-operator", "db-operator@example.com")
	role := jsonString(t, f.call("root", "default", "POST", "/api/teams/default/roles", map[string]any{"name": "Database settings without secrets", "permissions": []string{"view", "edit_configuration"}}, 200), "id")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','db-operator','member',$1)`, []grant{{Scope: "project", ScopeID: "managed-project", Role: role}})
	w := f.call(session, "default", "GET", "/api/workloads/"+source+"/database", nil, 200)
	var result struct {
		Dependents []databaseDependent `json:"dependents"`
		Hidden     int                 `json:"hidden_dependents"`
		CanCutover bool                `json:"can_cutover"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Dependents) != 1 || result.Dependents[0].ID != visible || result.Hidden != 1 {
		t.Fatal("dependent permission filtering disagrees", w.Body.String())
	}
	f.call(session, "default", "PUT", "/api/workloads/"+visible+"/variable-bindings", resourceVariableBinding{Name: "PASSWORD", SourceID: source, SourceKey: "DB_PASSWORD"}, 403)
	f.call(session, "default", "PUT", "/api/workloads/"+visible+"/variable-bindings", resourceVariableBinding{Name: "HOST", SourceID: source, SourceKey: "DB_HOST"}, 200)
	if result.CanCutover {
		t.Fatal("non-administrator offered coordinated cutover")
	}
	called := false
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		called = true
		return worker.RPCResponse{}, nil
	}
	f.call(session, "default", "POST", "/api/workloads/"+source+"/database-access", map[string]any{"tool": "adminer", "service": "postgres", "port": 8080}, 403)
	if called {
		t.Fatal("tool received credentials without secret permission")
	}
}

func TestDatabaseMetadataSurvivesPrunedSettingsAndExposesPoolAlias(t *testing.T) {
	f, id := managedDatabaseFixture(t, "")
	config := readDatabaseConfig(t, f, id)
	if err := configureDatabasePooler(&config, true); err != nil {
		t.Fatal(err)
	}
	config.Database.SettingsOperation = "pruned-operation"
	config.Database.PrivateCompose = config.Compose
	raw, err := f.s.sealJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("UPDATE workloads SET configuration=$2 WHERE id=$1", id, raw)
	w := f.call("root", "default", "GET", "/api/workloads/"+id+"/database", nil, 200)
	var metadata struct {
		State    string           `json:"settings_state"`
		Database databaseResource `json:"database"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.State != "unknown" || metadata.Database.PoolerHost != databasePoolHostname(config.Database) || metadata.Database.PrivateCompose != "" {
		t.Fatal("metadata did not degrade safely", w.Body.String())
	}
	w = f.call("root", "default", "GET", "/api/workloads/"+id, nil, 200)
	if err = json.Unmarshal(w.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Database.PoolerHost != databasePoolHostname(config.Database) || metadata.Database.PrivateCompose != "" {
		t.Fatal("resource API omitted alias or exposed internal recovery configuration")
	}
}

func TestDatabaseAccessRejectsStaleToolChoiceBeforeWorker(t *testing.T) {
	f, id := managedDatabaseFixture(t, "")
	f.sql(`UPDATE nodes SET capabilities='{"managed_databases":true,"database_access":true}' WHERE id='managed-node'`)
	config := readDatabaseConfig(t, f, id)
	config.Database.Engine, config.Database.Service = "mongodb", "mongo"
	raw, err := f.s.sealJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("UPDATE workloads SET configuration=$2 WHERE id=$1", id, raw)
	called := false
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		called = true
		return worker.RPCResponse{}, nil
	}
	f.call("root", "default", "POST", "/api/workloads/"+id+"/database-access", map[string]any{"tool": "adminer", "service": "postgres", "port": 8080}, 400)
	if called {
		t.Fatal("stale tool choice reached the worker or became a native tunnel")
	}
	var leases int
	if err = f.s.DB.QueryRow(t.Context(), "SELECT count(*) FROM database_access WHERE workload_id=$1", id).Scan(&leases); err != nil || leases != 0 {
		t.Fatal("stale tool choice created an access lease", err)
	}
}

func TestDatabaseCanDisableUnmanagedAllowedExtensionWithDependencyPreflight(t *testing.T) {
	f, id := managedDatabaseFixture(t, "")
	checked := false
	f.s.workerRPC = func(_ context.Context, _ string, in worker.RPCRequest) (worker.RPCResponse, error) {
		if in.Database == nil || in.Database.DisableExtension != "hstore" {
			t.Fatal("removal did not inspect the specific extension")
		}
		checked = true
		return worker.RPCResponse{Database: &worker.DatabaseInspection{Version: "18.7", Extensions: []string{"plpgsql", "hstore"}}}, nil
	}
	if len(readDatabaseConfig(t, f, id).Database.Extensions) != 0 {
		t.Fatal("fixture unexpectedly manages the installed extension")
	}
	revision := jsonString(t, f.call("root", "default", "GET", "/api/workloads/"+id+"/database", nil, 200), "revision")
	path := "/api/workloads/" + id + "/database/maintenance"
	f.call("root", "default", "POST", path, map[string]any{"kind": "extension", "extension": "arbitrary", "enabled": false, "confirm": true, "revision": revision}, 400)
	f.call("root", "default", "POST", path, map[string]any{"kind": "extension", "extension": "pg_trgm|hstore", "enabled": false, "confirm": true, "revision": revision}, 400)
	if checked {
		t.Fatal("unknown extension reached the engine")
	}
	w := f.call("root", "default", "POST", path, map[string]any{"kind": "extension", "extension": "hstore", "enabled": false, "confirm": true, "revision": revision}, 202)
	if !checked {
		t.Fatal("unmanaged extension skipped dependency inspection")
	}
	var raw []byte
	if err := f.s.DB.QueryRow(t.Context(), "SELECT payload FROM operations WHERE id=$1", jsonString(t, w, "id")).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var request worker.Request
	if f.s.unsealJSON(raw, &request) != nil || request.DatabaseMaintenance == nil || !request.DatabaseMaintenance.DisableExtension || request.DatabaseMaintenance.Extension != "hstore" {
		t.Fatal("wrong extension maintenance queued")
	}
}
