package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func newAutomaticScanFixture(t *testing.T) (*accountFixture, string, *worker.ScanDatabaseStatus, map[string]*worker.Request) {
	t.Helper()
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('auto-scan-node','Node','localhost',22,'',$1,'ready','{"image_scanning":true,"automatic_image_scans":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('auto-scan-project','Project','default')`)
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "App", NodeID: "auto-scan-node", ProjectID: "auto-scan-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"}, 201), "id")
	payload, err := f.s.sealJSON(worker.Request{ID: "original-deploy", Workload: id, Action: "deploy", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,created_at,updated_at) VALUES('original-deploy',$1,'deploy','succeeded',$2,now()-interval '2 minutes',now()-interval '2 minutes')`, id, payload)
	f.sql(`UPDATE workloads SET manually_stopped=false WHERE id=$1`, id)
	image := "sha256:" + strings.Repeat("a", 64)
	now := time.Now().UTC()
	database := &worker.ScanDatabaseStatus{Fresh: true, Database: worker.ScanDatabase{Digest: "sha256:" + strings.Repeat("b", 64), UpdatedAt: now.Add(-time.Hour), NextUpdate: now.Add(time.Hour)}}
	requests := map[string]*worker.Request{}
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "auto-scan-node" {
			t.Fatal("wrong scan node", node)
		}
		switch r.Method {
		case "image_scan_database":
			return worker.RPCResponse{ScanDatabase: database}, nil
		case "image_scan_observe":
			return worker.RPCResponse{ScanObservation: &worker.ScanObservation{Eligible: true, Images: map[string][]string{"app": {image}}}}, nil
		case "submit_automatic_image_scan":
			if r.Request == nil || r.Request.AutomaticScan == nil || r.Request.Source != nil || len(r.Request.Environment) > 0 || r.Request.Registry != nil {
				t.Fatal("scan leaked deployment inputs", r.Method)
			}
			copy := *r.Request
			requests[copy.ID] = &copy
			return worker.RPCResponse{Operation: &worker.Operation{ID: copy.ID, Workload: copy.Workload, Action: "image_scan", State: "queued", ScanOrigin: copy.AutomaticScan.Origin}}, nil
		case "cancel":
			return worker.RPCResponse{Operation: &worker.Operation{ID: r.Request.ID, Workload: r.Request.Workload, Action: "image_scan", State: "cancelled", ScanOrigin: r.Request.AutomaticScan.Origin}}, nil
		default:
			t.Fatal("unexpected automatic scan RPC", r.Method)
			return worker.RPCResponse{}, nil
		}
	}
	return f, id, database, requests
}

func enableAutomaticFixture(t *testing.T, f *accountFixture, id string) {
	t.Helper()
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/image-scanning", map[string]any{"enabled": true, "notify": true, "automatic": true}, 200)
}

func automaticLeaseCount(t *testing.T, f *accountFixture) int {
	t.Helper()
	var count int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM automatic_image_scans`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestAutomaticImageScansDefaultOffAndDeployNeverWaits(t *testing.T) {
	f, id, _, requests := newAutomaticScanFixture(t)
	ctx := context.Background()
	if err := f.s.scheduleAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	if automaticLeaseCount(t, f) != 0 {
		t.Fatal("automatic scans defaulted on")
	}
	enableAutomaticFixture(t, f, id)
	if err := f.s.scheduleAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	if automaticLeaseCount(t, f) != 1 {
		t.Fatal("post-deploy scan was not coalesced")
	}
	if err := f.s.reconcileAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatal(requests)
	}
	for _, r := range requests {
		if r.AutomaticScan.Origin != "post_deploy" {
			t.Fatal(r)
		}
	}
	queued := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]any{"action": "deploy"}, 202), "id")
	if queued == "" {
		t.Fatal("foreground deploy was held by automatic scan")
	}
	if err := f.s.reconcileAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	if automaticLeaseCount(t, f) != 0 {
		t.Fatal("automatic cancellation did not settle")
	}
	var count int
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM operations WHERE workload_id=$1 AND action='image_scan' AND state='cancelled' AND result->>'scan_origin'='post_deploy'`, id).Scan(&count); err != nil || count != 1 {
		t.Fatal("origin/cancel history lost", count, err)
	}
}

func TestAutomaticImageScansGenerationCoverageAndCoalescing(t *testing.T) {
	f, id, database, _ := newAutomaticScanFixture(t)
	ctx := context.Background()
	enableAutomaticFixture(t, f, id)
	image := "sha256:" + strings.Repeat("a", 64)
	report := worker.ImageScan{ScannerImage: worker.ScannerImage, ScannerVersion: "0.76.2", State: "complete", CheckedAt: time.Now().UTC(), Database: database.Database, Images: []worker.ScannedImage{{Service: "app", ImageID: image, Identity: image, ConfigDigest: image, State: "complete", Coverage: worker.ScanCoverage{OS: "scanned", OtherLanguages: "not_detected", JavaArchives: "not_detected"}}}}
	result, _ := json.Marshal(worker.Operation{ImageScan: &report, ScanOrigin: "manual"})
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES('covered',$1,'image_scan','succeeded',$2,$3)`, id, []byte{}, result)
	if err := f.s.scheduleAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	if automaticLeaseCount(t, f) != 0 {
		t.Fatal("fresh same-image result was scanned again")
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES('later-failed-service-deploy',$1,'deploy','failed',$2)`, id, []byte{})
	database.Database.Digest = "sha256:" + strings.Repeat("c", 64)
	f.sql(`UPDATE image_scan_automation_state SET next_check_at=now()`)
	if err := f.s.scheduleAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	if automaticLeaseCount(t, f) != 1 {
		t.Fatal("changed DB generation did not schedule")
	}
	for range 5 {
		f.sql(`UPDATE image_scan_automation_state SET next_check_at=now()`)
		if err := f.s.scheduleAutomaticScans(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if automaticLeaseCount(t, f) != 1 {
		t.Fatal("burst queued unbounded scans")
	}
	var payload []byte
	if err := f.s.DB.QueryRow(ctx, `SELECT payload FROM automatic_image_scans`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var request worker.Request
	if err := f.s.unsealJSON(payload, &request); err != nil || request.AutomaticScan.Origin != "scheduled" {
		t.Fatal(request, err)
	}
	baseRPC := f.s.workerRPC
	f.s.workerRPC = func(ctx context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method != "submit_automatic_image_scan" {
			return baseRPC(ctx, node, r)
		}
		report.Database = database.Database
		report.CheckedAt = time.Now().UTC()
		return worker.RPCResponse{Operation: &worker.Operation{ID: r.Request.ID, Workload: id, Action: "image_scan", State: "succeeded", ScanOrigin: "scheduled", ImageScan: &report}}, nil
	}
	if err := f.s.reconcileAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	if automaticLeaseCount(t, f) != 0 {
		t.Fatal("completed lease not released")
	}
	f.sql(`UPDATE image_scan_automation_state SET next_check_at=now()`)
	if err := f.s.scheduleAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	if automaticLeaseCount(t, f) != 0 {
		t.Fatal("same generation repeatedly rescanned")
	}
}

func TestAutomaticImageScansMissingDatabaseAndIntentHolds(t *testing.T) {
	f, id, database, _ := newAutomaticScanFixture(t)
	ctx := context.Background()
	enableAutomaticFixture(t, f, id)
	database.Fresh = false
	for range 3 {
		f.sql(`UPDATE image_scan_automation_state SET next_check_at=now()`)
		if err := f.s.scheduleAutomaticScans(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if automaticLeaseCount(t, f) != 0 {
		t.Fatal("missing DB queued scan storm")
	}
	database.Fresh = true
	f.sql(`UPDATE workloads SET manually_stopped=true WHERE id=$1`, id)
	f.sql(`UPDATE image_scan_automation_state SET next_check_at=now()`)
	if err := f.s.scheduleAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	if automaticLeaseCount(t, f) != 0 {
		t.Fatal("manual stop was scanned")
	}
	f.sql(`UPDATE workloads SET manually_stopped=false WHERE id=$1`, id)
	f.sql(`UPDATE image_scanning_settings SET automatic=false`)
	if err := f.s.scheduleAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	if automaticLeaseCount(t, f) != 0 {
		t.Fatal("automatic opt-out ignored")
	}
}

func TestAutomaticImageScansNotifyOnlyNewStoredFindings(t *testing.T) {
	f, id, database, _ := newAutomaticScanFixture(t)
	ctx := context.Background()
	enableAutomaticFixture(t, f, id)
	image := "sha256:" + strings.Repeat("a", 64)
	report := worker.ImageScan{ScannerImage: worker.ScannerImage, ScannerVersion: "0.76.2", State: "complete", CheckedAt: time.Now().UTC(), Database: database.Database, Images: []worker.ScannedImage{{Service: "app", ImageID: image, Identity: image, ConfigDigest: image, State: "complete", Coverage: worker.ScanCoverage{OS: "scanned", OtherLanguages: "not_detected", JavaArchives: "not_detected"}, Counts: map[string]int{"HIGH": 1}, Findings: []worker.ScanFinding{{ID: "CVE-2026-12345", Package: "fixture", Installed: "1", Severity: "HIGH"}}}}}
	finish := func(operation string) {
		t.Helper()
		report.CheckedAt = time.Now().UTC()
		result, _ := json.Marshal(worker.Operation{ImageScan: &report})
		f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES($1,$2,'image_scan','succeeded',$3,$4)`, operation, id, []byte{}, result)
	}
	finish("first-scan")
	_, summary, allowed, err := f.s.scanNotification(ctx, "first-scan", "image_scan")
	if err != nil || !allowed || !strings.Contains(summary, "HIGH: 1") {
		t.Fatal(summary, allowed, err)
	}
	report.Database.Digest = "sha256:" + strings.Repeat("d", 64)
	finish("same-findings")
	if _, _, allowed, err = f.s.scanNotification(ctx, "same-findings", "image_scan"); err != nil || allowed {
		t.Fatal("repeat notification", allowed, err)
	}
	report.Images[0].Findings = append(report.Images[0].Findings, worker.ScanFinding{ID: "CVE-2026-99999", Package: "new-fixture", Installed: "1", Severity: "HIGH"})
	finish("new-finding")
	_, summary, allowed, err = f.s.scanNotification(ctx, "new-finding", "image_scan")
	if err != nil || !allowed || !strings.Contains(summary, "CVE-2026-99999") || strings.Contains(summary, "CVE-2026-12345") || !strings.Contains(summary, "HIGH: 1") {
		t.Fatal(summary, allowed, err)
	}
	report.Images[0].Truncated = true
	finish("truncated-baseline")
	report.Images[0].Findings = append(report.Images[0].Findings, worker.ScanFinding{ID: "CVE-2026-88888", Package: "uncertain", Installed: "1", Severity: "HIGH"})
	finish("unprovable-new")
	if _, _, allowed, err = f.s.scanNotification(ctx, "unprovable-new", "image_scan"); err != nil || allowed {
		t.Fatal("truncated baseline invented new findings", allowed, err)
	}
}

