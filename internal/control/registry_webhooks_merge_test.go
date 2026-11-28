package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestRegistryWebhookMergedOwnershipAndScopedTokens(t *testing.T) {
	f := newRegistryHookFixture(t, "distribution", "registry.example.test")
	owner := f.user("registry-owner", "registry-owner@example.invalid")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','registry-owner','owner')`)
	input := map[string]any{"provider": "distribution", "enabled": true, "filters": []registryWebhookFilter{{Repository: "team/app", Tag: "stable"}}}
	_, read := mintPublic(t, f.accountFixture, owner, "user", "workloads:read")
	_, write := mintPublic(t, f.accountFixture, owner, "user", "workloads:write")
	_, both := mintPublic(t, f.accountFixture, owner, "user", "workloads:write", "workloads:operate")
	path := "/api/v1/workloads/" + f.id + "/registry-webhook"
	publicCall(t, f.accountFixture, read, "default", "GET", path, nil, 200)
	publicCall(t, f.accountFixture, read, "default", "PUT", path, input, 403)
	publicCall(t, f.accountFixture, write, "default", "PUT", path, input, 403)
	publicCall(t, f.accountFixture, both, "default", "PUT", path, input, 200)
	f.send(distributionPush("owned-pending"), "distribution", "", 202)
	f.sql(`INSERT INTO authentication_failures(key,failures,window_start,locked_until) VALUES('account:registry-owner',5,now(),now()+interval '1 hour')`)
	if !f.status().OwnershipRequired {
		t.Fatal("locked creator still authorised")
	}
	if err := f.s.dispatchRegistryWebhooks(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.status(); got.Pending != nil || got.LastOperation != nil || got.Ignored == "" {
		t.Fatal("unauthorised pending intent survived", got)
	}
	f.sql(`DELETE FROM authentication_failures WHERE key='account:registry-owner'`)
	f.send(distributionPush("owned-queued"), "distribution", "", 202)
	if err := f.s.dispatchRegistryWebhooks(t.Context()); err != nil {
		t.Fatal(err)
	}
	op := f.status().LastOperation.ID
	var reason string
	if err := f.s.DB.QueryRow(t.Context(), `SELECT reason FROM operations WHERE id=$1`, op).Scan(&reason); err != nil || !strings.Contains(reason, "Registry webhook") {
		t.Fatal("automatic reason missing", reason, err)
	}
	tx, err := f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err = revokeSCIMAccess(t.Context(), tx, "registry-owner"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !f.status().OwnershipRequired {
		t.Fatal("deprovisioned ownership resumed without a claim")
	}
	if proceed, err := f.s.registryWebhookDispatch(t.Context(), op, false); err != nil || proceed {
		t.Fatal("unsubmitted revoked deployment was not cancelled", proceed, err)
	}
	f.sql(`UPDATE operations SET state='queued',cancel_requested=false WHERE id=$1`, op)
	f.sql(`UPDATE registry_webhook_runs SET dispatch_started=true WHERE operation_id=$1`, op)
	if proceed, err := f.s.registryWebhookDispatch(t.Context(), op, false); err != nil || !proceed {
		t.Fatal("uncertain submission must reconcile worker cancellation", proceed, err)
	}
	if proceed, err := f.s.registryWebhookDispatch(t.Context(), op, true); err != nil || proceed {
		t.Fatal("revoked uncertain deployment was resubmitted", proceed, err)
	}
	var claimed registryWebhookStatus
	if err := json.Unmarshal(f.call("root", "default", "POST", f.path+"/registry-webhook/rotate", nil, 200).Body.Bytes(), &claimed); err != nil || claimed.OwnershipRequired || claimed.Token == "" {
		t.Fatal("explicit rotation did not claim and reveal once", err)
	}
	metadata := f.call("root", "default", "GET", f.path+"/registry-webhook", nil, 200)
	if metadata.Header().Get("Cache-Control") != "no-store" || strings.Contains(metadata.Body.String(), claimed.Token) {
		t.Fatal("registry credential exposed through cacheable metadata")
	}
}

func TestRegistryWebhookMergedSavedDeploymentMetadata(t *testing.T) {
	f := newRegistryHookFixture(t, "distribution", "registry.example.test")
	f.send(distributionPush("pending"), "distribution", "", 202)
	if err := f.s.dispatchRegistryWebhooks(t.Context()); err != nil {
		t.Fatal(err)
	}
	operation := f.status().LastOperation.ID
	f.call("root", "default", "DELETE", f.path+"/registry-webhook", nil, 200)
	f.call("root", "default", "PUT", f.path+"/registry-webhook", map[string]any{"provider": "distribution", "enabled": true, "filters": []registryWebhookFilter{{Repository: "team/app", Tag: "stable"}}}, 201)
	if proceed, err := f.s.registryWebhookDispatch(t.Context(), operation, true); err != nil || proceed {
		t.Fatal("deleted webhook still authorised its queued deployment", proceed, err)
	}
	var count int
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM operations WHERE id=$1 AND state='cancelled'`, operation).Scan(&count); err != nil || count != 1 {
		t.Fatal("cancelled operation retained workload lock", count, err)
	}
	for _, route := range []string{"/workloads/{id}/registry-webhook", "/workloads/{id}/registry-webhook/rotate"} {
		document, err := json.Marshal(f.s.openAPIDocument())
		if err != nil || !strings.Contains(string(document), route) {
			t.Fatal("new endpoint absent from OpenAPI", route, err)
		}
	}
}

func TestRegistryWebhookMergedConfigurationOnlyCanDisableButNotEnable(t *testing.T) {
	f := newRegistryHookFixture(t, "distribution", "registry.example.test")
	cookie := f.user("registry-config", "registry-config@example.invalid")
	f.sql(`INSERT INTO team_roles(team_id,id,name,permissions) VALUES('default','registry-config','Configuration only',ARRAY['view','edit_configuration'])`)
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','registry-config','member','[{"scope":"team","scope_id":"default","role":"registry-config"}]')`)
	input := map[string]any{"provider": "distribution", "enabled": true, "filters": []registryWebhookFilter{{Repository: "team/app", Tag: "stable"}}}
	f.call(cookie, "default", "PUT", f.path+"/registry-webhook", input, 403)
	f.call(cookie, "default", "POST", f.path+"/registry-webhook/rotate", nil, 403)
	input["enabled"] = false
	f.call(cookie, "default", "PUT", f.path+"/registry-webhook", input, 200)
	f.call(cookie, "default", "DELETE", f.path+"/registry-webhook", nil, 200)
}

func TestRegistryWebhookMergedComposeOverridesRemainImageOnly(t *testing.T) {
	s := &Server{Key: []byte(strings.Repeat("x", 32))}
	config := workloadInput{Kind: "service", Compose: "services:\n  app:\n    image: registry.example.test/team/app:stable\n", ComposeBundle: &worker.ComposeBundle{Files: []worker.ComposeFile{{Path: "override.yaml", Content: "services:\n  app:\n    image: registry.example.test/team/app:next\n"}}}}
	sealed, err := s.sealJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	request, err := s.registryWebhookDeployment("fixture", sealed)
	if err != nil || request.ComposeBundle == nil || request.ComposeBundle.Files[0].Content != config.ComposeBundle.Files[0].Content {
		t.Fatal("image-only override was lost", err)
	}
	config.ComposeBundle.Files[0].Content = "services:\n  app:\n    build: .\n"
	sealed, err = s.sealJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.registryWebhookDeployment("fixture", sealed); err == nil {
		t.Fatal("override introduced a build into image-only automation")
	}
	config.Compose = "services:\n  app:\n    image: registry.example.test/team/app:stable\n    build: .\n"
	config.ComposeBundle.Files[0].Content = "services:\n  app:\n    image: registry.example.test/team/app:next\n"
	sealed, err = s.sealJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.registryWebhookDeployment("fixture", sealed); err == nil {
		t.Fatal("override image concealed a base build service")
	}
}
