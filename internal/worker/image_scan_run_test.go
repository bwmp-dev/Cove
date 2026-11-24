package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageScanFailedCleanupSurvivesTerminalJournalAndRecovery(t *testing.T) {
	t.Setenv("COVE_IMAGE_SCANNING", "1")
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		return errors.New("fixture daemon unavailable")
	}
	r := Request{ID: "cleanup-scan", Workload: "app", Action: "image_scan", Kind: "service"}
	if _, err = e.Submit(r); err != nil {
		t.Fatal(err)
	}
	j, _ := e.read(r.ID)
	e.execute(context.Background(), j)
	j, err = e.read(r.ID)
	if err != nil || !j.ImageScanCleanup || j.Operation.State != "failed" {
		t.Fatal("cleanup intent lost from terminal scan", j, err)
	}
	if err = e.Recover(); err == nil {
		t.Fatal("unconfirmed cleanup was forgotten")
	}
	input := filepath.Join(e.Root, "scanner-inputs", r.ID+"-orphan")
	if err = os.MkdirAll(input, 0700); err != nil {
		t.Fatal(err)
	}
	queries := 0
	e.Run = func(_ context.Context, _ string, args, _ []string, _ io.Writer) error {
		if args[1] != "ps" {
			t.Fatal("recovery attempted non-inventory action for absent containers", args)
		}
		queries++
		return nil
	}
	if err = e.Recover(); err != nil {
		t.Fatal(err)
	}
	j, err = e.read(r.ID)
	if err != nil || j.ImageScanCleanup || queries != 2 || j.Operation.State != "failed" {
		t.Fatal("recovery did not settle exact scan cleanup", j, queries, err)
	}
	if _, err = os.Stat(input); !os.IsNotExist(err) {
		t.Fatal("recovery left operation input", err)
	}
}

func TestImageScanWorkerSwitchAndJournalResult(t *testing.T) {
	t.Setenv("COVE_IMAGE_SCANNING", "")
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		t.Fatal("disabled node attempted a Docker command")
		return nil
	}
	r := Request{ID: "scan-off", Workload: "app", Action: "image_scan", Kind: "service"}
	if _, err = e.Submit(r); err != nil {
		t.Fatal(err)
	}
	j, _ := e.read(r.ID)
	e.execute(context.Background(), j)
	operation, err := e.Status(r.ID)
	if err != nil || operation.ImageScan == nil || operation.ImageScan.State != "unknown" || operation.ImageScan.ScannerImage != ScannerImage {
		t.Fatal(operation, err)
	}
	r.ID = "scan-secret"
	r.Environment = map[string]string{"TOKEN": "must-not-be-forwarded"}
	if err = ValidateRequest(r); err == nil {
		t.Fatal("scan accepted deployment secrets")
	}
}

func TestImageScanCapacityWaitCanBeCancelled(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.scanMu.Lock()
	defer e.scanMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = e.performImageScan(ctx, Request{}, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestImageScanContainerConfinementAndPartialJava(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	imageID := "sha256:" + strings.Repeat("a", 64)
	started, removed := false, false
	name := "cove-scan-owner-op"
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		switch args[1] {
		case "run":
			started = true
			joined := strings.Join(args, " ")
			for _, required := range []string{"--network none", "--read-only", "--user 65532:65532", "--cap-drop ALL", "no-new-privileges:true", "--memory 2g", "--cpus 1", "--pids-limit 128", "--scanners vuln", "--offline-scan", "--skip-db-update", "--skip-java-db-update", "--skip-version-check", "--disable-telemetry", "target=/input/image.tar,readonly", "target=/cache/db,readonly", ScannerImage, "/report:size=8m"} {
				if !strings.Contains(joined, required) {
					t.Fatal("missing confinement", required)
				}
			}
			if strings.Contains(joined, "docker.sock") || strings.Contains(joined, "download-java") {
				t.Fatal("scanner has Docker access or Java refresh")
			}
		case "exec":
			if args[len(args)-1] == "/report/exit" {
				return ioWrite(out, "0")
			}
			return ioWrite(out, `{"SchemaVersion":2,"Metadata":{"ImageID":"`+imageID+`"},"Results":[{"Class":"os-pkgs","Type":"alpine","Vulnerabilities":[{"VulnerabilityID":"CVE-2026-12345","PkgName":"pkg","InstalledVersion":"1","Severity":"HIGH"}]}]}`)
		case "ps":
			return ioWrite(out, "owned-id")
		case "inspect":
			return json.NewEncoder(out).Encode([]any{map[string]any{"Name": "/" + name, "Config": map[string]any{"Labels": map[string]string{"cove.managed": "image-scan", "cove.image-scan-owner": "owner"}}}})
		case "rm":
			removed = true
		default:
			t.Fatal(args)
		}
		return nil
	}
	result, err := e.scanImageArchive(context.Background(), "owner", "op", imageID, "/owned/input.tar", "/owned/db", scanArchive{ConfigDigest: imageID, JavaArchives: true}, ScannerImage)
	if err != nil || !started || !removed || result.State != "unknown" || result.Counts["HIGH"] != 1 || result.Coverage.JavaArchives != "not_scanned" || result.Coverage.OS != "scanned" {
		t.Fatal(result, err, started, removed)
	}
}
