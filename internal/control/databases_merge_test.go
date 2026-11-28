package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestDatabasesMergeScopedTokensRevalidateSecretsAndPolicies(t *testing.T) {
	f, id := managedDatabaseFixture(t, "")
	session := f.user("database-api", "database-api@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','database-api','owner')`)
	token := jsonString(t, f.call(session, "default", "POST", "/api/access-tokens", map[string]any{"name": "Database client", "scope": "user", "days": 1, "scopes": []string{"workloads:read", "workloads:write", "databases:manage"}}, 201), "token")
	call := func(method, path string, body any, want int) {
		t.Helper()
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, "/api/v1"+path, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Cove-Team", "default")
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, req)
		if response.Code != want {
			t.Fatal(method, path, response.Code, want, response.Body.String())
		}
	}
	call(http.MethodGet, "/workloads/"+id+"/database", nil, 200)
	call(http.MethodGet, "/workloads/"+id+"/database/credentials", nil, 200)
	f.sql(`INSERT INTO team_roles(team_id,id,name,permissions) VALUES('default','database-no-secrets','Database config',ARRAY['view','edit_configuration'])`)
	f.sql(`UPDATE team_members SET role='member',grants='[{"scope":"team","role":"database-no-secrets"}]' WHERE user_id='database-api'`)
	call(http.MethodGet, "/workloads/"+id+"/database", nil, 200)
	call(http.MethodGet, "/workloads/"+id+"/database/credentials", nil, 403)
	call(http.MethodPost, "/workloads/"+id+"/database/maintenance", map[string]any{"kind": "rotate", "confirm": true}, 403)
	f.sql(`UPDATE team_roles SET permissions=ARRAY['view','edit_configuration','read_secrets'] WHERE id='database-no-secrets'`)
	call(http.MethodGet, "/workloads/"+id+"/database/credentials", nil, 200)
	f.sql(`UPDATE teams SET require_second_factor=true WHERE id='default'`)
	call(http.MethodGet, "/workloads/"+id+"/database", nil, 403)
	f.sql(`UPDATE teams SET require_second_factor=false WHERE id='default'`)
	f.sql(`UPDATE control_settings SET panel_allowlist=ARRAY['127.0.0.1/32']`)
	call(http.MethodGet, "/workloads/"+id+"/database", nil, 403)
}

func TestDatabasesMergeMaintenancePreservesFreezeAndPauseGates(t *testing.T) {
	f, id := managedDatabaseFixture(t, "")
	f.sql(`INSERT INTO environments(id,project_id,team_id,name,paused) VALUES('database-paused','managed-project','default','Paused',true)`)
	f.sql(`UPDATE workloads SET environment_id='database-paused' WHERE id=$1`, id)
	revision := jsonString(t, f.call("root", "default", "GET", "/api/workloads/"+id+"/database", nil, 200), "revision")
	input := map[string]any{"kind": "rotate", "revision": revision, "confirm": true}
	f.call("root", "default", "POST", "/api/workloads/"+id+"/database/maintenance", input, 409)
	f.sql(`UPDATE environments SET paused=false WHERE id='database-paused'`)
	f.sql(`INSERT INTO deployment_policies(team_id,freeze_from,freeze_until) VALUES('default',now()-interval '1 minute',now()+interval '1 hour')`)
	operation := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/database/maintenance", input, 202), "id")
	if admitted, err := f.s.admitDeployment(t.Context(), operation); err != nil || admitted {
		t.Fatal("database maintenance bypassed freeze", admitted, err)
	}
}

