package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestImageScanningSettingsStayWithinTeamNodePair(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("scan-owner", "scan-owner@example.com")
	other := f.user("scan-other", "scan-other@example.com")
	f.sql(`INSERT INTO teams(id,name) VALUES('scan-a','A'),('scan-b','B')`)
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('scan-a','scan-owner','owner','[]'),('scan-b','scan-other','owner','[]')`)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('scan-shared','Shared','localhost',22,'',$1,'ready','{"image_scanning":true}'),('scan-other-node','Other','localhost',22,'',$1,'ready','{"image_scanning":true}')`, []byte{})
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('scan-a','scan-shared'),('scan-b','scan-shared'),('scan-a','scan-other-node')`)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('scan-pa','A','scan-a'),('scan-pb','B','scan-b')`)
	sealed, _ := seal(f.s.Key, []byte(`{"kind":"service"}`))
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,team_id,project_id,manually_stopped) VALUES('scan-wa','scan-shared','A',$1,'scan-a','scan-pa',false),('scan-wa2','scan-shared','A2',$1,'scan-a','scan-pa',true),('scan-wb','scan-shared','B',$1,'scan-b','scan-pb',false),('scan-wc','scan-other-node','C',$1,'scan-a','scan-pa',false)`, sealed)
	f.call(owner, "scan-a", "PUT", "/api/workloads/scan-wa/image-scanning", map[string]any{"enabled": true, "notify": false}, 200)
	for _, test := range []struct {
		token, team, id string
		enabled         bool
	}{{owner, "scan-a", "scan-wa2", true}, {other, "scan-b", "scan-wb", false}, {owner, "scan-a", "scan-wc", false}} {
		response := f.call(test.token, test.team, "GET", "/api/workloads/"+test.id+"/image-scanning", nil, 200)
		var settings struct{ Enabled bool }
		if json.Unmarshal(response.Body.Bytes(), &settings) != nil || settings.Enabled != test.enabled {
			t.Fatal("settings escaped their team/node pair", test.id, response.Body.String())
		}
	}
	f.call(owner, "scan-a", "GET", "/api/workloads/scan-wb/image-scanning", nil, 404)
	f.call(owner, "scan-a", "GET", "/api/workloads/scan-wb/image-scans", nil, 404)
	f.call(owner, "scan-a", "PUT", "/api/workloads/scan-wb/image-scanning", map[string]any{"enabled": true, "notify": false}, 404)
	f.call(owner, "scan-a", "POST", "/api/workloads/scan-wa2/operations", map[string]any{"action": "image_scan"}, 202)
	var stopped bool
	if err := f.s.DB.QueryRow(context.Background(), `SELECT manually_stopped FROM workloads WHERE id='scan-wa2'`).Scan(&stopped); err != nil || !stopped {
		t.Fatal("metadata scan cleared manual stop intent", stopped, err)
	}
}

func TestImageScanningOptInAndMinimalRequest(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('scan-node','Node','localhost',22,'',$1,'ready','{"image_scanning":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('scan-project','Project','default')`)
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "App", NodeID: "scan-node", ProjectID: "scan-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n", Environment: map[string]string{"PRIVATE_VALUE": "never-in-scan"}}, 201), "id")
	path := "/api/workloads/" + id
	f.call("root", "default", "POST", path+"/operations", map[string]any{"action": "image_scan"}, 409)
	operator := f.user("scan-operator", "scan-operator@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','scan-operator','member','[{"scope":"team","role":"operator"}]')`)
	f.call(operator, "default", "GET", path+"/image-scanning", nil, 200)
	f.call(operator, "default", "PUT", path+"/image-scanning", map[string]any{"enabled": true, "notify": false}, 403)
	f.call("root", "default", "PUT", path+"/image-scanning", map[string]any{"enabled": true, "notify": false}, 200)
	queued := jsonString(t, f.call(operator, "default", "POST", path+"/operations", map[string]any{"action": "image_scan"}, 202), "id")
	var encrypted []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE id=$1", queued).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	plain, err := unseal(f.s.Key, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	var request worker.Request
	if json.Unmarshal(plain, &request) != nil || request.Action != "image_scan" || request.Compose != "" || len(request.Environment) != 0 || request.Registry != nil || request.Source != nil || strings.Contains(string(plain), "never-in-scan") {
		t.Fatal("scan request contains deployment or credential input")
	}
	f.sql(`UPDATE operations SET state='cancelled' WHERE id=$1`, queued)
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='scan-node'`)
	f.call("root", "default", "PUT", path+"/image-scanning", map[string]any{"enabled": true, "notify": false}, 409)
	f.call("root", "default", "PUT", path+"/image-scanning", map[string]any{"enabled": false, "notify": false}, 200)
}

func TestImageScanningNotificationEventsAndOptOut(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	connection := jsonString(t, f.call("root", "default", "POST", "/api/notifications", connectionConfig{Name: "Scans", Notification: &notificationConfig{Provider: "webhook", URL: "https://notify.example.test/fixture-only", Enabled: true, Events: []string{"image.vulnerabilities", "image.scan_completed", "image.scan_unknown"}}}, 201), "id")
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('scan-notify-node','Node','localhost',22,'',$1)`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('scan-notify-project','Project','default')`)
	sealed, _ := seal(f.s.Key, []byte(`{}`))
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('scan-notify-resource','scan-notify-node','Resource',$1,'scan-notify-project','default')`, sealed)
	finish := func(id string, report *worker.ImageScan) {
		t.Helper()
		data, _ := json.Marshal(worker.Operation{ImageScan: report})
		f.sql(`INSERT INTO operations(id,workload_id,action,payload,result) VALUES($1,'scan-notify-resource','image_scan',$2,$3)`, id, sealed, data)
		f.sql(`UPDATE operations SET state='succeeded' WHERE id=$1`, id)
	}
	finish("scan-no-notify", nil)
	var count int
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries WHERE operation_id='scan-no-notify'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("scan notifications were not opt-in", count, err)
	}
	f.sql(`INSERT INTO image_scanning_settings(team_id,node_id,enabled,notify) VALUES('default','scan-notify-node',true,true)`)
	now := time.Now().UTC()
	id := "sha256:" + strings.Repeat("a", 64)
	report := &worker.ImageScan{State: "complete", ScannerImage: worker.ScannerImage, ScannerVersion: "0.76.2", CheckedAt: now, Database: worker.ScanDatabase{Digest: id, UpdatedAt: now.Add(-time.Hour), NextUpdate: now.Add(time.Hour)}, Images: []worker.ScannedImage{{Service: "app", ImageID: id, ConfigDigest: id, State: "complete", Counts: map[string]int{"HIGH": 1}, Coverage: worker.ScanCoverage{OS: "scanned", OtherLanguages: "not_detected", JavaArchives: "not_detected"}, Findings: []worker.ScanFinding{{ID: "CVE-2026-12345", Package: "pkg", Installed: "1", Severity: "HIGH"}}}}}
	finish("scan-with-findings", report)
	var events []string
	f.s.NotificationTransport = notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
		var event map[string]string
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(event["message"], "CVE-2026-12345") || !strings.Contains(event["message"], "HIGH: 1") {
			t.Fatal("notification lost bounded CVE/severity summary", event)
		}
		events = append(events, event["event"])
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if err := f.s.deliverNotification(ctx, "scan-with-findings", connection, 1); err != nil {
		t.Fatal(err)
	}
	report.Images[0].Coverage.JavaArchives = "not_scanned"
	finish("scan-partial-java", report)
	if err := f.s.deliverNotification(ctx, "scan-partial-java", connection, 1); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "image.vulnerabilities,image.scan_unknown" {
		t.Fatal("partial Java coverage appeared complete", events)
	}
	report.Images[0].Findings[0].ID = "CVE-@everyone\nprivate"
	finish("scan-invalid-advisory", report)
	_, summary, allowed, err := f.s.scanNotification(ctx, "scan-invalid-advisory", "image_scan")
	if err != nil || !allowed || strings.Contains(summary, "private") || strings.Contains(summary, "@everyone") {
		t.Fatal("notification forwarded an unvalidated advisory identifier", summary, allowed, err)
	}
	finish("scan-notify-disabled", report)
	f.sql(`UPDATE image_scanning_settings SET notify=false WHERE node_id='scan-notify-node'`)
	if err := f.s.deliverNotification(ctx, "scan-notify-disabled", connection, 1); err != nil || len(events) != 2 {
		t.Fatal("pending scan notification ignored opt-out", events, err)
	}
}

