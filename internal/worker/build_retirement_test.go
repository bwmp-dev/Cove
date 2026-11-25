package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetireBuildKeepsLateDeliveryTombstoneAndRuntimeState(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "remote-build", Workload: strings.Repeat("a", 32), Action: "build", Kind: "service", Compose: "services:\n  app:\n    build: .\n", Build: &BuildOptions{Repository: "team/app"}, Environment: map[string]string{"FIXTURE_SECRET": "private-fixture-value"}, Registry: &Registry{Server: "registry.example.test", Username: "fixture", Password: "private-fixture-password"}}
	if _, err = e.Submit(r); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "deployed.json"), []byte("retained-runtime-state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.Root, "operations", r.ID+".log"), []byte("private-log"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = e.RetireBuild(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err = e.RetireBuild(context.Background(), r); err != nil {
		t.Fatal("retry", err)
	}
	got, err := e.Submit(r)
	if err != nil || got.State != "cancelled" {
		t.Fatal("late delivery was not retired", got, err)
	}
	data, err := os.ReadFile(e.path(r.ID))
	if err != nil || strings.Contains(string(data), "private-fixture") {
		t.Fatal("build credentials retained", err)
	}
	if data, err = os.ReadFile(filepath.Join(dir, "deployed.json")); err != nil || string(data) != "retained-runtime-state" {
		t.Fatal("runtime state touched", err)
	}
	if _, err = os.Stat(filepath.Join(e.Root, "operations", r.ID+".log")); !os.IsNotExist(err) {
		t.Fatal("build log retained", err)
	}
	restarted, err := NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.Recover(); err != nil {
		t.Fatal("retired journal recovery", err)
	}
	if op, err := restarted.Submit(r); err != nil || op.State != "cancelled" {
		t.Fatal("restart lost build tombstone", op, err)
	}
	other := r
	other.Workload = strings.Repeat("b", 32)
	if err = e.RetireBuild(context.Background(), other); err == nil {
		t.Fatal("foreign ownership accepted")
	}
}

func TestRetireUnacknowledgedBuildPreventsExecution(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "never-acknowledged", Workload: strings.Repeat("a", 32), Action: "build", Compose: "services:\n  app:\n    build: .\n", Build: &BuildOptions{Repository: "team/app"}}
	if err = e.RetireBuild(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if op, err := e.Submit(r); err != nil || op.State != "cancelled" {
		t.Fatal(op, err)
	}
}

func TestDesignatedBuildRequiresDedicatedSubmissionMethod(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "platform-build", Workload: strings.Repeat("a", 32), Action: "build", Compose: "services:\n  app:\n    build: .\n", Build: &BuildOptions{Repository: "team/app"}, BuildPlatform: "linux/amd64"}
	if response := e.RPC(context.Background(), RPCRequest{Method: "submit", Request: &r}); response.Error == "" {
		t.Fatal("generic method dropped build platform")
	}
	if response := e.RPC(context.Background(), RPCRequest{Method: "submit_build_v2", Request: &r}); response.Error != "" || response.Operation == nil {
		t.Fatal(response.Error)
	}
}

func TestDesignatedBuildJournalRejectsPrePlatformWorkers(t *testing.T) {
	for _, noCache := range []bool{false, true} {
		e, err := NewEngine(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		r := Request{ID: "platform-journal", Workload: strings.Repeat("a", 32), Action: "build", Compose: "services:\n  app:\n    build: .\n", Build: &BuildOptions{Repository: "team/app", NoCache: noCache}, BuildPlatform: "linux/amd64"}
		if _, err = e.Submit(r); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(e.path(r.ID))
		if err != nil {
			t.Fatal(err)
		}
		var legacy struct {
			Request struct {
				Build struct {
					NoCache bool `json:"no_cache"`
				} `json:"build"`
			} `json:"request"`
		}
		if json.Unmarshal(data, &legacy) == nil {
			t.Fatal("older worker silently dropped the target platform")
		}
		restarted, err := NewEngine(e.Root)
		if err != nil {
			t.Fatal(err)
		}
		journal, err := restarted.read(r.ID)
		if err != nil || journal.Request.Build == nil || journal.Request.Build.NoCache != noCache || journal.Request.BuildPlatform != "linux/amd64" {
			t.Fatal(journal.Request.Build, err)
		}
	}
}

func TestDesignatedBuildOptionsRefuseIncompleteProtection(t *testing.T) {
	for _, data := range []string{
		`{"repository":"team/app","no_cache":"cove-build-platform-v1"}`,
		`{"repository":"team/app","no_cache":true,"cove_no_cache":false}`,
		`{"repository":"team/app","no_cache":"unknown","cove_no_cache":true}`,
	} {
		var options BuildOptions
		if json.Unmarshal([]byte(data), &options) == nil {
			t.Fatal("incomplete build protection accepted", data)
		}
	}
}
