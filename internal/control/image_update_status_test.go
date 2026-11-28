package control

import (
	"encoding/json"
	"strings"
	"testing"

	"cove/internal/worker"
)

type imagePolicyStatusFixture struct {
	Stale     bool                      `json:"stale"`
	Check     *string                   `json:"last_check_id"`
	Freshness string                    `json:"freshness_message"`
	Error     string                    `json:"last_error"`
	Updates   *worker.ImageUpdateResult `json:"updates"`
}

func readImagePolicyStatusFixture(t *testing.T, f *registryHookFixture) imagePolicyStatusFixture {
	t.Helper()
	var status imagePolicyStatusFixture
	response := f.call("root", "default", "GET", f.path+"/image-update-policy", nil, 200)
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestImagePolicyAppliedCandidateBecomesCurrent(t *testing.T) {
	f, _ := imageRetentionFixture(t)
	automatic := imageRetentionCandidate(t, f, map[string]string{"app": imageRetentionTarget()})
	imageRetentionProcess(t, f, automatic, worker.Operation{State: "succeeded"})
	assertCurrent := func() {
		t.Helper()
		status := readImagePolicyStatusFixture(t, f)
		if status.Stale || status.Updates == nil || status.Updates.State != "current" || len(status.Updates.Candidates) != 0 || status.Updates.Message != "The retained automatic image versions are applied." {
			t.Fatal("applied target still presented as available or stale", status)
		}
	}
	assertCurrent()
	ordinary := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "deploy"}, 202), "id")
	imageRetentionProcess(t, f, ordinary, worker.Operation{State: "succeeded"})
	assertCurrent()
	restart := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "restart"}, 202), "id")
	imageRetentionProcess(t, f, restart, worker.Operation{State: "succeeded"})
	assertCurrent()
}

func TestImagePolicyManualCheckWakesFreshPolicyProcessing(t *testing.T) {
	f, _ := imageRetentionFixture(t)
	automatic := imageRetentionCandidate(t, f, map[string]string{"app": imageRetentionTarget()})
	imageRetentionProcess(t, f, automatic, worker.Operation{State: "succeeded"})
	config := workloadInput{Name: "Changed", NodeID: "node", Kind: "service", Compose: "services:\n  app:\n    image: registry.example.test/team/app:v1.12.0\n"}
	f.call("root", "default", "PUT", f.path, config, 200)
	beforeCheck := readImagePolicyStatusFixture(t, f)
	if !beforeCheck.Stale || beforeCheck.Freshness != "Run an image check for the current policy and configuration." {
		t.Fatal("changed configuration lacks a working freshness action", beforeCheck)
	}
	f.sql(`UPDATE image_update_policies SET next_dispatch_at=now()+interval '1 minute',last_error='Run a new image check for the current policy and configuration.' WHERE workload_id=$1`, f.id)
	check := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "image_check"}, 202), "id")
	submitted, _ := imageRetentionProcess(t, f, check, worker.Operation{State: "succeeded", ImageCheck: &worker.ImageCheck{Updates: &worker.ImageUpdateResult{State: "current", Candidates: []worker.ImageUpdateCandidate{}}, Images: []worker.ImageStatus{{Service: "app", Reference: "registry.example.test/team/app:v1.12.0", ImageID: "sha256:" + strings.Repeat("a", 64), Status: "current"}}}})
	if submitted.ImageUpdates == nil || submitted.ImageUpdates.Images["app"] != "registry.example.test/team/app:v1.12.0" {
		t.Fatal("manual check lost current policy inputs", submitted.ImageUpdates)
	}
	if err := f.s.dispatchImageUpdates(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := readImagePolicyStatusFixture(t, f)
	if status.Check == nil || *status.Check != check || status.Stale || status.Error != "" || status.Updates == nil || status.Updates.State != "current" || status.Freshness != "" {
		t.Fatal("completed manual check did not refresh the policy without another save or timer wait", status)
	}
}

func TestImagePolicyCurrentNeedsConfirmedRetainedEvidence(t *testing.T) {
	for _, scenario := range []string{"pending rollout", "failed later deploy", "new candidate", "unknown check", "configuration changed"} {
		t.Run(scenario, func(t *testing.T) {
			f, _ := imageRetentionFixture(t)
			automatic := imageRetentionCandidate(t, f, map[string]string{"app": imageRetentionTarget()})
			if scenario == "pending rollout" {
				imageRetentionProcess(t, f, automatic, worker.Operation{State: "succeeded", Rollout: &worker.RolloutStatus{Phase: "waiting", Release: automatic}})
			} else {
				imageRetentionProcess(t, f, automatic, worker.Operation{State: "succeeded"})
				switch scenario {
				case "failed later deploy":
					next := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "deploy"}, 202), "id")
					imageRetentionProcess(t, f, next, worker.Operation{State: "failed"})
				case "configuration changed":
					f.call("root", "default", "PUT", f.path, workloadInput{Name: "Changed", NodeID: "node", Kind: "service", Compose: "services:\n  app:\n    image: registry.example.test/team/app:v2.0.0\n"}, 200)
				default:
					check := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "image_check"}, 202), "id")
					updates := &worker.ImageUpdateResult{State: "unknown", Message: "Registry unavailable.", Candidates: []worker.ImageUpdateCandidate{}}
					if scenario == "new candidate" {
						updates = &worker.ImageUpdateResult{State: "available", Key: strings.Repeat("c", 64), Pins: map[string]string{"app": "registry.example.test/team/app:v1.13.0@sha256:" + strings.Repeat("b", 64)}, Candidates: []worker.ImageUpdateCandidate{{Service: "app", Current: imageRetentionTarget(), Target: "registry.example.test/team/app:v1.13.0@sha256:" + strings.Repeat("b", 64)}}}
					}
					imageRetentionProcess(t, f, check, worker.Operation{State: "succeeded", ImageCheck: &worker.ImageCheck{Updates: updates}})
					f.sql(`UPDATE image_update_policies SET next_dispatch_at=now() WHERE workload_id=$1`, f.id)
					if err := f.s.dispatchImageUpdates(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
			}
			status := readImagePolicyStatusFixture(t, f)
			if !status.Stale && status.Updates != nil && status.Updates.State == "current" {
				t.Fatal("unconfirmed or unknown state was advertised as fresh current", status)
			}
		})
	}
}

func TestImagePolicyDisabledDoesNotOfferFreshnessAction(t *testing.T) {
	f, policy := imageRetentionFixture(t)
	if status := readImagePolicyStatusFixture(t, f); status.Freshness != "Run an image check for the current policy and configuration." {
		t.Fatal("enabled policy without a check lacks guidance", status)
	}
	policy.Enabled = false
	f.call("root", "default", "PUT", f.path+"/image-update-policy", policy, 200)
	if status := readImagePolicyStatusFixture(t, f); status.Freshness != "" {
		t.Fatal("a disabled policy suggests a check that cannot refresh it", status)
	}
}
