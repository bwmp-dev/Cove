package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cove/internal/outbound"
	"cove/internal/worker"
)

func TestMergedCustomRoleTokenCapabilities(t *testing.T) {
	f, id, cookie := publicFixture(t)
	_, token := mintPublic(t, f, cookie, "user", "workloads:read", "workloads:write", "workloads:operate", "secrets:read", "secrets:write", "exec:run")
	f.sql(`INSERT INTO team_roles(team_id,id,name,permissions) VALUES('default','configuration-only','Configuration only',ARRAY['view','edit_configuration'])`)
	f.sql(`UPDATE team_members SET role='member',grants='[{"scope":"environment","scope_id":"variable-env","role":"configuration-only"}]' WHERE user_id='public-owner'`)
	base := "/api/v1/workloads/" + id
	view := publicCall(t, f, token, "default", "GET", base, nil, 200)
	if !strings.Contains(view.Body.String(), `"edit_configuration"`) || strings.Contains(view.Body.String(), `"read_secrets"`) {
		t.Fatal("metadata did not preserve the limited role", view.Body.String())
	}
	publicCall(t, f, token, "default", "POST", base+"/variables/import", map[string]string{"dotenv": "VALUE=changed"}, 200)
	publicCall(t, f, token, "default", "POST", base+"/variables/export", nil, 403)
	publicCall(t, f, token, "default", "POST", base+"/exec", map[string]any{"service": "app", "command": []string{"env"}}, 403)
	publicCall(t, f, token, "default", "POST", base+"/operations", apiOperationInput{Action: "deploy"}, 403)
	f.call(cookie, "default", "POST", "/api/access-tokens", map[string]any{"name": "Denied secret scope", "scope": "user", "scopes": []string{"secrets:read"}, "days": 1}, 403)
	_, limited := mintPublic(t, f, cookie, "user", "workloads:write", "secrets:write")
	publicCall(t, f, limited, "default", "POST", base+"/variables/import", map[string]string{"dotenv": "VALUE=limited"}, 200)
	f.sql(`UPDATE team_roles SET permissions=ARRAY['view'] WHERE id='configuration-only'`)
	publicCall(t, f, limited, "default", "POST", base+"/variables/import", map[string]string{"dotenv": "VALUE=revoked"}, 403)
}

func TestMergedTokenSecondFactorLockoutAndAllowlist(t *testing.T) {
	f, id, cookie := publicFixture(t)
	_, token := mintPublic(t, f, cookie, "user", "workloads:read")
	f.sql(`UPDATE teams SET require_second_factor=true WHERE id='default'`)
	publicCall(t, f, token, "default", "GET", "/api/v1/workloads/"+id, nil, 403)
	f.sql(`UPDATE control_settings SET mcp_enabled=true WHERE id=1`)
	publicCall(t, f, token, "default", "POST", "/mcp", map[string]any{}, 403)
	sealed, err := seal(f.s.Key, []byte("12345678901234567890"))
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO account_totp(user_id,secret,confirmed) VALUES('public-owner',$1,true)`, sealed)
	publicCall(t, f, token, "default", "GET", "/api/v1/workloads/"+id, nil, 200)
	f.sql(`INSERT INTO authentication_failures(key,locked_until) VALUES('account:public-owner',now()+interval '1 minute')`)
	publicCall(t, f, token, "default", "GET", "/api/v1/workloads/"+id, nil, 429)
	f.sql(`DELETE FROM authentication_failures`)
	f.sql(`UPDATE control_settings SET panel_allowlist=ARRAY['127.0.0.1/32'] WHERE id=1`)
	f.s.PublicURL = "https://cove.example.com/control"
	f.handler = f.s.Handler()
	request := func(address string, status int) {
		r := httptest.NewRequest("GET", "/control/api/v1/workloads/"+id, nil)
		r.RemoteAddr = address
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Cove-Team", "default")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatal(address, w.Code, w.Body.String())
		}
	}
	request("192.0.2.2:1234", 403)
	request("127.0.0.1:1234", 200)
	var deliveries int
	f.call("root", "", "GET", "/control/api/session", nil, 403)
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='panel access restriction' AND status=403`).Scan(&deliveries); err != nil || deliveries == 0 {
		t.Fatal("missing network rejection audit", deliveries, err)
	}
}

