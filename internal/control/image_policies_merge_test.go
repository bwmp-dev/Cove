package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cove/internal/worker"
)

func mergedImagePolicyFixture(t *testing.T, mode string) (*registryHookFixture, worker.ImageUpdatePolicy) {
	t.Helper()
	f := newRegistryHookFixture(t, "distribution", "registry.example.test")
	f.call("root", "default", "DELETE", f.path+"/registry-webhook", nil, 200)
	f.sql(`UPDATE nodes SET capabilities=capabilities || '{"image_updates":true,"compose_bundles":true}'::jsonb WHERE id='node'`)
	p := worker.ImageUpdatePolicy{Enabled: true, Mode: mode, Rules: []worker.ImageUpdateRule{{Service: "app"}}}
	f.call("root", "default", "PUT", f.path+"/image-update-policy", p, 200)
	return f, p
}

func mergedImagePolicyCandidate(t *testing.T, f *registryHookFixture) string {
	t.Helper()
	tx, err := f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	r := worker.Request{ID: resourceID(), Workload: f.id, Kind: "service", Action: "image_check"}
	if err := f.s.prepareImageUpdateCheck(t.Context(), tx, &r); err != nil || r.ImageUpdates == nil {
		t.Fatal("authorised policy check not prepared", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	payload, err := f.s.sealJSON(r)
	if err != nil {
		t.Fatal(err)
	}
	target := "registry.example.test/team/app:stable@sha256:" + strings.Repeat("a", 64)
	result, err := json.Marshal(worker.Operation{ImageCheck: &worker.ImageCheck{Updates: &worker.ImageUpdateResult{State: "available", Key: strings.Repeat("b", 64), Candidates: []worker.ImageUpdateCandidate{{Service: "app", Current: "registry.example.test/team/app:stable", Target: target}}, Pins: map[string]string{"app": target}}}})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES($1,$2,'image_check','succeeded',$3,$4)`, r.ID, f.id, payload, result)
	return r.ID
}

func TestImagePoliciesMergeStandaloneOwnershipRevocationAndScopes(t *testing.T) {
	f, policy := mergedImagePolicyFixture(t, "auto")
	owner := f.user("policy-owner", "policy-owner@example.invalid")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','policy-owner','owner')`)
	_, write := mintPublic(t, f.accountFixture, owner, "user", "workloads:write")
	_, both := mintPublic(t, f.accountFixture, owner, "user", "workloads:write", "workloads:operate")
	path := "/api/v1/workloads/" + f.id + "/image-update-policy"
	publicCall(t, f.accountFixture, write, "default", "PUT", path, policy, 403)
	publicCall(t, f.accountFixture, both, "default", "PUT", path, policy, 200)
	mergedImagePolicyCandidate(t, f)
	if err := f.s.dispatchImageUpdates(t.Context()); err != nil {
		t.Fatal("standalone policy incorrectly depends on a registry hook", err)
	}
	var operation, reason string
	if err := f.s.DB.QueryRow(t.Context(), `SELECT d.operation_id,o.reason FROM image_update_deployments d JOIN operations o ON o.id=d.operation_id WHERE o.workload_id=$1`, f.id).Scan(&operation, &reason); err != nil || reason != "Image update policy: verified candidate" {
		t.Fatal("automatic reason or deployment missing", reason, err)
	}
	tx, err := f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := revokeSCIMAccess(t.Context(), tx, "policy-owner"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.call("root", "default", "GET", f.path+"/image-update-policy", nil, 200).Body.String(), `"ownership_required":true`) {
		t.Fatal("deactivated policy silently retained an authorised creator")
	}
	if proceed, err := f.s.imageUpdateDispatch(t.Context(), operation, false); err != nil || proceed {
		t.Fatal("unsubmitted revoked policy deployment was not cancelled", proceed, err)
	}
	f.sql(`UPDATE operations SET state='queued',cancel_requested=false WHERE id=$1`, operation)
	f.sql(`UPDATE image_update_deployments SET dispatch_started=true WHERE operation_id=$1`, operation)
	if proceed, err := f.s.imageUpdateDispatch(t.Context(), operation, false); err != nil || !proceed {
		t.Fatal("uncertain submission must reconcile cancellation", proceed, err)
	}
	if proceed, err := f.s.imageUpdateDispatch(t.Context(), operation, true); err != nil || proceed {
		t.Fatal("revoked uncertain deployment was resubmitted", proceed, err)
	}
	check := worker.Request{ID: resourceID(), Workload: f.id, Kind: "service", Action: "image_check"}
	tx, err = f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := f.s.prepareImageUpdateCheck(t.Context(), tx, &check); err != nil || check.ImageUpdates != nil {
		t.Fatal("revoked policy still performs version discovery", err)
	}
}

func TestImagePoliciesMergeConfigurationOnlyCanDisableHeldDeployment(t *testing.T) {
	f, p := mergedImagePolicyFixture(t, "auto")
	mergedImagePolicyCandidate(t, f)
	if err := f.s.dispatchImageUpdates(t.Context()); err != nil {
		t.Fatal(err)
	}
	cookie := f.user("policy-config", "policy-config@example.invalid")
	f.sql(`INSERT INTO team_roles(team_id,id,name,permissions) VALUES('default','policy-config','Configuration only',ARRAY['view','edit_configuration'])`)
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','policy-config','member','[{"scope":"team","scope_id":"default","role":"policy-config"}]')`)
	f.call(cookie, "default", "PUT", f.path+"/image-update-policy", p, 403)
	p.Enabled = false
	f.call(cookie, "default", "PUT", f.path+"/image-update-policy", p, 200)
	var cancelled bool
	if err := f.s.DB.QueryRow(t.Context(), `SELECT cancel_requested FROM operations WHERE workload_id=$1 AND action='deploy'`, f.id).Scan(&cancelled); err != nil || !cancelled {
		t.Fatal("disabling policy left its held deployment active", cancelled, err)
	}
}

func TestImagePoliciesMergeNotifyUsesExplicitOutcomeCatalogue(t *testing.T) {
	f, _ := mergedImagePolicyFixture(t, "notify")
	f.call("root", "default", "POST", "/api/notifications", connectionConfig{Name: "Explicit updates", Notification: &notificationConfig{Provider: "webhook", URL: "https://notify.example.test/updates", Enabled: true, Events: []string{"image.update_available"}}}, 201)
	f.call("root", "default", "POST", "/api/notifications", connectionConfig{Name: "By result", Notification: &notificationConfig{Provider: "webhook", URL: "https://notify.example.test/results", Enabled: true, Success: true}}, 201)
	mergedImagePolicyCandidate(t, f)
	if err := f.s.dispatchImageUpdates(t.Context()); err != nil {
		t.Fatal(err)
	}
	var events, generic, deploys int
	err := f.s.DB.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM health_deliveries WHERE event='image.update_available' AND subject_id=$1),(SELECT count(*) FROM notification_deliveries),(SELECT count(*) FROM operations WHERE workload_id=$1 AND action='deploy')`, f.id).Scan(&events, &generic, &deploys)
	if err != nil || events != 2 || generic != 0 || deploys != 0 {
		t.Fatal("update outcome bypassed the catalogue or deployed in notify mode", events, generic, deploys, err)
	}
	if notificationMatches(&notificationConfig{Enabled: true, Success: true}, "image.update_available") || !notificationMatches(&notificationConfig{Enabled: true, Events: []string{"image.update_available"}}, "image.update_available") {
		t.Fatal("update outcome treated as generic operation success")
	}
}

func TestImagePoliciesMergeAdmissionKeepsGlobalAndPolicyWindows(t *testing.T) {
	f, _ := mergedImagePolicyFixture(t, "auto")
	mergedImagePolicyCandidate(t, f)
	if err := f.s.dispatchImageUpdates(t.Context()); err != nil {
		t.Fatal(err)
	}
	var operation string
	if err := f.s.DB.QueryRow(t.Context(), `SELECT operation_id FROM image_update_deployments`).Scan(&operation); err != nil {
		t.Fatal(err)
	}
	from, until := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	f.call("root", "default", "PUT", "/api/projects/project/deployment-policy", deploymentPolicy{Approval: true, FreezeFrom: &from, FreezeUntil: &until}, 200)
	if allowed, err := f.s.admitDeployment(t.Context(), operation); err != nil || allowed {
		t.Fatal("global freeze bypassed", allowed, err)
	}
	f.call("root", "default", "POST", "/api/operations/"+operation+"/approve", map[string]any{}, 200)
	f.call("root", "default", "PUT", "/api/projects/project/deployment-policy", deploymentPolicy{}, 200)
	now := time.Now().UTC()
	f.sql(`UPDATE image_update_deployments SET window_start=$2,window_end=$3 WHERE operation_id=$1`, operation, now.Add(time.Hour).Format("15:04"), now.Add(2*time.Hour).Format("15:04"))
	if allowed, err := f.s.admitDeployment(t.Context(), operation); err != nil || allowed {
		t.Fatal("image policy maintenance window bypassed", allowed, err)
	}
	f.call("root", "default", "POST", "/api/operations/"+operation+"/emergency-override", map[string]string{"reason": "Explicit incident recovery"}, 200)
	if allowed, err := f.s.admitDeployment(t.Context(), operation); err != nil || !allowed {
		t.Fatal("authorised emergency override did not cover the policy window", allowed, err)
	}
}

func TestImagePoliciesMergeUsesOverridesAndRejectsIncompleteCapabilities(t *testing.T) {
	f, policy := mergedImagePolicyFixture(t, "notify")
	bundle := &worker.ComposeBundle{Files: []worker.ComposeFile{{Path: "override.yaml", Content: "services:\n  sidecar:\n    image: registry.example.test/team/sidecar:2.0.0\n"}}}
	f.call("root", "default", "PUT", f.path+"/compose-bundle", map[string]any{"bundle": bundle}, 200)
	policy.Rules = []worker.ImageUpdateRule{{Service: "sidecar", Range: "^2.0.0"}}
	f.call("root", "default", "PUT", f.path+"/image-update-policy", policy, 200)
	tx, err := f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	check := worker.Request{ID: resourceID(), Workload: f.id, Kind: "service", Action: "image_check"}
	if err := f.s.prepareImageUpdateCheck(t.Context(), tx, &check); err != nil || check.ImageUpdates == nil || check.ImageUpdates.Images["sidecar"] != "registry.example.test/team/sidecar:2.0.0" || len(check.ImageUpdates.Images) != 2 {
		t.Fatal("override service was omitted from discovery", check.ImageUpdates, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE nodes SET capabilities=capabilities-'image_lifecycle' WHERE id='node'`)
	f.call("root", "default", "PUT", f.path+"/image-update-policy", policy, 409)
	var status struct {
		WorkerSupported bool `json:"worker_supported"`
	}
	if err := json.Unmarshal(f.call("root", "default", "GET", f.path+"/image-update-policy", nil, 200).Body.Bytes(), &status); err != nil || status.WorkerSupported {
		t.Fatal("incomplete capability union reported supported", status, err)
	}
}

func TestImagePoliciesMergeExplicitReferencesAndPinnedMetadata(t *testing.T) {
	s := &Server{Key: []byte(strings.Repeat("x", 32))}
	config := workloadInput{Kind: "service", Compose: "services:\n  app:\n    image: registry.example.test/team/app:stable\n  sidecar:\n    image: $SIDECAR\n"}
	sealed, err := s.sealJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	p := worker.ImageUpdatePolicy{Enabled: true, Mode: "notify", Rules: []worker.ImageUpdateRule{{Service: "app"}}}
	if _, err := s.validateImageUpdateConfiguration("fixture", sealed, p); err == nil {
		t.Fatal("unselected unresolved image accepted as complete coverage")
	}
	config.Compose = "services:\n  app:\n    image: registry.example.test/team/app@sha256:" + strings.Repeat("a", 64) + "\n"
	images, err := imageOnlyServiceImages(config)
	if err != nil || len(images) != 1 || images["app"] == "" {
		t.Fatal("pinned images disappeared from metadata", images, err)
	}
}
