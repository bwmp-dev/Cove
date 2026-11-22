package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPanelRecoveryAuthorizationSchedulingAndRetention(t *testing.T) {
	f := newAccountFixture(t)
	member := f.user("panel-member", "panel-member@example.com")
	f.sql("INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','panel-member','owner','[]')")
	path := "/api/settings/panel-backup"
	f.call(member, "default", "GET", path, nil, 403)
	f.call("root", "default", "GET", path, nil, 200)
	f.call("root", "default", "POST", path+"/actions", map[string]any{"action": "backup"}, 409)
	host := func(action string, body any, status int) *httptest.ResponseRecorder {
		t.Helper()
		data, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/api/internal/panel-host/"+action, bytes.NewReader(data))
		stamp := strconv.FormatInt(time.Now().Unix(), 10)
		r.Header.Set("Authorization", "Bearer "+f.s.AdminToken)
		r.Header.Set("X-Cove-Host-Time", stamp)
		r.Header.Set("X-Cove-Host-Signature", panelHostSignature(f.s.Key, stamp, r.Method, r.URL.Path, data))
		w := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("host %s: %d %s", action, w.Code, w.Body.String())
		}
		return w
	}
	claim := host("claim", map[string]any{"agent_id": "panel-agent"}, 200)
	if !strings.Contains(claim.Body.String(), `"sealed":""`) {
		t.Fatal("disabled schedule created a job")
	}
	host("claim", map[string]any{"agent_id": "other-agent"}, 409)
	config := defaultPanelBackupConfig()
	config.Enabled = true
	config.MaxBytes = 64 << 20
	f.call("root", "default", "PUT", path, config, 200)
	claim = host("claim", map[string]any{"agent_id": "panel-agent"}, 200)
	var opened bytes.Buffer
	if err := PanelRecoveryPlan(bytes.NewReader(claim.Body.Bytes()), &opened, f.s.Key, false); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Plan panelHostPlan `json:"plan"`
	}
	if json.Unmarshal(opened.Bytes(), &decoded) != nil || decoded.Plan.Action != "backup" || decoded.Plan.AgentID != "panel-agent" {
		t.Fatal("invalid host plan")
	}
	job := decoded.Plan.JobID
	claim = host("claim", map[string]any{"agent_id": "panel-agent"}, 200)
	opened.Reset()
	if PanelRecoveryPlan(bytes.NewReader(claim.Body.Bytes()), &opened, f.s.Key, false) != nil || json.Unmarshal(opened.Bytes(), &decoded) != nil || decoded.Plan.JobID != job {
		t.Fatal("claim replay created another backup")
	}
	f.call("root", "default", "POST", path+"/actions", map[string]any{"action": "backup"}, 409)
	f.call("root", "default", "PUT", path, config, 409)
	snapshot := panelRecoverySnapshot{ID: strings.Repeat("a", 64), CreatedAt: time.Now().UTC(), Version: "fixture", Bytes: 1000, Verified: true, Origin: f.s.PublicURL}
	host("report", map[string]any{"agent_id": "panel-agent", "job_id": job, "state": "succeeded", "snapshots": []panelRecoverySnapshot{snapshot}}, 200)
	claim = host("claim", map[string]any{"agent_id": "panel-agent"}, 200)
	if !strings.Contains(claim.Body.String(), `"sealed":""`) {
		t.Fatal("schedule ignored its interval")
	}
	f.call("root", "default", "POST", path+"/actions", map[string]any{"action": "restore", "snapshot_id": snapshot.ID}, 400)
	f.call("root", "default", "POST", path+"/actions", map[string]any{"action": "restore", "snapshot_id": strings.Repeat("b", 64), "confirm": true}, 400)
	f.call("root", "default", "POST", path+"/actions", map[string]any{"action": "restore", "snapshot_id": snapshot.ID, "confirm": true}, 202)
	host("report", map[string]any{"agent_id": "panel-agent", "state": "succeeded", "snapshots": make([]panelRecoverySnapshot, 103)}, 400)
	f.sql("UPDATE panel_recovery_jobs SET state='succeeded',finished_at=now()")
	f.sql("INSERT INTO panel_recovery_jobs(id,actor_id,action,state,requested_at) SELECT 'old-'||i,'fixture','backup','failed',now()-interval '31 days' FROM generate_series(1,1100) i")
	host("report", map[string]any{"agent_id": "panel-agent", "state": "succeeded", "snapshots": []panelRecoverySnapshot{snapshot}}, 200)
	var count int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM panel_recovery_jobs").Scan(&count); err != nil || count > 999 {
		t.Fatal("unbounded panel recovery history", count, err)
	}
	f.sql("INSERT INTO panel_recovery_jobs(id,actor_id,action,state,requested_at) VALUES('pending-remote','fixture','backup','failed',now()-interval '31 days')")
	host("report", map[string]any{"agent_id": "panel-agent", "job_id": "pending-remote", "action": "backup", "actor_id": "fixture", "state": "failed", "error": "Offsite copy pending", "pending_remote_job": "pending-remote", "snapshots": []panelRecoverySnapshot{snapshot}}, 200)
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM panel_recovery_jobs WHERE id='pending-remote'").Scan(&count); err != nil || count != 1 {
		t.Fatal("pending remote recovery reference pruned", err)
	}
	f.call("root", "default", "POST", path+"/actions", map[string]any{"action": "backup"}, 202)
	f.sql("UPDATE panel_recovery_jobs SET state='failed' WHERE state='queued'")
	host("report", map[string]any{"agent_id": "panel-agent", "job_id": "restored-receipt", "action": "restore", "actor_id": "fixture", "snapshot_id": snapshot.ID, "state": "failed", "pending_recovery": true, "error": "Host restore journal pending", "snapshots": []panelRecoverySnapshot{snapshot}}, 200)
	f.call("root", "default", "POST", path+"/actions", map[string]any{"action": "backup"}, 409)
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM panel_recovery_jobs WHERE id='restored-receipt' AND action='restore'").Scan(&count); err != nil || count != 1 {
		t.Fatal("post-restore receipt was not persisted", err)
	}
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE actor='panel-host:panel-agent'").Scan(&count); err != nil || count < 2 {
		t.Fatal("host recovery actions not audited", err)
	}
}

