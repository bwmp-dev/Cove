package control

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cove/internal/worker"
)

func managedDatabaseFixture(t *testing.T, version string) (*accountFixture, string) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('managed-node','Node','localhost',22,'',$1,'ready','{"managed_databases":true}')`, []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('managed-project','Databases','default')")
	in := databaseInput(t, "postgresql", "managed-node", "managed-project")
	if version != "" {
		in.Database.Version = version
	}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", in, 201), "id")
	deployed := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]any{"action": "deploy"}, 202), "id")
	f.sql("UPDATE operations SET state='succeeded' WHERE id=$1", deployed)
	return f, id
}

func TestDatabaseMaintenancePreservesCredentialsUntilConfirmedAndResumesSameChange(t *testing.T) {
	f, id := managedDatabaseFixture(t, "")
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/backup-policy", map[string]any{"backup": worker.BackupConfig{Repository: "local", Password: strings.Repeat("e", 32), KeepLast: 3, IntervalHours: 1}}, 200)
	original := readDatabaseConfig(t, f, id)
	revision := jsonString(t, f.call("root", "default", "GET", "/api/workloads/"+id+"/database", nil, 200), "revision")
	path := "/api/workloads/" + id + "/database/maintenance"
	f.call("root", "default", "POST", path, map[string]any{"kind": "rotate", "revision": revision}, 400)
	f.call("root", "default", "POST", path, map[string]any{"kind": "rotate", "revision": "stale", "confirm": true}, 409)
	f.call("root", "default", "POST", path, map[string]any{"kind": "extension", "revision": revision, "confirm": true, "extension": "arbitrary"}, 400)
	task := jsonString(t, f.call("root", "default", "POST", path, map[string]any{"kind": "rotate", "revision": revision, "confirm": true}, 202), "id")
	if readDatabaseConfig(t, f, id).Environment["POSTGRES_PASSWORD"] != original.Environment["POSTGRES_PASSWORD"] {
		t.Fatal("unconfirmed credentials became active")
	}
	f.call("root", "default", "GET", "/api/workloads/"+id+"/database/credentials", nil, 409)
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/backup-policy", map[string]any{"backup": nil}, 409)
	f.call("root", "default", "POST", "/api/operations/"+task+"/cancel", nil, 409)
	if err := f.s.finishDatabaseMaintenance(context.Background(), task, "failed", []byte(`{"error":"deliberate interruption"}`)); err != nil {
		t.Fatal(err)
	}
	f.sql("UPDATE workloads SET next_backup_at=now()-interval '1 hour' WHERE id=$1", id)
	if err := f.s.scheduleBackups(context.Background()); err != nil {
		t.Fatal("existing backup scheduler failed during maintenance", err)
	}
	var backup string
	var backupPayload []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT id,payload FROM operations WHERE workload_id=$1 AND action='backup' AND state='queued'", id).Scan(&backup, &backupPayload); err != nil {
		t.Fatal(err)
	}
	if err := f.s.processOperation(context.Background(), backup, "managed-node", backupPayload); err != nil {
		t.Fatal("held backup was dispatched", err)
	}
	f.s.workerRPC = func(_ context.Context, _ string, in worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{}, errors.New("worker has not accepted maintenance yet")
	}
	f.call("root", "default", "POST", path+"/"+task+"/retry", map[string]any{}, 202)
	var backupState string
	if err := f.s.DB.QueryRow(context.Background(), "SELECT state FROM operations WHERE id=$1", backup).Scan(&backupState); err != nil || backupState != "cancelled" {
		t.Fatal("unstarted scheduled backup blocked maintenance retry", backupState, err)
	}
	if err := f.s.finishDatabaseMaintenance(context.Background(), task, "succeeded", []byte(`{"id":"confirmed"}`)); err != nil {
		t.Fatal(err)
	}
	current := readDatabaseConfig(t, f, id)
	if current.Environment["POSTGRES_PASSWORD"] == original.Environment["POSTGRES_PASSWORD"] || len(current.Environment["POSTGRES_PASSWORD"]) != 64 {
		t.Fatal("confirmed rotation was not committed")
	}
	w := f.call("root", "default", "GET", "/api/workloads/"+id+"/database/credentials", nil, 200)
	if !strings.Contains(w.Body.String(), current.Environment["POSTGRES_PASSWORD"]) {
		t.Fatal("confirmed credentials not returned")
	}
}

func TestDatabaseUpgradeUsesSeparateVolumesFreshCredentialsAndApprovalGate(t *testing.T) {
	f, id := managedDatabaseFixture(t, "17.12-alpine")
	f.s.workerRPC = func(_ context.Context, _ string, in worker.RPCRequest) (worker.RPCResponse, error) {
		if in.Method != "database_inspection" {
			return worker.RPCResponse{}, errors.New("unexpected worker request")
		}
		return worker.RPCResponse{Database: &worker.DatabaseInspection{Version: "17.12", Extensions: []string{"plpgsql"}, Arch: "x86_64"}}, nil
	}
	original := readDatabaseConfig(t, f, id)
	if !strings.Contains(original.Compose, "data:/var/lib/postgresql/data") {
		t.Fatal("PostgreSQL 17 data directory is incorrect")
	}
	f.call("root", "default", "PUT", "/api/teams/default/deployment-policy", map[string]any{"approval": true}, 200)
	revision := jsonString(t, f.call("root", "default", "GET", "/api/workloads/"+id+"/database", nil, 200), "revision")
	f.call("root", "default", "POST", "/api/workloads/"+id+"/database/maintenance", map[string]any{"kind": "upgrade", "revision": revision, "confirm": true, "version": "18.7-alpine", "name": "Upgraded copy"}, 400)
	w := f.call("root", "default", "POST", "/api/workloads/"+id+"/database/maintenance", map[string]any{"kind": "upgrade", "revision": revision, "confirm": true, "confirm_writes_stopped": true, "version": "18.7-alpine", "name": "Upgraded copy"}, 202)
	target, task := jsonString(t, w, "resource_id"), jsonString(t, w, "id")
	copy := readDatabaseConfig(t, f, target)
	if target == id || copy.Database.Version != "18.7-alpine" || copy.Database.Host == original.Database.Host || copy.Environment["POSTGRES_PASSWORD"] == original.Environment["POSTGRES_PASSWORD"] || !strings.Contains(copy.Compose, "data:/var/lib/postgresql\n") {
		t.Fatal("upgrade target is not isolated")
	}
	admitted, err := f.s.admitDeployment(context.Background(), task)
	if err != nil || admitted {
		t.Fatal("maintenance bypassed approval", err)
	}
	f.call("root", "default", "POST", "/api/operations/"+task+"/approve", nil, 200)
	admitted, err = f.s.admitDeployment(context.Background(), task)
	if err != nil || !admitted {
		t.Fatal("approved maintenance not admitted", err)
	}
	if !f.s.databaseLocked(context.Background(), id) {
		t.Fatal("upgrade source can change during copy")
	}
	if err = f.s.finishDatabaseMaintenance(context.Background(), task, "succeeded", []byte(`{"id":"copied"}`)); err != nil {
		t.Fatal(err)
	}
	if f.s.databaseLocked(context.Background(), id) || readDatabaseConfig(t, f, id).Database.Version != "17.12-alpine" {
		t.Fatal("upgrade changed the source")
	}
}