func TestDatabasesMergeCanonicalRoundTripKeepsKindAndSealedValues(t *testing.T) {
	f, id := managedDatabaseFixture(t, "")
	config := readDatabaseConfig(t, f, id)
	config.Environment["COVE_DB_TLS_KEY"] = "fixture-private-material"
	config.Database.PrivateCompose = "private-template-marker"
	raw, err := f.s.sealJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE workloads SET configuration=$2 WHERE id=$1`, id, raw)
	manifest := projectConfiguration{APIVersion: "cove/v1", Project: "Managed project", Resources: []configurationResource{{Key: id, Configuration: config}}}
	manifest.Resources[0].Configuration.ProjectID = ""
	manifest.Resources[0].Configuration.EnvironmentID = ""
	for _, secrets := range []bool{false, true} {
		public := publicProjectConfiguration(manifest, secrets)
		data, _ := json.Marshal(public)
		if strings.Contains(string(data), "fixture-private-material") || strings.Contains(string(data), "private-template-marker") {
			t.Fatal("database internals leaked")
		}
		if err := validateProjectConfiguration(public); err != nil {
			t.Fatal("portable database no longer validates", err)
		}
		merged, err := mergeManifestResource(public.Resources[0].Configuration, config)
		if err != nil || merged.Database == nil || merged.Environment["COVE_DB_TLS_KEY"] != "fixture-private-material" {
			t.Fatal("database round-trip changed sealed values", err)
		}
	}
	response := f.call("root", "default", "GET", "/api/projects/managed-project/configuration", nil, 200)
	var view struct {
		Manifest projectConfiguration `json:"manifest"`
	}
	if json.Unmarshal(response.Body.Bytes(), &view) != nil {
		t.Fatal("manifest unavailable")
	}
	data, _ := json.Marshal(view.Manifest)
	draft := jsonString(t, f.call("root", "default", "POST", "/api/projects/managed-project/configuration/drafts", map[string]string{"content": string(data)}, 201), "id")
	f.call("root", "default", "POST", "/api/projects/managed-project/configuration/drafts/"+draft+"/apply", nil, 200)
	var kind string
	if err := f.s.DB.QueryRow(t.Context(), "SELECT kind FROM workloads WHERE id=$1", id).Scan(&kind); err != nil || kind != "database" {
		t.Fatal("draft demoted database", kind, err)
	}
	current := readDatabaseConfig(t, f, id)
	if current.Environment["POSTGRES_PASSWORD"] != config.Environment["POSTGRES_PASSWORD"] {
		t.Fatal("draft rotated database credentials")
	}
}

func TestDatabasesMergeStagingDoesNotClearDependentRedeploy(t *testing.T) {
	f, source := managedDatabaseFixture(t, "")
	app := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Consumer", NodeID: "managed-node", ProjectID: "managed-project", Compose: "services: {}"}, 201), "id")
	f.call("root", "default", "PUT", "/api/workloads/"+app+"/variable-bindings", resourceVariableBinding{Name: "DATABASE", SourceID: source, SourceKey: "DB_URL"}, 200)
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result,created_at,updated_at) VALUES('old-consumer',$1,'deploy','succeeded',$2,'{}',now()-interval '1 hour',now()-interval '1 hour')`, app, []byte{})
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES('rotation',$1,'deploy','succeeded',$2,'{}')`, source, []byte{})
	f.sql(`INSERT INTO database_maintenance(id,workload_id,node_id,kind,configuration,state) VALUES('rotation',$1,'managed-node','rotate',$2,'succeeded')`, source, []byte{})
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result,updated_at) VALUES('consumer-candidate',$1,'deploy','succeeded',$2,'{"rollout":{"phase":"waiting","release":"consumer-candidate"}}',now()+interval '1 minute')`, app, []byte{})
	req := httptest.NewRequest("GET", "/", nil).WithContext(context.WithValue(context.Background(), identityKey{}, &identity{Admin: true, Team: "default"}))
	check := func(want bool) {
		t.Helper()
		items, _, err := f.s.databaseDependents(req, source)
		if err != nil || len(items) != 1 || items[0].NeedsRedeploy != want {
			t.Fatal("dependent commit state", items, err)
		}
	}
	check(true)
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result,updated_at) VALUES('consumer-promote',$1,'rollout_promote','succeeded',$2,'{"rollout":{"phase":"completed","release":"consumer-candidate"}}',now()+interval '2 minutes')`, app, []byte{})
	check(false)
}

func TestDatabasesMergeStatelessPoliciesAndOpenAPI(t *testing.T) {
	f, id := managedDatabaseFixture(t, "")
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/idle-policy", map[string]any{"enabled": true, "after_seconds": 300, "request_driven": true}, 400)
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/autoscale-policy", map[string]any{"enabled": true, "services": map[string]any{"postgres": map[string]any{"min_replicas": 1, "max_replicas": 2, "target_cpu_percent": 70}}}, 400)
	for _, engine := range databaseEngines {
		if (engine.id == "redis" || engine.id == "valkey") && !engine.logical {
			t.Fatal("cache backups omitted existing RDB support")
		}
		if engine.id == "mongodb" && engine.logical {
			t.Fatal("standalone Mongo falsely offered online replica-set backup")
		}
	}
	document, _ := json.Marshal(f.s.openAPIDocument())
	for _, path := range []string{"/database-engines", "/database/maintenance/{task}/cutover", "/database/credentials", "private_key", "writeOnly", "databases:manage"} {
		if !bytes.Contains(document, []byte(path)) {
			t.Fatal("database API contract omitted", path)
		}
	}
	f.sql(`UPDATE nodes SET capabilities=capabilities-'managed_databases' WHERE id='managed-node'`)
	r := httptest.NewRequest("GET", "/", nil).WithContext(context.WithValue(context.Background(), identityKey{}, &identity{Admin: true, Team: "default"}))
	if capable, err := f.s.canCutoverDatabase(r, id, id); err != nil || capable {
		t.Fatal("cutover capability did not fail closed", capable, err)
	}
}

func TestDatabasesMergeRetentionKeepsUnresolvedMaintenance(t *testing.T) {
	f, id := managedDatabaseFixture(t, "")
	revision := jsonString(t, f.call("root", "default", "GET", "/api/workloads/"+id+"/database", nil, 200), "revision")
	task := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/database/maintenance", map[string]any{"kind": "rotate", "revision": revision, "confirm": true}, 202), "id")
	if err := f.s.finishDatabaseMaintenance(t.Context(), task, "failed", []byte(`{"error":"retry required"}`)); err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE operations SET updated_at=now()-interval '2 years' WHERE id=$1`, task)
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		result := &worker.JournalPruneResult{}
		for _, candidate := range r.JournalPrune.Candidates {
			if candidate.ID == task {
				t.Fatal("unresolved maintenance offered for pruning")
			}
			result.Pruned = append(result.Pruned, candidate.ID)
		}
		return worker.RPCResponse{JournalPrune: result}, nil
	}
	if err := f.s.pruneNodeOperationHistory(t.Context(), "managed-node", 1, 1); err != nil {
		t.Fatal(err)
	}
	if !f.s.databaseLocked(t.Context(), id) {
		t.Fatal("retention unlocked uncertain credentials")
	}
}