func TestAutomaticImageScansCapabilityOptOutAndUnknownAttempt(t *testing.T) {
	f, id, _, _ := newAutomaticScanFixture(t)
	ctx := context.Background()
	f.sql(`UPDATE nodes SET capabilities='{"image_scanning":true}' WHERE id='auto-scan-node'`)
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/image-scanning", map[string]any{"enabled": true, "notify": false, "automatic": true}, 409)
	f.sql(`UPDATE nodes SET capabilities='{"image_scanning":true,"automatic_image_scans":true}' WHERE id='auto-scan-node'`)
	enableAutomaticFixture(t, f, id)
	if err := f.s.scheduleAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	base := f.s.workerRPC
	f.s.workerRPC = func(ctx context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method == "submit_automatic_image_scan" {
			return worker.RPCResponse{Operation: &worker.Operation{ID: r.Request.ID, Workload: id, Action: "image_scan", State: "succeeded", ScanOrigin: "post_deploy", ImageScan: &worker.ImageScan{State: "unknown", CheckedAt: time.Now().UTC(), Message: "fixture missing package coverage"}}}, nil
		}
		return base(ctx, node, r)
	}
	if err := f.s.reconcileAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE image_scan_automation_state SET next_check_at=now()`)
	if err := f.s.scheduleAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	if automaticLeaseCount(t, f) != 0 {
		t.Fatal("unknown result caused repeated same-identity attempts")
	}
	f.sql(`UPDATE nodes SET capabilities='{"image_scanning":true}' WHERE id='auto-scan-node'`)
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/image-scanning", map[string]any{"enabled": true, "notify": false, "automatic": false}, 200)
	// Legacy settings writers preserve an omitted automatic choice.
	f.sql(`UPDATE image_scanning_settings SET automatic=true`)
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/image-scanning", map[string]any{"enabled": true, "notify": false}, 200)
	var automatic bool
	if err := f.s.DB.QueryRow(ctx, `SELECT automatic FROM image_scanning_settings`).Scan(&automatic); err != nil || !automatic {
		t.Fatal("legacy save changed automatic preference", err)
	}
}

func TestAutomaticImageScansDisableCancelsAndHistoryUsesRequestOrder(t *testing.T) {
	f, id, _, _ := newAutomaticScanFixture(t)
	ctx := context.Background()
	enableAutomaticFixture(t, f, id)
	if err := f.s.scheduleAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	var automatic string
	if err := f.s.DB.QueryRow(ctx, `SELECT id FROM automatic_image_scans`).Scan(&automatic); err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES('later-manual',$1,'image_scan','succeeded',$2,'{"scan_origin":"manual"}')`, id, []byte{})
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/image-scanning", map[string]any{"enabled": true, "notify": false, "automatic": false}, 200)
	if err := f.s.reconcileAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	if automaticLeaseCount(t, f) != 0 {
		t.Fatal("disabled automatic lease did not cancel")
	}
	response := f.call("root", "default", "GET", "/api/workloads/"+id+"/image-scans", nil, 200)
	if jsonString(t, response, "operation_id") != "later-manual" {
		t.Fatal("late automatic import hid a newer manual result", response)
	}
	var state string
	if err := f.s.DB.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1`, automatic).Scan(&state); err != nil || state != "cancelled" {
		t.Fatal(state, err)
	}
}

func TestAutomaticImageScansNotificationWaitsForEarlierBaseline(t *testing.T) {
	f, id, db, _ := newAutomaticScanFixture(t)
	ctx := context.Background()
	enableAutomaticFixture(t, f, id)
	if err := f.s.scheduleAutomaticScans(ctx); err != nil {
		t.Fatal(err)
	}
	report := &worker.ImageScan{Database: db.Database, Images: []worker.ScannedImage{{ImageID: "sha256:" + strings.Repeat("a", 64)}}}
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES('later-manual',$1,'image_scan','succeeded',$2,'{}')`, id, []byte{})
	if _, err := f.s.newScanFindings(ctx, "later-manual", report); err != errScanBaselinePending {
		t.Fatal("notification raced earlier background baseline", err)
	}
}