func TestPanelBackupOffsiteSecretsNeverLeaveSealedPlan(t *testing.T) {
	f := newAccountFixture(t)
	response := f.call("root", "default", "POST", "/api/storage-locations", map[string]any{"name": "Panel offsite", "storage": map[string]string{"protocol": "s3", "endpoint": "https://s3.example.com", "path": "backups", "access_key": "synthetic-access", "secret_key": "synthetic-storage-secret"}}, 201)
	var created struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(response.Body.Bytes(), &created) != nil || created.ID == "" {
		t.Fatal("storage fixture unavailable", response.Body.String())
	}
	config := defaultPanelBackupConfig()
	config.StorageID = created.ID
	config.Password = "synthetic-independent-panel-password"
	result := f.call("root", "default", "PUT", "/api/settings/panel-backup", config, 200)
	if strings.Contains(result.Body.String(), config.Password) || strings.Contains(result.Body.String(), "synthetic-storage-secret") {
		t.Fatal("panel settings exposed secrets")
	}
	var encrypted []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT configuration FROM panel_recovery WHERE id=1").Scan(&encrypted); err != nil || bytes.Contains(encrypted, []byte(config.Password)) {
		t.Fatal("unsealed panel configuration", err)
	}
	config.Password = ""
	f.call("root", "default", "PUT", "/api/settings/panel-backup", config, 200)
	if err := f.s.DB.QueryRow(context.Background(), "SELECT configuration FROM panel_recovery WHERE id=1").Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	plain, err := unseal(f.s.Key, encrypted)
	if err != nil || !bytes.Contains(plain, []byte("synthetic-independent-panel-password")) {
		t.Fatal("blank password did not preserve saved value", err)
	}
	config.StorageID = "other-team-storage"
	f.call("root", "default", "PUT", "/api/settings/panel-backup", config, 400)
}