func TestDatabasesMergeRestoreCopyRetainsSnapshotKindAndCredentials(t *testing.T) {
	f, source := managedDatabaseFixture(t, "")
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"restore_copies":true}' WHERE id='managed-node'`)
	f.call("root", "default", "PUT", "/api/workloads/"+source+"/backup-policy", map[string]any{"backup": worker.BackupConfig{Repository: "local", Password: strings.Repeat("x", 32), KeepLast: 3, IntervalHours: 1}}, 200)
	original := readDatabaseConfig(t, f, source)
	snapshot := databaseMetadata(original.Database)
	snapshot.Extensions = []string{"pg_trgm"}
	metadata, _ := json.Marshal(snapshot)
	plan := &worker.RestoreCopy{Compose: original.Compose, Environment: original.Environment, DatabaseMetadata: metadata}
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method != "restore_copy_plan" {
			t.Fatal("unexpected RPC", r.Method)
		}
		return worker.RPCResponse{RestoreCopy: plan}, nil
	}
	body := map[string]string{"name": "Database recovery", "backup_id": "snapshot"}
	response := f.call("root", "default", "POST", "/api/workloads/"+source+"/restore-copy", body, 202)
	recovered := jsonString(t, response, "resource_id")
	config := readDatabaseConfig(t, f, recovered)
	if config.Database == nil || len(config.Database.Extensions) != 1 || config.Database.Host == snapshot.Host || config.Environment["POSTGRES_PASSWORD"] != original.Environment["POSTGRES_PASSWORD"] {
		t.Fatal("managed snapshot metadata or credentials lost")
	}
	var kind string
	if err := f.s.DB.QueryRow(t.Context(), "SELECT kind FROM workloads WHERE id=$1", recovered).Scan(&kind); err != nil || kind != "database" {
		t.Fatal(kind, err)
	}
	f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id=$1`, recovered)
	plan.DatabaseMetadata = nil
	f.call("root", "default", "POST", "/api/workloads/"+source+"/restore-copy", body, 409)
}

func TestDatabasesMergeCanonicalImportCreatesTypedDatabaseWithFreshCredentials(t *testing.T) {
	f, source := managedDatabaseFixture(t, "")
	response := f.call("root", "default", "GET", "/api/projects/managed-project/configuration", nil, 200)
	var view struct {
		Manifest projectConfiguration `json:"manifest"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	target := jsonString(t, f.call("root", "default", "POST", "/api/projects", map[string]string{"name": "Imported databases"}, 201), "id")
	view.Manifest.Project = "Imported databases"
	raw, _ := json.Marshal(view.Manifest)
	draft := jsonString(t, f.call("root", "default", "POST", "/api/projects/"+target+"/configuration/drafts", map[string]string{"content": string(raw)}, 201), "id")
	f.call("root", "default", "POST", "/api/projects/"+target+"/configuration/drafts/"+draft+"/apply", nil, 200)
	var id, kind string
	if err := f.s.DB.QueryRow(t.Context(), "SELECT id,kind FROM workloads WHERE project_id=$1", target).Scan(&id, &kind); err != nil || kind != "database" {
		t.Fatal(kind, err)
	}
	created, original := readDatabaseConfig(t, f, id), readDatabaseConfig(t, f, source)
	if created.Database == nil || created.Database.Engine != "postgresql" || created.Environment["POSTGRES_PASSWORD"] == "" || created.Environment["POSTGRES_PASSWORD"] == original.Environment["POSTGRES_PASSWORD"] {
		t.Fatal("import lost typed metadata or reused credentials")
	}
}

