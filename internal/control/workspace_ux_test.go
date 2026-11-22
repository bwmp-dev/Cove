package control

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestNotesSanitizeEncryptAndRejectStaleWrites(t *testing.T) {
	f, id, owner := publicFixture(t)
	path := "/api/workloads/" + id + "/notes"
	version := jsonString(t, f.call(owner, "default", "GET", path, nil, 200), "version")
	markdown := "# Runbook\n\n**Ready** [bad](javascript:alert%281%29)\n\n<script>alert(1)</script>\n\n![tracking](https://third-party.example/image)"
	response := f.call(owner, "default", "PUT", path, map[string]string{"version": version, "markdown": markdown}, 200)
	html := jsonString(t, response, "html")
	if !strings.Contains(html, "<h1>Runbook</h1>") || strings.Contains(html, "<script") || strings.Contains(html, "javascript:") || strings.Contains(html, "<img") {
		t.Fatal(html)
	}
	f.call(owner, "default", "PUT", path, map[string]string{"version": version, "markdown": "stale"}, 409)
	var encrypted []byte
	if err := f.s.DB.QueryRow(t.Context(), `SELECT content FROM workload_notes WHERE resource_id=$1`, id).Scan(&encrypted); err != nil || strings.Contains(string(encrypted), "Runbook") {
		t.Fatal("notes not sealed", err)
	}
	viewer := f.user("notes-viewer", "notes@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','notes-viewer','member','[{"scope":"environment","scope_id":"variable-env","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", path, nil, 200)
	f.call(viewer, "default", "PUT", path, map[string]string{"version": version, "markdown": "no"}, 403)
	f.sql(`UPDATE team_members SET grants='[]' WHERE user_id='notes-viewer'`)
	f.call(viewer, "default", "GET", path, nil, 403)
}

func TestTimezoneInheritanceValidationAndTeamAuthorization(t *testing.T) {
	f, _, owner := publicFixture(t)
	f.call(owner, "default", "PUT", "/api/teams/default/timezone", map[string]string{"timezone": "America/New_York"}, 200)
	response := f.call(owner, "default", "GET", "/api/preferences/timezone", nil, 200)
	if jsonString(t, response, "timezone") != "America/New_York" {
		t.Fatal(response.Body.String())
	}
	f.call(owner, "default", "PUT", "/api/preferences/timezone", map[string]string{"timezone": "Europe/London"}, 200)
	response = f.call(owner, "default", "GET", "/api/preferences/timezone", nil, 200)
	if jsonString(t, response, "timezone") != "Europe/London" || jsonString(t, response, "schedule_timezone") != "UTC" {
		t.Fatal(response.Body.String())
	}
	f.call(owner, "default", "PUT", "/api/preferences/timezone", map[string]string{"timezone": ""}, 200)
	f.call(owner, "default", "PUT", "/api/preferences/timezone", map[string]string{"timezone": "Local"}, 400)
	viewer := f.user("timezone-viewer", "timezone@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','timezone-viewer','member')`)
	f.call(viewer, "default", "PUT", "/api/teams/default/timezone", map[string]string{"timezone": "UTC"}, 403)
	for _, zone := range []string{"America/New_York", "Asia/Kathmandu", "UTC"} {
		if !validTimezone(zone, false) {
			t.Fatal(zone)
		}
	}
}

func TestProjectStatusSharingIsExplicitMinimalExpiringAndRevocable(t *testing.T) {
	f, id, owner := publicFixture(t)
	path := "/api/projects/variable-project/status-share"
	f.call(owner, "default", "GET", path, nil, 200)
	input := map[string]any{"title": "Public app", "days": 1, "acknowledge": true}
	response := f.call(owner, "default", "POST", path, input, 201)
	link, err := url.Parse(jsonString(t, response, "dashboard_url"))
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(link.Path, "/shared/")
	publicPath := "/api/public/projects/" + token
	public := f.call("", "", "GET", publicPath, nil, 200).Body.String()
	for _, private := range []string{"variable-project", "variable-env", "variable-node", "token_hash", "markdown", "configuration"} {
		if strings.Contains(public, `"`+private+`"`) {
			t.Fatal(public)
		}
	}
	if !strings.Contains(public, "Public app") {
		t.Fatal(public)
	}
	f.call(owner, "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "stop"}, 202)
	queued := f.call("", "", "GET", publicPath, nil, 200)
	if jsonString(t, queued, "state") != "pending" {
		t.Fatal("queued stop reported as completed", queued.Body.String())
	}
	badge := f.call("", "", "GET", "/status/"+token+"/badge.svg", nil, 200)
	if !strings.HasPrefix(badge.Header().Get("Content-Type"), "image/svg+xml") || !strings.Contains(badge.Body.String(), "<svg") {
		t.Fatal(badge.Body.String())
	}
	response = f.call(owner, "default", "GET", path, nil, 200)
	if strings.Contains(response.Body.String(), token) {
		t.Fatal("saved share token returned")
	}
	response = f.call(owner, "default", "POST", path, input, 201)
	link, err = url.Parse(jsonString(t, response, "dashboard_url"))
	if err != nil {
		t.Fatal(err)
	}
	currentPath := "/api/public/projects/" + strings.TrimPrefix(link.Path, "/shared/")
	f.call("", "", "GET", publicPath, nil, 404)
	f.call("", "", "GET", currentPath, nil, 200)
	f.sql(`UPDATE project_status_shares SET expires_at=now()-interval '1 minute'`)
	f.call("", "", "GET", currentPath, nil, 404)
	response = f.call(owner, "default", "POST", path, input, 201)
	link, err = url.Parse(jsonString(t, response, "dashboard_url"))
	if err != nil {
		t.Fatal(err)
	}
	currentPath = "/api/public/projects/" + strings.TrimPrefix(link.Path, "/shared/")
	f.call("", "", "GET", currentPath, nil, 200)
	f.call(owner, "default", "DELETE", path, nil, 200)
	f.call("", "", "GET", currentPath, nil, 404)
}

func TestResourceTagsPinsAndBulkRespectIndividualGrants(t *testing.T) {
	f, id, owner := publicFixture(t)
	path := "/api/workloads/" + id + "/tags"
	f.call(owner, "default", "PUT", path, map[string]any{"tags": []string{"Production", "api", "api"}}, 200)
	f.call(owner, "default", "PUT", path, map[string]any{"tags": []string{"bad tag"}}, 400)
	pin := map[string]any{"kind": "workload", "id": id, "pinned": true}
	f.call(owner, "default", "PUT", "/api/pins", pin, 200)
	page := f.call(owner, "default", "GET", "/api/resource-browser?tag=production&pinned=true&page_size=1", nil, 200)
	if !strings.Contains(page.Body.String(), id) || !strings.Contains(page.Body.String(), `"total":1`) {
		t.Fatal(page.Body.String())
	}
	page = f.call(owner, "default", "GET", "/api/resource-browser?tag=production&page=2&page_size=1", nil, 200)
	if !strings.Contains(page.Body.String(), `"total":1`) || !strings.Contains(page.Body.String(), `"items":[]`) {
		t.Fatal(page.Body.String())
	}
	viewer := f.user("ux-viewer", "ux-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','ux-viewer','member','[{"scope":"environment","scope_id":"variable-env","role":"viewer"}]')`)
	if strings.Contains(f.call(viewer, "default", "GET", "/api/pins", nil, 200).Body.String(), id) {
		t.Fatal("another user's pin exposed")
	}
	f.call(viewer, "default", "PUT", path, map[string]any{"tags": []string{}}, 403)
	input := map[string]any{"ids": []string{id, "nonexistent"}, "action": "stop", "acknowledge": true}
	result := f.call(viewer, "default", "POST", "/api/bulk-actions", input, 200)
	if !strings.Contains(result.Body.String(), `"status":403`) || !strings.Contains(result.Body.String(), `"status":404`) {
		t.Fatal(result.Body.String())
	}
	f.sql(`UPDATE users SET name='administrator API token' WHERE id='public-owner'`)
	result = f.call(owner, "default", "POST", "/api/bulk-actions", input, 200)
	if !strings.Contains(result.Body.String(), `"status":202`) {
		t.Fatal(result.Body.String())
	}
	var audits int
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE subject=$1 AND action='POST /api/workloads/{id}/operations'`, id).Scan(&audits); err != nil || audits < 2 {
		t.Fatal(audits, err)
	}
	var auditActor string
	if err := f.s.DB.QueryRow(t.Context(), `SELECT actor FROM audit_events WHERE subject=$1 AND action='POST /api/workloads/{id}/operations' AND status=202 ORDER BY id DESC LIMIT 1`, id).Scan(&auditActor); err != nil || auditActor != "user public-owner" {
		t.Fatal("bulk audit did not retain creator identity", auditActor, err)
	}
	f.sql(`UPDATE team_members SET grants='[]' WHERE user_id='ux-viewer'`)
	f.call(viewer, "default", "PUT", "/api/pins", pin, 403)
	if strings.Contains(f.call(viewer, "default", "GET", "/api/resource-browser", nil, 200).Body.String(), id) {
		t.Fatal("lost grant still visible")
	}
	f.call(owner, "default", "GET", "/api/resource-browser?page=0", nil, 400)
	input["ids"] = []string{id, id}
	f.call(owner, "default", "POST", "/api/bulk-actions", input, 400)
}

func TestWorkspaceActivityAndOnboardingUseRealVisibleState(t *testing.T) {
	f, id, owner := publicFixture(t)
	viewer := f.user("activity-viewer", "activity@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','activity-viewer','member','[]')`)
	f.sql(`INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES('private-actor','PUT /api/workloads/{id}',$1,200,'default')`, id)
	if strings.Contains(f.call(viewer, "default", "GET", "/api/activity", nil, 200).Body.String(), "configuration") {
		t.Fatal("hidden activity exposed")
	}
	result := f.call(owner, "default", "GET", "/api/activity", nil, 200).Body.String()
	if !strings.Contains(result, "configuration") || strings.Contains(result, "private-actor") {
		t.Fatal(result)
	}
	result = f.call(owner, "default", "GET", "/api/onboarding", nil, 200).Body.String()
	var data struct {
		Complete  bool `json:"complete"`
		Dismissed bool `json:"dismissed"`
	}
	if json.Unmarshal([]byte(result), &data) != nil || data.Complete {
		t.Fatal(result)
	}
	f.call(owner, "default", "PUT", "/api/onboarding", map[string]any{"dismissed": true}, 200)
	if !strings.Contains(f.call(owner, "default", "GET", "/api/onboarding", nil, 200).Body.String(), `"dismissed":true`) {
		t.Fatal("skip not saved")
	}
	if strings.Contains(f.call(viewer, "default", "GET", "/api/onboarding", nil, 200).Body.String(), `"dismissed":true`) {
		t.Fatal("skip shared across users")
	}
}
