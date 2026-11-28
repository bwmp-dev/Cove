package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDirectoryMergeDeprovisionRevokesOwnedAutomation(t *testing.T) {
	f := newSCIMFixture(t)
	f.call("root", "default", "POST", "/api/notifications", connectionConfig{Name: "Directory outcomes", Notification: &notificationConfig{Provider: "webhook", URL: "https://notify.example.test/identity", Enabled: true, Events: []string{"identity.provisioned", "identity.deprovisioned"}}}, 201)
	user, _ := f.request("POST", "/scim/v2/Users", scimFixtureUser("automation@example.invalid", "automation"), 201)
	owner := user["id"].(string)
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default',$1,'member','[{"scope":"team","scope_id":"default","role":"editor"}]')`, owner)
	cookie := randomToken()
	f.sql(`INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 hour')`, tokenHash(cookie), owner)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('directory-node','Node','localhost',22,'',$1,'ready','{"job_runs":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('directory-project','Project','default')`)
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Job", NodeID: "directory-node", ProjectID: "directory-project", Kind: "job", JobService: "app", Compose: "services:\n  app:\n    image: busybox:1.37\n"}, 201), "id")
	f.call(cookie, "default", "PUT", "/api/workloads/"+id+"/schedule", map[string]any{"cron": "* * * * *"}, 200)
	share := f.call(cookie, "default", "POST", "/api/projects/directory-project/status-share", map[string]any{"title": "Public", "days": 1, "acknowledge": true}, 201)
	status := f.call(cookie, "default", "PUT", "/api/projects/directory-project/status-page", map[string]any{"enabled": true, "title": "Owned uptime"}, 200)
	statusSlug := jsonString(t, status, "slug")
	shareURL := jsonString(t, share, "dashboard_url")
	shareToken := shareURL[strings.LastIndex(shareURL, "/")+1:]
	f.call(cookie, "default", "POST", "/api/workloads/"+id+"/deploy-hook", map[string]any{"days": 1}, 201)
	_, token := mintPublic(t, f.accountFixture, cookie, "user", "workloads:read")
	publicCall(t, f.accountFixture, token, "default", "GET", "/api/v1/workloads/"+id, nil, 200)
	f.sql(`UPDATE workloads SET next_job_at=now()-interval '1 minute' WHERE id=$1`, id)
	if err := f.s.scheduleJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.request("PATCH", "/scim/v2/Users/"+owner, scimFixturePatch("active", false), 200)
	var deliveries int
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM health_deliveries WHERE event='identity.deprovisioned'`).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatal("deprovision event did not reach the operations queue", deliveries, err)
	}
	var tokens, hooks, shares int
	var suspended, cancelled bool
	err := f.s.DB.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM access_tokens WHERE user_id=$1),(SELECT count(*) FROM deploy_hooks WHERE creator_id=$1),(SELECT count(*) FROM project_status_shares WHERE creator_id=$1),job_schedule_creator_id IS NULL AND next_job_at IS NULL AND job_pending_at IS NULL,(SELECT cancel_requested FROM operations WHERE workload_id=$2 AND state='queued') FROM workloads WHERE id=$2`, owner, id).Scan(&tokens, &hooks, &shares, &suspended, &cancelled)
	if err != nil || tokens != 0 || hooks != 0 || shares != 0 || !suspended || !cancelled {
		t.Fatal("deprovision retained owned access", tokens, hooks, shares, suspended, cancelled, err)
	}
	var operationID string
	if err := f.s.DB.QueryRow(t.Context(), `SELECT operation_id FROM scheduled_job_runs WHERE creator_id=$1`, owner).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	if proceed, err := f.s.scheduledJobDispatch(t.Context(), operationID, false); err != nil || proceed {
		t.Fatal("never-submitted revoked job was not cancelled locally", proceed, err)
	}
	var state string
	if err := f.s.DB.QueryRow(t.Context(), `SELECT state FROM operations WHERE id=$1`, operationID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatal("cancelled scheduled job retained workload lock", state, err)
	}
	f.sql(`UPDATE operations SET state='queued',cancel_requested=false WHERE id=$1`, operationID)
	f.sql(`UPDATE scheduled_job_runs SET dispatch_started=true WHERE operation_id=$1`, operationID)
	if proceed, err := f.s.scheduledJobDispatch(t.Context(), operationID, false); err != nil || !proceed {
		t.Fatal("uncertain submission must reconcile cancellation with the worker", proceed, err)
	}
	if err := f.s.DB.QueryRow(t.Context(), `SELECT state,cancel_requested FROM operations WHERE id=$1`, operationID).Scan(&state, &cancelled); err != nil || state != "queued" || !cancelled {
		t.Fatal("uncertain submission prematurely released the workload lock", state, cancelled, err)
	}
	if proceed, err := f.s.scheduledJobDispatch(t.Context(), operationID, true); err != nil || proceed {
		t.Fatal("revoked uncertain operation was resubmitted", proceed, err)
	}
	publicCall(t, f.accountFixture, token, "default", "GET", "/api/v1/session", nil, 401)
	publicCall(t, f.accountFixture, "", "", "GET", "/api/public/projects/"+shareToken, nil, 404)
	publicCall(t, f.accountFixture, "", "", "GET", "/api/status/"+statusSlug, nil, 404)
	f.request("PATCH", "/scim/v2/Users/"+owner, scimFixturePatch("active", true), 200)
	publicCall(t, f.accountFixture, token, "default", "GET", "/api/v1/session", nil, 401)
	if !strings.Contains(f.call("root", "default", "GET", "/api/workloads/"+id+"/schedule", nil, 200).Body.String(), `"ownership_required":true`) {
		t.Fatal("reactivation silently reclaimed a schedule")
	}
}

