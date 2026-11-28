package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIdleEligibilityRejectsUnobservedTraffic(t *testing.T) {
	config := func() map[string]any {
		return map[string]any{"services": map[string]any{"web": map[string]any{}, "init": map[string]any{}}}
	}
	routes := []Route{{Service: "web", Port: 8080}}
	if err := idleComposeEligibility(config(), routes, map[string]bool{"init": true}); err != nil {
		t.Fatal(err)
	}
	if err := idleComposeEligibility(config(), routes, nil); err == nil {
		t.Fatal("unobserved background service accepted")
	}
	for _, key := range []string{"ports", "network_mode", "external_links", "links", "privileged", "devices", "pid", "ipc"} {
		value := config()
		value["services"].(map[string]any)["web"].(map[string]any)[key] = true
		if err := idleComposeEligibility(value, routes, map[string]bool{"init": true}); err == nil {
			t.Fatal("unobserved traffic accepted", key)
		}
	}
	value := config()
	value["networks"] = map[string]any{"shared": map[string]any{"external": true}}
	if err := idleComposeEligibility(value, routes, map[string]bool{"init": true}); err == nil {
		t.Fatal("external network accepted")
	}
	for _, policy := range []IdlePolicy{{Enabled: true, AfterSeconds: 60}, {AfterSeconds: 59}, {AfterSeconds: 86401}} {
		if err := policy.validate(); err == nil {
			t.Fatal("invalid idle policy accepted", policy)
		}
	}
	for _, request := range []Request{{ID: "ordinary", Workload: "app", Action: "idle_sleep"}, {ID: "idle-" + strings.Repeat("a", 59), Workload: "app", Action: "idle_sleep"}, {ID: "ordinary", Workload: "app", Action: "idle_wake"}} {
		if err := ValidateRequest(request); err == nil {
			t.Fatal("invalid local operation identity accepted", request.ID)
		}
	}
	for _, seconds := range []int{0, 60, 86400} {
		if err := (IdlePolicy{Enabled: true, AfterSeconds: seconds, RequestDriven: true}).validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIdleJournalRetentionPreservesCurrentIntentAndPublicOperations(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(e.Root, "workloads", "app"), 0700); err != nil {
		t.Fatal(err)
	}
	finished := time.Now().UTC()
	for index := range 110 {
		id := fmt.Sprintf("idle-%03d", index)
		op, err := e.Submit(Request{ID: id, Workload: "app", Action: "idle_sleep"})
		if err != nil {
			t.Fatal(err)
		}
		j, err := e.read(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		j.Operation.State = "succeeded"
		j.Operation.CreatedAt = finished.Add(time.Duration(index) * time.Second)
		j.Operation.FinishedAt = &finished
		if err = writeJSON(e.path(id), j); err != nil {
			t.Fatal(err)
		}
	}
	e.pending = nil
	state := workloadSuspension{Operation: "pause", Options: SuspensionOptions{Reason: "environment", Scope: "development"}, Phase: "paused", Created: finished, BeforeIdle: &idleSuspensionOrigin{Operation: "idle-000", Created: finished}}
	if err = writeJSON(filepath.Join(e.Root, "workloads", "app", "suspension.json"), state); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Submit(Request{ID: "public-stop", Workload: "app", Action: "stop"}); err != nil {
		t.Fatal(err)
	}
	if err = e.pruneIdleOperations(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"idle-000", "idle-109", "public-stop"} {
		if _, err = e.Status(id); err != nil {
			t.Fatal("retention removed live intent or public history", id, err)
		}
	}
	if _, err = e.Status("idle-001"); !os.IsNotExist(err) {
		t.Fatal("old automatic journal was not pruned", err)
	}
	if _, err = e.Status("idle-010"); err != nil {
		t.Fatal("retention removed one of the last100", err)
	}
}

func TestIdleWakeRejectsOtherHoldsAndEpisodeReplay(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(e.Root, "workloads", "app"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, state := range []workloadSuspension{
		{Operation: "pause", Options: SuspensionOptions{Reason: "environment", Scope: "development"}},
		{Operation: "idle-current", Options: SuspensionOptions{Reason: "idle", Scope: "app"}, ManualStop: true},
		{Operation: "idle-current", Options: SuspensionOptions{Reason: "idle", Scope: "app"}},
	} {
		state.Phase = "paused"
		state.Created = time.Now().UTC()
		if err = writeJSON(filepath.Join(e.Root, "workloads", "app", "suspension.json"), state); err != nil {
			t.Fatal(err)
		}
		if err = e.wakeIdleEpisode(context.Background(), "app", "idle-old"); err == nil {
			t.Fatal("stale episode bypassed another hold")
		}
	}
	if err = e.wakeIdleEpisode(context.Background(), "app", strings.Repeat("x", 64)); err == nil {
		t.Fatal("non-idle episode accepted")
	}
	if len(e.pending) != 0 {
		t.Fatal("denied wake queued work")
	}
}

func TestIdleStatusShowsFailedWakeWithoutInventingAHealthyGateway(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(e.Root, "workloads", "app"), 0700); err != nil {
		t.Fatal(err)
	}
	policy := idleWorkloadState{Policy: IdlePolicy{Enabled: true, AfterSeconds: 60, RequestDriven: true}, Config: idleGatewayConfig{Workload: "app", Listen: "10.250.0.2:8080", Peer: "10.250.0.3", FrontSubnet: "10.250.0.0/29", BackendSubnet: "10.250.1.0/24", Mode: "asleep", Episode: "idle-current", Token: strings.Repeat("a", 64), Targets: map[string]string{}}}
	policy.Config.Revision = policy.Config.digest()
	if err = e.writeIdle("app", &policy); err != nil {
		t.Fatal(err)
	}
	suspension := workloadSuspension{Operation: "idle-current", Options: SuspensionOptions{Reason: "idle", Scope: "app"}, Phase: "paused", Created: time.Now().UTC()}
	if err = writeJSON(filepath.Join(e.Root, "workloads", "app", "suspension.json"), suspension); err != nil {
		t.Fatal(err)
	}
	op, err := e.Submit(Request{ID: "wake-idle-current", Workload: "app", Action: "idle_wake"})
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.read(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	j.Operation.State = "failed"
	if err = writeJSON(e.path(op.ID), j); err != nil {
		t.Fatal(err)
	}
	status, err := e.IdleStatus(context.Background(), "app")
	if err != nil || status.Phase != "wake_failed" || status.Error == "" {
		t.Fatal(status, err)
	}

	suspension.Phase = "awake"
	if err = writeJSON(filepath.Join(e.Root, "workloads", "app", "suspension.json"), suspension); err != nil {
		t.Fatal(err)
	}
	if status, err := e.IdleStatus(context.Background(), "app"); err != nil || status.Phase != "waking" {
		t.Fatal("health-ready snapshot was reported complete before route confirmation", status, err)
	}
	suspension.Phase = "paused"
	suspension.ManualStop = true
	if err = writeJSON(filepath.Join(e.Root, "workloads", "app", "suspension.json"), suspension); err != nil {
		t.Fatal(err)
	}
	status, err = e.IdleStatus(context.Background(), "app")
	if err != nil || status.Phase != "paused" {
		t.Fatal("manual stop was hidden by a stale wake result", status, err)
	}
	suspension.Options.Reason = "manual_stop"
	if err = writeJSON(filepath.Join(e.Root, "workloads", "app", "suspension.json"), suspension); err != nil {
		t.Fatal(err)
	}
	if status, err := e.suspensionStatus("app"); err != nil || status == nil || status.Reason != "manual_stop" {
		t.Fatal("intentional worker stop was hidden from runtime", status, err)
	}

}
