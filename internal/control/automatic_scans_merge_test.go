package control

import (
	"cove/internal/outbound"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAutomaticScansMergeNotificationOutcomes(t *testing.T) {
	f, id, database, _ := newAutomaticScanFixture(t)
	enableAutomaticFixture(t, f, id)
	image := "sha256:" + strings.Repeat("a", 64)
	report := func(outcome string, findings ...worker.ScanFinding) *worker.ImageScan {
		value := &worker.ImageScan{State: "complete", ScannerImage: worker.ScannerImage, ScannerVersion: "0.76.2", Database: database.Database, CheckedAt: time.Now().UTC(), Images: []worker.ScannedImage{{Service: "app", ImageID: image, Identity: image, ConfigDigest: image, State: "complete", Coverage: worker.ScanCoverage{OS: "scanned", OtherLanguages: "not_detected", JavaArchives: "not_detected"}, Counts: map[string]int{}, Findings: findings}}}
		for _, finding := range findings {
			value.Images[0].Counts[finding.Severity]++
		}
		if outcome == "unknown" {
			value.State = "unknown"
			value.Images[0].State = "unknown"
			value.Images[0].Coverage.JavaArchives = "not_scanned"
		}
		return value
	}
	index := 0
	finish := func(name, origin string, scan *worker.ImageScan, wanted string) {
		t.Helper()
		index++
		request := worker.Request{ID: name, Workload: id, Action: "image_scan", Kind: "service"}
		if automaticScanOrigin(origin) {
			request.AutomaticScan = &worker.AutomaticScan{Origin: origin, Database: database.Database.Digest, Images: map[string][]string{"app": {image}}}
		}
		payload, err := f.s.sealJSON(request)
		if err != nil {
			t.Fatal(err)
		}
		state := "succeeded"
		if scan == nil {
			state = "failed"
		}
		data, err := json.Marshal(worker.Operation{ImageScan: scan, ScanOrigin: origin})
		if err != nil {
			t.Fatal(err)
		}
		f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result,created_at) VALUES($1,$2,'image_scan',$3,$4,$5,$6)`, name, id, state, payload, data, time.Now().Add(-time.Minute).Add(time.Duration(index)*time.Millisecond))
		event, _, allowed, err := f.s.scanNotification(t.Context(), name, "image_scan")
		if err != nil || allowed != (wanted != "") || allowed && event != wanted {
			t.Fatalf("%s: event=%q allowed=%v want=%q error=%v", name, event, allowed, wanted, err)
		}
	}
	first := worker.ScanFinding{ID: "CVE-2026-12345", Package: "fixture", Installed: "1", Severity: "HIGH"}
	second := worker.ScanFinding{ID: "CVE-2026-23456", Package: "other", Installed: "1", Severity: "HIGH"}
	finish("first-clean", "post_deploy", report("clean"), "image.scan_completed")
	database.Database.Digest = "sha256:" + strings.Repeat("d", 64)
	finish("repeat-clean", "scheduled", report("clean"), "")
	finish("clean-to-findings", "scheduled", report("vulnerable", first), "image.vulnerabilities")
	finish("vulnerable-to-clean", "scheduled", report("clean"), "image.scan_completed")
	finish("first-unknown", "scheduled", report("unknown"), "image.scan_unknown")
	finish("repeat-unknown", "scheduled", report("unknown"), "")
	finish("unknown-to-clean", "scheduled", report("clean"), "image.scan_completed")
	finish("clean-to-unknown", "scheduled", report("unknown"), "image.scan_unknown")
	finish("unknown-to-new-findings", "scheduled", report("vulnerable", second), "image.vulnerabilities")
	finish("vulnerable-to-unknown", "scheduled", report("unknown", second), "image.scan_unknown")
	finish("same-partial-findings", "scheduled", report("unknown", second), "")
	finish("manual-clean-one", "manual", report("clean"), "image.scan_completed")
	finish("manual-clean-two", "manual", report("clean"), "image.scan_completed")
	finish("manual-findings-one", "manual", report("vulnerable", second), "image.vulnerabilities")
	finish("manual-findings-two", "manual", report("vulnerable", second), "image.vulnerabilities")
	finish("manual-unknown-one", "manual", report("unknown"), "image.scan_unknown")
	finish("manual-unknown-two", "manual", report("unknown"), "image.scan_unknown")
	finish("auto-after-manual", "scheduled", report("unknown", second), "")
	requestImage := "sha256:" + strings.Repeat("c", 64)
	image = requestImage
	finish("failed-without-report", "post_deploy", nil, "image.scan_unknown")
	finish("repeat-failed-without-report", "scheduled", nil, "")
}

func TestAutomaticScansMergeRefreshRequiresProxyCapableWorker(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,mode,state,capabilities) VALUES('old-auto','Old worker','localhost',22,'',$1,'local','ready','{"automatic_image_scans":true,"image_scanning":true}')`, []byte{})
	proxy := outbound.Config{HTTPS: "http://fixture:opaque@proxy.example.invalid:8080"}
	_, err := f.s.nodeRPCUsing(t.Context(), f.s.DB, "old-auto", worker.RPCRequest{Method: "image_scan_database", RefreshScanDatabase: true}, proxy)
	if !errors.Is(err, errOutboundWorkerUnsupported) {
		t.Fatal("refresh dropped the required proxy instead of refusing the worker", err)
	}
}