func TestMergedFreezeHoldsDeployHook(t *testing.T) {
	f, id, cookie := publicFixture(t)
	created := f.call(cookie, "default", "POST", "/api/workloads/"+id+"/deploy-hook", nil, 201)
	hook, token := jsonString(t, created, "id"), jsonString(t, created, "token")
	f.sql(`INSERT INTO deployment_policies(team_id,freeze_from,freeze_until) VALUES('default',now()-interval '1 minute',now()+interval '1 hour')`)
	accepted := publicCall(t, f, token, "", "POST", "/hooks/deploy/"+hook, map[string]string{"delivery": "frozen-delivery", "reason": "Reviewed hook deployment"}, 202)
	operation := jsonString(t, accepted, "operation_id")
	if err := f.s.dispatchWebhook(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := f.s.DB.QueryRow(t.Context(), `SELECT reason FROM operations WHERE id=$1`, operation).Scan(&reason); err != nil || reason != "Reviewed hook deployment" {
		t.Fatal("hook reason was not preserved", reason, err)
	}
	admitted, err := f.s.admitDeployment(t.Context(), operation)
	if err != nil || admitted {
		t.Fatal("hook bypassed freeze", admitted, err)
	}
	var result json.RawMessage
	if err = f.s.DB.QueryRow(t.Context(), `SELECT result FROM operations WHERE id=$1 AND admitted_at IS NULL`, operation).Scan(&result); err != nil || !strings.Contains(string(result), "freeze") {
		t.Fatal(string(result), err)
	}
	f.sql(`DELETE FROM deployment_policies`)
	admitted, err = f.s.admitDeployment(t.Context(), operation)
	if err != nil || !admitted {
		t.Fatal("hook did not resume after freeze", admitted, err)
	}
}

func TestMergedReadSecretsIsIndependentOfConfigurationEdits(t *testing.T) {
	f, source, cookie := publicFixture(t)
	f.sql(`INSERT INTO environments(id,name,project_id,team_id) VALUES('target-env','Target','variable-project','default')`)
	target := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Target", NodeID: "variable-node", ProjectID: "variable-project", EnvironmentID: "target-env", Compose: "services:\n  app:\n    image: busybox:1.37\n"}, 201), "id")
	f.sql(`INSERT INTO team_roles(team_id,id,name,permissions) VALUES('default','secret-reader','Secrets only',ARRAY['view','read_secrets']),('default','config-writer','Configuration only',ARRAY['view','edit_configuration'])`)
	f.sql(`UPDATE team_members SET role='member',grants='[{"scope":"environment","scope_id":"variable-env","role":"secret-reader"},{"scope":"environment","scope_id":"target-env","role":"config-writer"}]' WHERE user_id='public-owner'`)
	f.call(cookie, "default", "PUT", "/api/workloads/"+target+"/variable-bindings", resourceVariableBinding{Name: "BOUND", SourceID: source, SourceKey: "SECRET"}, 200)
	f.call(cookie, "default", "POST", "/api/workloads/"+source+"/variables/export", nil, 200)
	f.call(cookie, "default", "POST", "/api/workloads/"+source+"/variables/import", map[string]string{"dotenv": "CHANGED=no"}, 403)
	f.call(cookie, "default", "POST", "/api/projects/variable-project/configuration/export", map[string]string{"environment_id": "variable-env"}, 200)
	f.call(cookie, "default", "POST", "/api/projects/variable-project/configuration/export", map[string]string{"environment_id": "target-env"}, 403)
	f.sql(`UPDATE team_roles SET permissions=ARRAY['view'] WHERE id='secret-reader'`)
	f.call(cookie, "default", "PUT", "/api/workloads/"+target+"/variable-bindings", resourceVariableBinding{Name: "BOUND", SourceID: source, SourceKey: "SECRET"}, 403)
}

func TestMergedHookRevalidatesSecondFactorAndAccountLockout(t *testing.T) {
	f, id, cookie := publicFixture(t)
	created := f.call(cookie, "default", "POST", "/api/workloads/"+id+"/deploy-hook", nil, 201)
	hook, token := jsonString(t, created, "id"), jsonString(t, created, "token")
	f.sql(`UPDATE teams SET require_second_factor=true WHERE id='default'`)
	publicCall(t, f, token, "", "POST", "/hooks/deploy/"+hook, map[string]string{"delivery": "policy"}, 401)
	sealed, err := seal(f.s.Key, []byte("12345678901234567890"))
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO account_totp(user_id,secret,confirmed) VALUES('public-owner',$1,true)`, sealed)
	publicCall(t, f, token, "", "POST", "/hooks/deploy/"+hook, map[string]string{"delivery": "policy"}, 202)
	f.sql(`INSERT INTO authentication_failures(key,locked_until) VALUES('account:public-owner',now()+interval '1 minute')`)
	f.sql(`UPDATE workloads SET webhook_secret=$2 WHERE id=$1`, id, []byte("fixture-other-trigger"))
	if err := f.s.dispatchWebhook(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	var cancelled bool
	if err := f.s.DB.QueryRow(t.Context(), `SELECT cancelled FROM deploy_hook_receipts WHERE hook_id=$1`, hook).Scan(&cancelled); err != nil || !cancelled {
		t.Fatal("locked creator's hook was not cancelled", cancelled, err)
	}
}

func TestMergedSchemaFreshAndBothUpgradePaths(t *testing.T) {
	database := os.Getenv("COVE_TEST_DATABASE_URL")
	if database == "" {
		t.Skip("COVE_TEST_DATABASE_URL is not set")
	}
	merged, err := assets.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"", "aef07fd9cd3a905599fccb98bb51d9d8fe6a8e32", "d09be3ce5e1d98e5dfba90a62cf78ee3e79332a0", "1cc37b32636f2ed374ace5b8f5eeffff9a53b21b", "4c30c1ebaef9a09b7b14cfbad92036501e84c08c", "e50327eb96832f9d012a549774c3228305fa6eaf", "c02877174627168a8f8c711bd8b857d63f41b421"} {
		t.Run(ref, func(t *testing.T) {
			db := openTestDatabase(t, database)
			if ref != "" {
				old, err := exec.Command("git", "show", ref+":internal/control/schema.sql").Output()
				if err != nil {
					t.Fatal("read pinned schema", err)
				}
				if _, err = db.Exec(t.Context(), string(old)); err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec(t.Context(), `INSERT INTO projects(id,name,team_id) VALUES('upgrade-sentinel','Preserved','default')`); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if _, err := db.Exec(context.Background(), string(merged)); err != nil {
					t.Fatal("schema not idempotent", err)
				}
			}
			var count int
			if err := db.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=ANY(ARRAY['access_tokens','team_roles','account_totp','configuration_drafts','template_repositories','project_status_shares'])`).Scan(&count); err != nil || count != 6 {
				t.Fatal("missing merged tables", count, err)
			}
			if ref != "" {
				var name string
				if err := db.QueryRow(t.Context(), `SELECT name FROM projects WHERE id='upgrade-sentinel'`).Scan(&name); err != nil || name != "Preserved" {
					t.Fatal("upgrade lost data", name, err)
				}
			}
		})
	}
}