func TestImageScanningUnknownStaleAndJavaCoverage(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('scan-node','Node','localhost',22,'',$1,'ready','{"image_scanning":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('scan-project','Project','default')`)
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "App", NodeID: "scan-node", ProjectID: "scan-project", Kind: "service", Compose: "services: {}"}, 201), "id")
	path := "/api/workloads/" + id + "/image-scans"
	if value := jsonString(t, f.call("root", "default", "GET", path, nil, 200), "state"); value != "unknown" {
		t.Fatal(value)
	}
	now := time.Now().UTC()
	report := worker.ImageScan{ScannerImage: worker.ScannerImage, ScannerVersion: "0.76.2", State: "complete", CheckedAt: now, Database: worker.ScanDatabase{Digest: "sha256:" + strings.Repeat("a", 64), UpdatedAt: now.Add(-time.Hour), NextUpdate: now.Add(time.Hour)}, Images: []worker.ScannedImage{{Service: "app", ImageID: "sha256:" + strings.Repeat("a", 64), ConfigDigest: "sha256:" + strings.Repeat("b", 64), State: "complete", Coverage: worker.ScanCoverage{OS: "scanned", OtherLanguages: "not_detected", JavaArchives: "not_detected"}}}}
	result := worker.Operation{ImageScan: &report}
	data, _ := json.Marshal(result)
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES('scan-result',$1,'image_scan','succeeded',$2,$3)`, id, []byte{}, data)
	if value := jsonString(t, f.call("root", "default", "GET", path, nil, 200), "state"); value != "complete" {
		t.Fatal(value)
	}
	report.Images[0].Coverage.JavaArchives = "not_scanned"
	report.Images[0].Coverage.JavaMessage = "Java dependency coverage unknown."
	data, _ = json.Marshal(result)
	f.sql(`UPDATE operations SET result=$1 WHERE id='scan-result'`, data)
	response := f.call("root", "default", "GET", path, nil, 200)
	if jsonString(t, response, "state") != "unknown" || !strings.Contains(response.Body.String(), `"java_archives":"not_scanned"`) {
		t.Fatal(response.Body.String())
	}
	report.State = "complete"
	report.Database.NextUpdate = now.Add(-time.Second)
	data, _ = json.Marshal(result)
	f.sql(`UPDATE operations SET result=$1 WHERE id='scan-result'`, data)
	if value := jsonString(t, f.call("root", "default", "GET", path, nil, 200), "state"); value != "unknown" {
		t.Fatal("stale database appeared complete", value)
	}
}