func TestDatabasesMergeMovesKeepBucketAndCopyDependenciesExplicit(t *testing.T) {
	f, source := managedDatabaseFixture(t, "")
	destination := jsonString(t, f.call("root", "default", "POST", "/api/projects", map[string]string{"name": "Move destination"}, 201), "id")
	app := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Bucket owner", NodeID: "managed-node", ProjectID: "managed-project", Compose: "services: {}"}, 201), "id")
	f.sql(`INSERT INTO object_buckets(id,workload_id,storage_resource_id,configuration,state) VALUES('move-bucket',$1,$2,$3,'succeeded')`, app, source, []byte{})
	check := func(id, want string) {
		t.Helper()
		in := resourceMoveInput{TeamID: "default", ProjectID: destination}
		response := f.call("root", "default", "POST", "/api/workloads/"+id+"/move/preview", in, 200)
		var review resourceMoveReview
		if err := json.Unmarshal(response.Body.Bytes(), &review); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(review.Blockers, " "), want) {
			t.Fatal("dependent move was not blocked", review)
		}
		in.Version, in.Acknowledge = review.Version, true
		f.call("root", "default", "POST", "/api/workloads/"+id+"/move", in, 409)
	}
	check(app, "bucket keys")
	check(source, "bucket keys")
	f.sql(`DELETE FROM object_buckets WHERE id='move-bucket'`)
	target := jsonString(t, f.call("root", "default", "POST", "/api/workloads", databaseInput(t, "postgresql", "managed-node", "managed-project"), 201), "id")
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES('move-copy',$1,'deploy','succeeded',$2)`, target, []byte{})
	f.sql(`INSERT INTO database_maintenance(id,workload_id,source_id,node_id,kind,configuration,state) VALUES('move-copy',$1,$2,'managed-node','upgrade',$3,'succeeded')`, target, source, []byte{})
	check(source, "pending cut-over")
	check(target, "pending cut-over")
}