func TestMergedPasskeyOriginAndOIDCCallbackBelowPath(t *testing.T) {
	s := &Server{PublicURL: "https://panel.example/control/cove"}
	web, err := s.webauthn()
	if err != nil {
		t.Fatal(err)
	}
	if len(web.Config.RPOrigins) != 1 || web.Config.RPOrigins[0] != "https://panel.example" || web.Config.RPID != "panel.example" {
		t.Fatal(web.Config)
	}
	if s.oidcRedirectURI("fixture") != "https://panel.example/control/cove/auth/oidc/fixture/callback" {
		t.Fatal("OIDC callback dropped prefix")
	}
}

func TestMergedProvidersUseConfiguredPublicProxy(t *testing.T) {
	targets := make(chan string, 2)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			t.Error("provider bypassed CONNECT validation", r.Method)
		}
		targets <- r.Host
		http.Error(w, "fixture refuses the connection", 502)
	}))
	defer proxy.Close()
	s := &Server{outboundState: &outboundProxyState{}}
	config := outbound.Config{HTTPS: proxy.URL}
	s.outboundState.value.Store(&config)
	p := connectedGitProvider{server: s, connection: gitConnection{URL: "https://8.8.8.8", Token: "fixture-provider-token"}}
	if err := p.api(t.Context(), "GET", "/repositories", nil, nil); err == nil {
		t.Fatal("refused proxy unexpectedly succeeded")
	}
	secret := httpVariableSource{server: s, config: secretProviderConfig{Kind: "vault", URL: "https://8.8.8.8", Token: "fixture-secret-token"}}
	if err := secret.read(t.Context(), "/v1/test", nil, &map[string]any{}); err == nil {
		t.Fatal("refused proxy unexpectedly succeeded")
	}
	for range 2 {
		select {
		case target := <-targets:
			if target != "8.8.8.8:443" {
				t.Fatal("proxy did not receive validated destination IP", target)
			}
		default:
			t.Fatal("provider did not use selected instance proxy")
		}
	}
}

