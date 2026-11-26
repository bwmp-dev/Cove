package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func automaticScanFixtureRequest() Request {
	image := "sha256:" + strings.Repeat("a", 64)
	return Request{ID: "auto-scan", Workload: "app", Kind: "service", Action: "image_scan", AutomaticScan: &AutomaticScan{Origin: "post_deploy", Database: image, Images: map[string][]string{"app": {image}}}}
}

func TestAutomaticImageScanUsesDedicatedMethodAndNoSecrets(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := automaticScanFixtureRequest()
	if err := ValidateRequest(r); err != nil {
		t.Fatal(err)
	}
	if response := e.RPC(context.Background(), RPCRequest{Method: "submit", Request: &r}); response.Error == "" {
		t.Fatal("ordinary method accepted background semantics")
	}
	response := e.RPC(context.Background(), RPCRequest{Method: "submit_automatic_image_scan", Request: &r})
	if response.Error != "" || response.Operation == nil || response.Operation.ScanOrigin != "post_deploy" {
		t.Fatal(response)
	}
	r.Environment = map[string]string{"SECRET": "fixture-private"}
	if ValidateRequest(r) == nil {
		t.Fatal("automatic scan accepted secret inputs")
	}
}

func TestAutomaticImageScanDoesNotConsumeForegroundSlot(t *testing.T) {
	t.Setenv("COVE_IMAGE_SCANNING", "1")
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	var once sync.Once
	e.Run = func(ctx context.Context, _ string, args, _ []string, out io.Writer) error {
		if strings.Join(args, " ") == "docker image inspect "+ScannerImage {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return ctx.Err()
		}
		if handled, err := releaseFixture(args, out); handled {
			return err
		}
		return nil
	}
	r := automaticScanFixtureRequest()
	if _, err = e.Submit(r); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var jobs sync.WaitGroup
	if err = e.dispatch(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("scan did not start")
	}
	e.mu.Lock()
	busy := e.busy(r.Workload)
	slots := len(e.active)
	e.mu.Unlock()
	if busy || slots != 0 {
		t.Fatal("background scan reserved deployment capacity", busy, slots)
	}
	deploy := Request{ID: "foreground-deploy", Workload: "app", Kind: "service", Action: "deploy", Compose: "services:\n  app:\n    image: busybox:1.37\n"}
	if _, err = e.Submit(deploy); err != nil {
		t.Fatal(err)
	}
	if err = e.dispatch(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	op, err := e.Status(deploy.ID)
	if err != nil || op.State != "succeeded" {
		t.Fatal("deployment waited for or failed on scan", op, err)
	}
	auto, err := e.Status(r.ID)
	if err != nil || auto.State == "running" || auto.ScanOrigin != "post_deploy" {
		t.Fatal(auto, err)
	}
}

func TestAutomaticImageScanDatabaseUnknownDoesNotStartRepeatedRefreshes(t *testing.T) {
	t.Setenv("COVE_IMAGE_SCANNING", "1")
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	status, err := e.scanDatabaseStatus(true)
	if err != nil || status.Fresh || !status.Refreshing {
		t.Fatal(status, err)
	}
	state, err := e.readScanRefresh()
	if err != nil {
		t.Fatal(err)
	}
	state.Requested = false
	state.NextAttempt = time.Now().Add(time.Hour)
	state.Message = "fixture failed refresh"
	if err = writeJSON(filepath.Join(e.Root, "scanner", "automatic-refresh.json"), state); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		status, err = e.scanDatabaseStatus(true)
		if err != nil || status.Fresh || status.Refreshing || status.Message != "fixture failed refresh" {
			t.Fatal("failed DB refresh storm", status, err)
		}
	}
	if err = os.WriteFile(filepath.Join(e.Root, "scanner", "current.json"), []byte(`{"digest":"x","path":"db-`+strings.Repeat("a", 64)+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = e.cachedScanDatabase(); err == nil {
		t.Fatal("malformed database identity was trusted")
	}
}

func TestAutomaticImageScanObservationDoesNotStartContainers(t *testing.T) {
	t.Setenv("COVE_IMAGE_SCANNING", "1")
	for _, state := range []string{"running", "exited"} {
		t.Run(state, func(t *testing.T) {
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			r := automaticScanFixtureRequest()
			e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
				if args[1] == "ps" {
					return json.NewEncoder(out).Encode(map[string]string{"ID": "container", "State": state})
				}
				if args[1] == "inspect" {
					return json.NewEncoder(out).Encode(map[string]any{"Image": r.AutomaticScan.Images["app"][0], "Service": "app", "State": map[string]string{"Status": state}})
				}
				return errors.New("unexpected mutating command")
			}
			observed, err := e.observeScan(context.Background(), r)
			if err != nil || observed.Eligible != (state == "running") {
				t.Fatal(observed, err)
			}
			r.Kind, r.JobService = "job", "app"
			observed, err = e.observeScan(context.Background(), r)
			if err != nil || !observed.Eligible {
				t.Fatal("retained completed job was not observable", observed, err)
			}
		})
	}
}

func TestAutomaticImageScanRefreshRecoveryAndRemoval(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(e.Root, "scanner")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	next := time.Now().UTC().Add(time.Hour)
	if err := writeJSON(filepath.Join(root, "automatic-refresh.json"), scanDatabaseRefresh{ID: id, NextAttempt: next}); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "refresh-input-"+id)
	if err := os.Mkdir(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		if len(args) > 1 && args[1] == "ps" {
			return nil
		}
		return errors.New("unexpected recovery command")
	}
	if err := e.recoverScanRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := e.readScanRefresh()
	if err != nil || state.ID != "" || state.Requested || !state.NextAttempt.Equal(next) || !strings.Contains(state.Message, "interrupted") {
		t.Fatal(state, err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatal("interrupted refresh input was retained", err)
	}
	e.automaticScans = map[string]string{"active-auto": "app"}
	cancelled := false
	e.cancels["active-auto"] = func() { cancelled = true }
	if _, err := e.RemoveWorkload(context.Background(), "app"); err == nil || !cancelled {
		t.Fatal("removal raced active background input", err, cancelled)
	}
}

func TestAutomaticImageScanRestartPreservesOrigin(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := automaticScanFixtureRequest()
	if _, err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	j, err := e.read(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	j.Operation.State = "running"
	if err := writeJSON(e.path(r.ID), j); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Recover(); err != nil {
		t.Fatal(err)
	}
	op, err := restarted.Status(r.ID)
	if err != nil || op.State != "interrupted" || op.ScanOrigin != "post_deploy" {
		t.Fatal(op, err)
	}
}

func TestAutomaticImageScanIdentitySeparatesPlatforms(t *testing.T) {
	index := "sha256:" + strings.Repeat("a", 64)
	amd64 := "sha256:" + strings.Repeat("b", 64)
	arm64 := "sha256:" + strings.Repeat("c", 64)
	if scanImageIdentity(index, amd64) == scanImageIdentity(index, arm64) || scanImageIdentity(index, amd64) == scanImageIdentity(index, "") {
		t.Fatal("multi-platform index coverage was reused across configurations")
	}
}