func TestDirectoryMergeLegacyOwnershipAndCurrentPermissions(t *testing.T) {
	f, _, cookie := publicFixture(t)
	f.call("root", "default", "POST", "/api/notifications", connectionConfig{Name: "Directory failure", Notification: &notificationConfig{Provider: "webhook", URL: "https://notify.example.test/identity", Enabled: true, Events: []string{"identity.sync_failed"}}}, 201)
	f.sql(`INSERT INTO audit_events(actor,action,subject,status) VALUES('account authentication','POST /auth/ldap/{provider}/sign-in','directory',502),('account authentication','POST /auth/ldap/{provider}/sign-in','directory',401)`)
	var failures int
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM health_deliveries WHERE event='identity.sync_failed'`).Scan(&failures); err != nil || failures != 1 {
		t.Fatal("directory failure routing included credential failures or lost provider failures", failures, err)
	}
	legacySlug := strings.Repeat("a", 64)
	f.sql(`INSERT INTO project_status_pages(project_id,title,enabled,slug) VALUES('variable-project','Legacy uptime',true,$1)`, legacySlug)
	publicCall(t, f, "", "", "GET", "/api/status/"+legacySlug, nil, 404)
	claimed := f.call(cookie, "default", "PUT", "/api/projects/variable-project/status-page", map[string]any{"enabled": true, "title": "Claimed uptime"}, 200)
	if jsonString(t, claimed, "slug") == legacySlug {
		t.Fatal("unknown uptime share owner was claimed without revoking the old link")
	}
	token := randomToken()
	f.sql(`INSERT INTO project_status_shares(project_id,title,token_hash,expires_at) VALUES('variable-project','Legacy',$1,now()+interval '1 day')`, tokenHash(token))
	publicCall(t, f, "", "", "GET", "/api/public/projects/"+token, nil, 404)
	metadata := f.call(cookie, "default", "GET", "/api/projects/variable-project/status-share", nil, 200).Body.String()
	if !strings.Contains(metadata, `"ownership_required":true`) || !strings.Contains(metadata, `"enabled":false`) {
		t.Fatal("legacy sharing was silently assigned")
	}
	f.call(cookie, "default", "POST", "/api/projects/variable-project/status-share", map[string]any{"title": "Claimed", "days": 1, "acknowledge": true}, 201)
	f.sql(`UPDATE team_members SET role='member',grants='[{"scope":"team","role":"viewer"}]' WHERE user_id='public-owner'`)
	var allowed bool
	if err := f.s.DB.QueryRow(t.Context(), `SELECT cove_automation_allowed('public-owner','default','variable-project','variable-env','edit_configuration')`).Scan(&allowed); err != nil || allowed {
		t.Fatal("removed current grant retained automation", allowed, err)
	}
}

func TestDirectoryMergeScopedAdministrationAndSeparateSCIMCredential(t *testing.T) {
	f := newSCIMFixture(t)
	cookie := f.user("directory-admin", "directory-admin@example.invalid")
	f.sql(`UPDATE users SET is_admin=true WHERE id='directory-admin'`)
	_, token := mintPublic(t, f.accountFixture, cookie, "user", "directory:manage")
	publicCall(t, f.accountFixture, token, "default", "GET", "/api/v1/settings/scim", nil, 200)
	f.request("GET", "/scim/v2/Users", nil, 401, map[string]string{"Authorization": "Bearer " + token})
	publicCall(t, f.accountFixture, f.token, "default", "GET", "/api/v1/settings/scim", nil, 401)
	f.sql(`UPDATE users SET is_admin=false WHERE id='directory-admin'`)
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','directory-admin','admin')`)
	publicCall(t, f.accountFixture, token, "default", "GET", "/api/v1/settings/scim", nil, 403)
	f.sql(`UPDATE users SET is_admin=true WHERE id='directory-admin'`)
	f.sql(`UPDATE control_settings SET require_second_factor=true`)
	publicCall(t, f.accountFixture, token, "default", "GET", "/api/v1/settings/scim", nil, 403)
	f.sql(`UPDATE control_settings SET require_second_factor=false,panel_allowlist=ARRAY['203.0.113.0/24']`)
	publicCall(t, f.accountFixture, token, "default", "GET", "/api/v1/settings/scim", nil, 403)
}