func TestAutomaticImageScansNodeAndControllerBoundsIncludeJobs(t *testing.T) {
	f, _, database, _ := newAutomaticScanFixture(t)
	ctx := context.Background()
	for n := range 6 {
		node := fmt.Sprintf("auto-extra-%d", n)
		f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES($1,$1,'localhost',22,'',$2,'ready','{"image_scanning":true,"automatic_image_scans":true}')`, node, []byte{})
		for range 2 {
			id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Completed job", NodeID: node, ProjectID: "auto-scan-project", Kind: "job", JobService: "task", Compose: "services:\n  task:\n    image: busybox:1.37\n"}, 201), "id")
			f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES($1,$2,'deploy','succeeded',$3)`, resourceID(), id, []byte{})
			f.sql(`UPDATE workloads SET manually_stopped=false WHERE id=$1`, id)
		}
		f.sql(`INSERT INTO image_scanning_settings(team_id,node_id,enabled,notify,automatic) VALUES('default',$1,true,false,true)`, node)
	}
	image := "sha256:" + strings.Repeat("a", 64)
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		switch r.Method {
		case "image_scan_database":
			return worker.RPCResponse{ScanDatabase: database}, nil
		case "image_scan_observe":
			if r.Request.Kind != "job" || r.Request.JobService != "task" {
				t.Fatal("job identity lost", r.Request)
			}
			return worker.RPCResponse{ScanObservation: &worker.ScanObservation{Eligible: true, Images: map[string][]string{"task": {image}}}}, nil
		default:
			t.Fatal("unexpected scheduler mutation", r.Method)
			return worker.RPCResponse{}, nil
		}
	}
	for range 8 {
		if err := f.s.scheduleAutomaticScans(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if count := automaticLeaseCount(t, f); count != 4 {
		t.Fatal("controller bound", count)
	}
	var nodes int
	if err := f.s.DB.QueryRow(ctx, `SELECT count(DISTINCT node_id) FROM automatic_image_scans`).Scan(&nodes); err != nil || nodes != 4 {
		t.Fatal("node bound", nodes, err)
	}
	var payload []byte
	if err := f.s.DB.QueryRow(ctx, `SELECT payload FROM automatic_image_scans LIMIT 1`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var request worker.Request
	if err := f.s.unsealJSON(payload, &request); err != nil || request.Kind != "job" || request.AutomaticScan.Origin != "post_deploy" {
		t.Fatal("completed job was not scheduled", request, err)
	}
}

func TestAutomaticImageScansOriginalNodeCannotBeDeletedDuringLease(t *testing.T) {
	f, id, _, _ := newAutomaticScanFixture(t)
	enableAutomaticFixture(t, f, id)
	if err := f.s.scheduleAutomaticScans(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('replacement-node','Replacement','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`UPDATE workloads SET node_id='replacement-node' WHERE id=$1`, id)
	f.call("root", "default", "DELETE", "/api/nodes/auto-scan-node", nil, 409)
	if automaticLeaseCount(t, f) != 1 {
		t.Fatal("original scan ownership was discarded")
	}
}

func TestAutomaticImageScansDoesNotReuseOtherPlatformOfIndex(t *testing.T) {
	f, id, db, _ := newAutomaticScanFixture(t)
	enableAutomaticFixture(t, f, id)
	index := "sha256:" + strings.Repeat("a", 64)
	report := worker.ImageScan{State: "complete", ScannerImage: worker.ScannerImage, ScannerVersion: "0.76.2", Database: db.Database, CheckedAt: time.Now().UTC(), Images: []worker.ScannedImage{{Service: "app", ImageID: index, Identity: "sha256:" + strings.Repeat("c", 64), ConfigDigest: "sha256:" + strings.Repeat("d", 64)}}}
	result, _ := json.Marshal(worker.Operation{ImageScan: &report})
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES('other-platform',$1,'image_scan','succeeded',$2,$3)`, id, []byte{}, result)
	if err := f.s.scheduleAutomaticScans(context.Background()); err != nil {
		t.Fatal(err)
	}
	if automaticLeaseCount(t, f) != 1 {
		t.Fatal("index digest hid an uncovered platform image")
	}
}
