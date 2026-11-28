package control

import (
	"cove/internal/worker"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestImageScansMergeMinimalManagedDatabaseAndScopedSettings(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("scanner-owner", "scanner-owner@example.invalid")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','scanner-owner','owner')`)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('scan-node','Scanner','localhost',22,'',$1,'ready','{"image_scanning":true,"outbound_proxy":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('scan-project','Project','default')`)
	f.sql(`INSERT INTO environments(id,name,project_id,team_id,paused) VALUES('scan-environment','Held','scan-project','default',true)`)
	sealed, err := f.s.sealJSON(workloadInput{Kind: "database", Compose: "services: {}", Environment: map[string]string{"PASSWORD": "application-value-must-stay-sealed"}})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO workloads(id,node_id,name,kind,configuration,team_id,project_id,environment_id,manually_stopped) VALUES('scan-resource','scan-node','Database','database',$1,'default','scan-project','scan-environment',true)`, sealed)
	_, read := mintPublic(t, f, owner, "user", "workloads:read")
	_, write := mintPublic(t, f, owner, "user", "workloads:write")
	_, configure := mintPublic(t, f, owner, "user", "configuration:write")
	_, operate := mintPublic(t, f, owner, "user", "workloads:operate")
	path := "/api/v1/workloads/scan-resource"
	settings := map[string]bool{"enabled": true, "notify": true}
	publicCall(t, f, read, "default", "GET", path+"/image-scanning", nil, 200)
	publicCall(t, f, read, "default", "GET", path+"/image-scans", nil, 200)
	publicCall(t, f, write, "default", "PUT", path+"/image-scanning", settings, 403)
	publicCall(t, f, configure, "default", "PUT", path+"/image-scanning", settings, 200)
	publicCall(t, f, read, "default", "POST", path+"/operations", map[string]string{"action": "image_scan"}, 403)
	queued := publicCall(t, f, operate, "default", "POST", path+"/operations", map[string]string{"action": "image_scan"}, 202)
	id := jsonString(t, queued, "id")
	var payload []byte
	if err := f.s.DB.QueryRow(t.Context(), `SELECT payload FROM operations WHERE id=$1`, id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var request worker.Request
	if err := f.s.unsealJSON(payload, &request); err != nil {
		t.Fatal(err)
	}
	f.call("root", "default", "PUT", "/api/settings/outbound-proxy", map[string]string{"https_proxy": "http://fixture:opaque@proxy.example.invalid:8080"}, 200)
	if err := f.s.prepareInfrastructure(t.Context(), &request); err != nil {
		t.Fatal(err)
	}
	if request.Kind != "service" || request.ManagedDatabase || len(request.DatabaseMetadata) != 0 || request.RecordDrift || request.DefaultLimits != nil || len(request.Environment) != 0 || request.Compose != "" || request.Registry != nil || worker.ValidateRequest(request) != nil {
		t.Fatal("scan acquired deployment inputs")
	}
	if request.OutboundProxy == nil || request.OutboundProxy.HTTPS != "http://fixture:opaque@proxy.example.invalid:8080" {
		t.Fatal("scan lost infrastructure proxy policy")
	}
	preparedBytes, _ := json.Marshal(request)
	f.call("root", "default", "PUT", "/api/settings/outbound-proxy", map[string]string{"https_proxy": "http://different.example.invalid:8080"}, 200)
	if err := f.s.DB.QueryRow(t.Context(), `SELECT payload FROM operations WHERE id=$1`, id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if err := f.s.unsealJSON(payload, &request); err != nil {
		t.Fatal(err)
	}
	if err := f.s.prepareInfrastructure(t.Context(), &request); err != nil {
		t.Fatal(err)
	}
	retriedBytes, _ := json.Marshal(request)
	if string(preparedBytes) != string(retriedBytes) {
		t.Fatal("uncertain dispatch inputs changed during retry")
	}
	f.sql(`UPDATE nodes SET capabilities='{"image_scanning":true}' WHERE id='scan-node'`)
	if err := f.s.prepareInfrastructure(t.Context(), &request); err == nil {
		t.Fatal("scanner ignored missing outbound-proxy capability")
	}
	f.sql(`UPDATE nodes SET capabilities='{"image_scanning":true,"outbound_proxy":true}' WHERE id='scan-node'`)
	var stopped, paused, prepared bool
	if err := f.s.DB.QueryRow(t.Context(), `SELECT w.manually_stopped,e.paused,o.drift_prepared FROM workloads w JOIN environments e ON e.id=w.environment_id JOIN operations o ON o.workload_id=w.id WHERE o.id=$1`, id).Scan(&stopped, &paused, &prepared); err != nil || !stopped || !paused || !prepared {
		t.Fatal("scan changed held runtime or lost preparation checkpoint", err)
	}
	f.call("root", "default", "PUT", "/api/workloads/scan-resource/image-scanning", map[string]bool{"enabled": false, "notify": false}, 200)
	if err := f.s.prepareInfrastructure(t.Context(), &request); err == nil {
		t.Fatal("dispatch did not recheck opt-out")
	}
}

func TestImageScansMergeExplicitEventsAndRuntimeFreshness(t *testing.T) {
	for _, event := range []string{"image.vulnerabilities", "image.scan_completed", "image.scan_unknown"} {
		if notificationMatches(&notificationConfig{Enabled: true, Success: true, Failure: true}, event) || !notificationMatches(&notificationConfig{Enabled: true, Events: []string{event}}, event) {
			t.Fatal("scan event escaped explicit routing", event)
		}
	}
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('scan-node','Node','localhost',22,'',$1)`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('scan-project','Project','default')`)
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,team_id,project_id) VALUES('scan-resource','scan-node','Resource',$1,'default','scan-project')`, []byte{})
	now := time.Now().UTC()
	digest := "sha256:" + strings.Repeat("a", 64)
	report := worker.ImageScan{State: "complete", ScannerImage: worker.ScannerImage, ScannerVersion: "0.76.2", CheckedAt: now, Database: worker.ScanDatabase{Digest: digest, UpdatedAt: now.Add(-time.Hour), NextUpdate: now.Add(time.Hour)}, Images: []worker.ScannedImage{{ImageID: digest, ConfigDigest: digest, State: "complete", Coverage: worker.ScanCoverage{OS: "scanned", OtherLanguages: "not_detected", JavaArchives: "not_detected"}}}}
	data, _ := json.Marshal(worker.Operation{ImageScan: &report})
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES('scan-result','scan-resource','image_scan','succeeded',$1,$2)`, []byte{}, data)
	for _, action := range []string{"move", "rollout_promote", "rollout_abort", "suspend"} {
		f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,updated_at) VALUES($1,'scan-resource',$2,'succeeded',$3,$4)`, "after-"+action, action, []byte{}, now.Add(time.Second))
		response := f.call("root", "default", "GET", "/api/workloads/scan-resource/image-scans", nil, 200)
		if jsonString(t, response, "state") != "unknown" || !strings.Contains(response.Body.String(), `"stale":true`) {
			t.Fatal("runtime transition preserved an old clean result", action)
		}
		f.sql(`DELETE FROM operations WHERE id=$1`, "after-"+action)
	}
}