func TestDirectoryMergeProtocolDocumentationAndPrefixedCallbacks(t *testing.T) {
	s := &Server{PublicURL: "https://panel.example/control/cove"}
	document := s.openAPIDocument()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"/control/cove/scim/v2", "directory:manage", "Separate instance provisioning token", "bind_password", "writeOnly", "/settings/scim", "/settings/ldap/providers"} {
		if !strings.Contains(string(raw), expected) {
			t.Fatal("missing protocol contract", expected)
		}
	}
	f := newAccountFixture(t)
	f.s.PublicURL = s.PublicURL
	r := httptest.NewRequest(http.MethodGet, "/control/cove/auth/github/callback", nil)
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, r)
	if w.Code != 303 || w.Header().Get("Location") != "/control/cove/login?error=oauth_failed" {
		t.Fatal("callback lost base path", w.Code, w.Header().Get("Location"))
	}
}

func TestDirectoryMergeManagedCustomRolesLimitExistingTokens(t *testing.T) {
	f, id := variablesFixture(t)
	cookie := f.user("directory-only", "directory-only@example.invalid")
	f.sql(`INSERT INTO team_roles(team_id,id,name,permissions) VALUES('default','directory-config','Configuration without secrets',ARRAY['view','edit_configuration'])`)
	reconcile := func(role string) {
		t.Helper()
		tx, err := f.s.identityTransaction(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		claims := []directoryClaim{}
		if role != "" {
			membership, err := directoryRole(t.Context(), tx, "default", role)
			if err != nil {
				t.Fatal(err)
			}
			claims = append(claims, directoryClaim{Group: "engineering", Team: "default", User: "directory-only", memberInput: membership})
		}
		if _, err := syncDirectoryMemberships(t.Context(), tx, "github", "fixture", "directory-only", claims, "fixture"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	reconcile("editor")
	_, token := mintPublic(t, f, cookie, "user", "workloads:read", "secrets:read")
	publicCall(t, f, token, "default", "POST", "/api/v1/workloads/"+id+"/variables/export", nil, 200)
	reconcile("directory-config")
	publicCall(t, f, token, "default", "GET", "/api/v1/workloads/"+id, nil, 200)
	publicCall(t, f, token, "default", "POST", "/api/v1/workloads/"+id+"/variables/export", nil, 403)
	reconcile("")
	publicCall(t, f, token, "default", "GET", "/api/v1/workloads/"+id, nil, 401)
}
