package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cove/internal/worker"
)

func TestPITRPolicyAuthorizationSealingAndScheduling(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('pitr-node','Node','localhost',22,'',$1,'ready','{"postgres_pitr":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('pitr-project','Project','default')`)
	data, _ := json.Marshal(workloadInput{Name: "Postgres", Kind: "service", Backup: &worker.BackupConfig{Repository: "local", Password: "synthetic-pitr-restic-password", KeepLast: 2}})
	encrypted, _ := seal(f.s.Key, data)
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('pitr-resource','pitr-node','Postgres',$1,'pitr-project','default')`, encrypted)
	settings := worker.PITRSettings{Enabled: true, Service: "postgres", IntervalHours: 24, KeepLast: 2, RetentionDays: 7, MaxBytes: 1 << 30}
	f.call("root", "default", "GET", "/api/workloads/pitr-resource/pitr", nil, 200)
	f.call("root", "default", "PUT", "/api/workloads/pitr-resource/pitr", map[string]any{"settings": settings}, 400)
	viewer := f.user("pitr-viewer", "pitr-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','pitr-viewer','member','[{"scope":"project","scope_id":"pitr-project","role":"viewer"}]')`)
	f.call(viewer, "default", "PUT", "/api/workloads/pitr-resource/pitr", map[string]any{"settings": settings, "confirm_restart": true}, 403)
	f.call("root", "default", "PUT", "/api/workloads/pitr-resource/pitr", map[string]any{"settings": settings, "confirm_restart": true}, 202)
	var stored []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT configuration FROM workload_pitr WHERE workload_id='pitr-resource'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	config, err := f.s.decodePITR(stored)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), config.Policy.Key) || len(config.Policy.Key) != 64 {
		t.Fatal("PITR key is not sealed")
	}
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "pitr-node" || r.Method != "pitr_status" || r.ID != "pitr-resource" {
			t.Fatal("unexpected PITR status request", r.Method)
		}
		return worker.RPCResponse{PITR: &worker.PITRStatus{State: "capturing", Bases: []worker.PITRBase{}}}, nil
	}
	body := f.call(viewer, "default", "GET", "/api/workloads/pitr-resource/pitr", nil, 200).Body.String()
	if strings.Contains(body, config.Policy.Key) || strings.Contains(body, "synthetic-pitr-restic-password") || strings.Contains(body, config.Policy.Archive) {
		t.Fatal("PITR secrets exposed")
	}
	f.call("root", "default", "POST", "/api/workloads/pitr-resource/pitr/backup", map[string]any{}, 409)
	f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id='pitr-resource'`)
	f.sql(`UPDATE workload_pitr SET next_backup_at=now()-interval '1 hour'`)
	if err = f.s.schedulePITR(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM operations WHERE workload_id='pitr-resource' AND action='pitr_backup'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("PITR schedule", count, err)
	}
	if err = f.s.schedulePITR(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id='pitr-resource'`)
	f.call("root", "default", "DELETE", "/api/workloads/pitr-resource/pitr", map[string]any{"confirm_delete": true}, 409)
	settings.Enabled = false
	f.call("root", "default", "PUT", "/api/workloads/pitr-resource/pitr", map[string]any{"settings": settings, "confirm_restart": true}, 202)
	f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id='pitr-resource'`)
	f.call("root", "default", "DELETE", "/api/workloads/pitr-resource/pitr", map[string]any{}, 400)
	f.call("root", "default", "DELETE", "/api/workloads/pitr-resource/pitr", map[string]any{"confirm_delete": true}, 202)
}

func TestPITRCopyCredentialsAreReturnedOnlyAtCreation(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('pitr-node','Node','localhost',22,'',$1,'ready','{"postgres_pitr":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('pitr-project','Project','default')`)
	workload, _ := json.Marshal(workloadInput{Name: "Postgres", Kind: "service"})
	encrypted, _ := seal(f.s.Key, workload)
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('pitr-resource','pitr-node','Postgres',$1,'pitr-project','default')`, encrypted)
	config := pitrConfiguration{Policy: worker.PITRPolicy{Archive: strings.Repeat("a", 32), Key: strings.Repeat("b", 64), Settings: worker.PITRSettings{Service: "postgres", IntervalHours: 24, KeepLast: 2, RetentionDays: 7, MaxBytes: 1 << 30}}, Backup: worker.BackupConfig{Repository: "local", Password: "synthetic-password", KeepLast: 2}}
	data, _ := json.Marshal(config)
	encrypted, _ = seal(f.s.Key, data)
	f.sql(`INSERT INTO workload_pitr(workload_id,configuration,enabled) VALUES('pitr-resource',$1,false)`, encrypted)
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "pitr-node" || r.Method != "pitr_copy_plan" || r.Request == nil {
			t.Fatal("unexpected copy plan", r.Method)
		}
		return worker.RPCResponse{RestoreCopy: &worker.RestoreCopy{Source: "pitr-resource", Compose: "services:\n  postgres:\n    image: postgres:17-alpine\n", Environment: map[string]string{"COVE_PITR_USER": "postgres", "COVE_PITR_PASSWORD": r.Request.Environment["COVE_PITR_PASSWORD"]}}}, nil
	}
	response := f.call("root", "default", "POST", "/api/workloads/pitr-resource/pitr/restore-copy", map[string]any{"name": "Recovered", "base": config.Policy.Archive + "_base_" + strings.Repeat("C", 24), "timestamp": time.Now().Add(-time.Minute).UTC()}, 202)
	var created struct {
		ID          string `json:"id"`
		Resource    string `json:"resource_id"`
		Credentials struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if response.Header().Get("Cache-Control") != "no-store" || created.Credentials.Username != "postgres" || len(created.Credentials.Password) != 64 {
		t.Fatal("missing protected creation credentials")
	}
	read := f.call("root", "default", "GET", "/api/workloads/"+created.Resource, nil, 200).Body.String()
	if strings.Contains(read, created.Credentials.Password) || strings.Contains(read, config.Policy.Key) {
		t.Fatal("saved recovery secret returned after creation")
	}
	var stored []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT configuration FROM workloads WHERE id=$1`, created.Resource).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), created.Credentials.Password) {
		t.Fatal("copy credentials were not sealed")
	}
	raw, err := unseal(f.s.Key, stored)
	var saved workloadInput
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Environment["COVE_PITR_PASSWORD"] != created.Credentials.Password {
		t.Fatal("sealed copy credentials do not match the creation response", err)
	}
}