func TestMergedProxyAndDriftPreparationPreservesOperationDigest(t *testing.T) {
	f, id := variablesFixture(t)
	first := outbound.Config{HTTPS: "http://first-proxy.example:3128"}
	f.s.outboundState.value.Store(&first)
	f.s.outboundState.saved.Store(true)
	operation := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", apiOperationInput{Action: "deploy"}, 202), "id")
	load := func() worker.Request {
		t.Helper()
		var sealed []byte
		if err := f.s.DB.QueryRow(t.Context(), `SELECT payload FROM operations WHERE id=$1`, operation).Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		var request worker.Request
		if err := f.s.unsealJSON(sealed, &request); err != nil {
			t.Fatal(err)
		}
		return request
	}
	request := load()
	if err := f.s.prepareInfrastructure(t.Context(), &request); err != nil {
		t.Fatal(err)
	}
	persisted := load()
	if persisted.OutboundProxy == nil || *persisted.OutboundProxy != first {
		t.Fatal("proxy was not sealed with drift preparation", persisted.OutboundProxy)
	}
	before, _ := json.Marshal(request)
	second := outbound.Config{HTTPS: "http://changed-proxy.example:3128"}
	f.s.outboundState.value.Store(&second)
	if err := f.s.prepareInfrastructure(t.Context(), &persisted); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(persisted)
	if string(before) != string(after) {
		t.Fatal("retry changed the worker request digest")
	}
	persisted.OutboundProxy = nil
	sealed, err := f.s.sealJSON(persisted)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE operations SET payload=$2,drift_prepared=false WHERE id=$1`, operation, sealed)
	before, _ = json.Marshal(persisted)
	if err := f.s.prepareInfrastructure(t.Context(), &persisted); err != nil {
		t.Fatal(err)
	}
	after, _ = json.Marshal(persisted)
	if string(before) != string(after) {
		t.Fatal("previously prepared legacy request was changed")
	}
}

func TestMergedCanonicalPolicyRoundTrip(t *testing.T) {
	f, _ := variablesFixture(t)
	base := "/api/projects/variable-project/configuration"
	response := f.call("root", "default", "GET", base, nil, 200)
	var view struct {
		Manifest projectConfiguration `json:"manifest"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	policy := &deploymentPolicy{Timezone: "America/New_York", OutsideWindow: "approval", Windows: []changeWindow{{Days: []int{1, 3, 5}, Start: "09:00", End: "17:00"}}, Freezes: []changeFreeze{{From: until.Add(-time.Hour), Until: until, Reason: "Reviewed freeze"}}}
	view.Manifest.Policy = policy
	raw, err := json.Marshal(view.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	draft := jsonString(t, f.call("root", "default", "POST", base+"/drafts", map[string]string{"content": string(raw)}, 201), "id")
	f.call("root", "default", "POST", base+"/drafts/"+draft+"/apply", nil, 200)
	response = f.call("root", "default", "GET", base, nil, 200)
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	got := view.Manifest.Policy
	if got == nil || got.Timezone != policy.Timezone || got.OutsideWindow != "approval" || len(got.Windows) != 1 || got.Windows[0].Start != "09:00" || len(got.Freezes) != 1 || got.Freezes[0].Reason != "Reviewed freeze" {
		t.Fatal("canonical import/export dropped governance", got)
	}
	got.Timezone = "not-a-zone"
	raw, _ = json.Marshal(view.Manifest)
	f.call("root", "default", "POST", base+"/drafts", map[string]string{"content": string(raw)}, 400)
}

func TestMergedPreviewDeploymentsInheritSourceEnvironmentFreeze(t *testing.T) {
	f, source := variablesFixture(t)
	create := func(environment string) string {
		f.sql(`INSERT INTO environments(id,name,project_id,team_id) VALUES($1,$1,'variable-project','default')`, environment)
		return jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: environment, NodeID: "variable-node", ProjectID: "variable-project", EnvironmentID: environment, Compose: "services:\n  app:\n    image: busybox:1.37\n"}, 201), "id")
	}
	full := create("environment-preview-target")
	pull := create("pull-request-target")
	f.sql(`INSERT INTO environment_previews(id,project_id,team_id,base_environment_id,environment_id,suffix,slug,state,stage,configuration,recipe_version) VALUES('governance-preview','variable-project','default','variable-env','environment-preview-target','fixture','fixture','ready','ready',$1,1)`, []byte{})
	f.sql(`INSERT INTO previews(source_id,number,workload_id,environment_id,revision,expires_at) VALUES($1,1,$2,'pull-request-target',$3,now()+interval '1 hour')`, source, pull, strings.Repeat("a", 40))
	f.sql(`INSERT INTO deployment_policies(team_id,environment_id,approval,freezes) VALUES('default','variable-env',true,jsonb_build_array(jsonb_build_object('from',now()-interval '1 minute','until',now()+interval '1 hour','reason','Source freeze')))`)
	for _, id := range []string{full, pull} {
		operation := resourceID()
		payload, err := f.s.sealJSON(worker.Request{ID: operation, Workload: id, Action: "deploy", Compose: "services:\n  app:\n    image: busybox:1.37\n"})
		if err != nil {
			t.Fatal(err)
		}
		f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'deploy',$3)`, operation, id, payload)
		var approval bool
		if err := f.s.DB.QueryRow(t.Context(), `SELECT approval_required FROM operations WHERE id=$1`, operation).Scan(&approval); err != nil || !approval {
			t.Fatal("preview bypassed source approval", id, approval, err)
		}
		allowed, err := f.s.admitDeployment(t.Context(), operation)
		if err != nil || allowed {
			t.Fatal("preview bypassed source freeze", id, allowed, err)
		}
	}
}
