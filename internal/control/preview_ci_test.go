package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newCIPreviewFixture(t *testing.T) (*previewFixture, string, string, previewCIEvent) {
	f := newPreviewFixture(t)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"preview_ci_images":true}'`)
	id := f.resource("web", workloadInput{Compose: "services:\n  web:\n    image: busybox:1.37\n", Source: &worker.GitSource{Repository: "https://git.example.test/app.git", Branch: "main", InlineCompose: true}, Routes: []worker.Route{{Domain: "app.example.test", Service: "web", Port: 8080}}}, 0, sha("production"))
	registry := jsonString(t, f.call("root", "default", "POST", "/api/registries", connectionConfig{Name: "CI fixture", Registry: &worker.Registry{Server: "registry.example.test", Username: "fixture", Password: "fixture-private-registry-password"}}, 201), "id")
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/registry", map[string]string{"registry_id": registry}, 200)
	recipe := defaultPreviewRecipe()
	recipe.Enabled = true
	recipe.BaseEnvironment = "prod"
	recipe.Resources = map[string]previewResourcePolicy{id: {Services: map[string]bool{"web": true}}}
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", recipe, 200)
	token := jsonString(t, f.call("root", "default", "PUT", "/api/projects/p/preview-images", map[string]any{"resource_ids": []string{id}, "days": 7}, 201), "token")
	event := previewCIEvent{Delivery: "build-1", Preview: "pull-42", Sequence: 1, Revision: sha("CI build"), Images: map[string]map[string]string{id: {"web": "registry.example.test/team/app@" + imageID("CI image")}}}
	return f, id, token, event
}

func ciSubmit(t *testing.T, f *previewFixture, token string, event any, status int) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(event)
	r := httptest.NewRequest("POST", "/hooks/preview-images/p", bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("CI submission got %d want %d: %s", w.Code, status, w.Body.String())
	}
	return w
}

func ciApprove(t *testing.T, f *previewFixture, id string) {
	t.Helper()
	response := f.call("root", "default", "GET", "/api/environment-previews/"+id, nil, 200)
	var view struct {
		Inputs *previewCIInputView `json:"ci_inputs"`
	}
	if json.Unmarshal(response.Body.Bytes(), &view) != nil || view.Inputs == nil {
		t.Fatal("CI input review metadata is unavailable")
	}
	f.call("root", "default", "POST", "/api/environment-previews/"+id+"/approve", map[string]string{"ci_identity": view.Inputs.Identity}, 202)
}

func TestPreviewCIApprovalRequiresReviewedCurrentInputs(t *testing.T) {
	f, base, token, event := newCIPreviewFixture(t)
	preview := jsonString(t, ciSubmit(t, f, token, event, 202), "preview_id")
	response := f.call("root", "default", "GET", "/api/environment-previews/"+preview, nil, 200)
	var view struct {
		Inputs previewCIInputView `json:"ci_inputs"`
	}
	if json.Unmarshal(response.Body.Bytes(), &view) != nil || view.Inputs.Identity != previewCIInputIdentity(event.Revision, event.Images) || view.Inputs.Revision != event.Revision || view.Inputs.Images[base]["web"] != event.Images[base]["web"] {
		t.Fatal("pending inputs cannot be reviewed", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "registry_id") || strings.Contains(response.Body.String(), "registry_server") || strings.Contains(response.Body.String(), "fixture-private-registry-password") {
		t.Fatal("review exposed credential bindings")
	}
	f.call("root", "default", "POST", "/api/environment-previews/"+preview+"/approve", nil, 409)
	f.call("root", "default", "POST", "/api/environment-previews/"+preview+"/approve", map[string]string{"ci_identity": "unreviewed"}, 409)
	event.Sequence, event.Delivery, event.Revision = 2, "changed-during-review", sha("changed during review")
	event.Images = map[string]map[string]string{base: {"web": "registry.example.test/team/app@" + imageID("changed during review")}}
	ciSubmit(t, f, token, event, 202)
	f.call("root", "default", "POST", "/api/environment-previews/"+preview+"/approve", map[string]string{"ci_identity": view.Inputs.Identity}, 409)
	f.settle()
	if state, _, _ := f.previewState(preview); state != "awaiting_approval" || len(f.submitted) != 0 {
		t.Fatal("stale review approved different inputs", state)
	}
	ciApprove(t, f, preview)
	f.settle()
	if requests := f.submittedFor(f.copyOf(preview, base), "deploy"); len(requests) != 1 || requests[0].ImagePins["web"] != event.Images[base]["web"] {
		t.Fatal("reviewed inputs were not deployed", requests)
	}
}

func TestPreviewCICompleteReplayApprovalAndPullOnlyWorkflow(t *testing.T) {
	f, base, token, event := newCIPreviewFixture(t)
	status := f.call("root", "default", "GET", "/api/projects/p/preview-images", nil, 200)
	if strings.Contains(status.Body.String(), token) || strings.Contains(status.Body.String(), "fixture-private-registry-password") {
		t.Fatal("secret returned in status")
	}
	partial := event
	partial.Images = map[string]map[string]string{base: {}}
	ciSubmit(t, f, token, partial, 400)
	var count int
	f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM environment_previews`).Scan(&count)
	if count != 0 {
		t.Fatal("partial submission created a preview")
	}
	result := ciSubmit(t, f, token, event, 202)
	preview := jsonString(t, result, "preview_id")
	f.handler = f.s.Handler()
	if !strings.Contains(ciSubmit(t, f, token, event, 202).Body.String(), `"duplicate":true`) {
		t.Fatal("exact retry not deduplicated")
	}
	conflict := event
	conflict.Revision = sha("conflict")
	ciSubmit(t, f, token, conflict, 409)
	stale := event
	stale.Delivery = "other-delivery"
	ciSubmit(t, f, token, stale, 409)
	f.settle()
	state, _, _ := f.previewState(preview)
	if state != "awaiting_approval" {
		t.Fatal(state)
	}
	if len(f.submitted) != 0 {
		t.Fatal("approval bypassed")
	}
	ciApprove(t, f, preview)
	f.settle()
	state, stage, detail := f.previewState(preview)
	if state != "ready" {
		t.Fatal(state, stage, detail)
	}
	copy := f.copyOf(preview, base)
	requests := f.submittedFor(copy, "deploy")
	if len(requests) != 1 || requests[0].Source != nil || requests[0].ImagePins["web"] != event.Images[base]["web"] || requests[0].EnvironmentPreview.ImageRegistry != "registry.example.test" || requests[0].Registry == nil {
		t.Fatal("wrong CI deployment", requests)
	}
	view := f.call("root", "default", "GET", "/api/environment-previews/"+preview, nil, 200)
	if !strings.Contains(view.Body.String(), `"source":"ci"`) || !strings.Contains(view.Body.String(), event.Revision) {
		t.Fatal(view.Body.String())
	}
	second := event
	second.Delivery = "build-2"
	second.Sequence = 2
	second.Revision = sha("second")
	second.Images = map[string]map[string]string{base: {"web": "registry.example.test/team/app@" + imageID("second")}}
	ciSubmit(t, f, token, second, 202)
	f.settle()
	if len(f.submittedFor(copy, "deploy")) != 1 {
		t.Fatal("updated input bypassed approval")
	}
	ciApprove(t, f, preview)
	f.settle()
	if len(f.submittedFor(copy, "deploy")) != 2 {
		t.Fatal("image-only change missing from desired identity")
	}
	ciSubmit(t, f, token, event, 202)
	if len(f.submittedFor(copy, "deploy")) != 2 {
		t.Fatal("replay queued work")
	}
	ciSubmit(t, f, token, map[string]any{"delivery_id": "untrusted", "preview": "pull-43", "sequence": 1, "revision": event.Revision, "images": event.Images, "compose": "untrusted"}, 400)
	for _, path := range []string{"/api/projects/p/preview-images", "/api/workloads/" + base, "/api/operations"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("CI token became an API credential", path, w.Code)
		}
	}
	f.call("root", "default", "DELETE", "/api/projects/p/preview-images", nil, 200)
	ciSubmit(t, f, token, second, 401)
}

func TestPreviewCIScopeExpiryAndClosedKeyRemainSafe(t *testing.T) {
	f, base, token, event := newCIPreviewFixture(t)
	bad := event
	bad.Images = map[string]map[string]string{base: {"web": "registry.example.test/team/app:stable@" + imageID("x")}}
	ciSubmit(t, f, token, bad, 400)
	bad.Images = map[string]map[string]string{base: {"web": "other.example.test/team/app@" + imageID("x")}}
	ciSubmit(t, f, token, bad, 400)
	preview := jsonString(t, ciSubmit(t, f, token, event, 202), "preview_id")
	f.call("root", "default", "DELETE", "/api/environment-previews/"+preview, map[string]string{"confirm": "pull-42"}, 202)
	newEvent := event
	newEvent.Sequence = 2
	newEvent.Delivery = "after-close"
	ciSubmit(t, f, token, newEvent, 410)
	f.settle()
	ciSubmit(t, f, token, newEvent, 410)
	newToken := jsonString(t, f.call("root", "default", "PUT", "/api/projects/p/preview-images", map[string]any{"resource_ids": []string{base}, "days": 2}, 201), "token")
	ciSubmit(t, f, token, event, 401)
	ciSubmit(t, f, newToken, newEvent, 410)
	f.sql(`UPDATE preview_ci_credentials SET expires_at=now()-interval '1 second'`)
	ciSubmit(t, f, newToken, event, 401)
}

func TestPreviewCIParsingAndIdentity(t *testing.T) {
	bindings := map[string]previewCIBinding{"resource": {Server: "registry.example.test", Services: []string{"web", "db"}}}
	event := previewCIEvent{Delivery: "build-1", Preview: "pr-1", Sequence: 1, Revision: sha("revision"), Images: map[string]map[string]string{"resource": {"web": "registry.example.test/app@" + imageID("web"), "db": "registry.example.test/db@" + imageID("db")}}}
	if err := validatePreviewCIEvent(event, bindings); err != nil {
		t.Fatal(err)
	}
	delete(event.Images["resource"], "db")
	if validatePreviewCIEvent(event, bindings) == nil {
		t.Fatal("partial map accepted")
	}
	for _, compose := range []string{"services: {}", "include: other.yaml\nservices:\n  web:\n    image: app", "services:\n  web:\n    extends: base"} {
		if _, err := previewCIServices(compose); err == nil {
			t.Fatal("unbounded service expansion accepted")
		}
	}
	one := previewResourceRow{BaselineRelease: "production", CIIdentity: "first"}
	two := one
	two.CIIdentity = "second"
	if one.desiredKey() == two.desiredKey() || one.desiredKey() == "baseline:production" {
		t.Fatal("CI identity ignored")
	}
}

func TestPreviewCICredentialPermissionsAndScopeChanges(t *testing.T) {
	f, base, token, event := newCIPreviewFixture(t)
	viewer := f.user("ci-viewer", "ci-viewer@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','ci-viewer','member','[{"scope":"project","scope_id":"p","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", "/api/projects/p/preview-images", nil, 200)
	f.call(viewer, "default", "PUT", "/api/projects/p/preview-images", map[string]any{"resource_ids": []string{base}, "days": 7}, 403)
	f.call(viewer, "default", "DELETE", "/api/projects/p/preview-images", nil, 403)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('other-project','Other','default')`)
	raw, _ := json.Marshal(event)
	r := httptest.NewRequest("POST", "/hooks/preview-images/other-project", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("project scope escaped", w.Code)
	}
	f.sql(`UPDATE preview_recipes SET version=version+1 WHERE project_id='p'`)
	ciSubmit(t, f, token, event, 409)
	owner := f.user("ci-owner", "ci-owner@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','ci-owner','member','[{"scope":"project","scope_id":"p","role":"editor"}]')`)
	token = jsonString(t, f.call(owner, "default", "PUT", "/api/projects/p/preview-images", map[string]any{"resource_ids": []string{base}, "days": 7}, 201), "token")
	f.sql(`UPDATE team_members SET grants='[{"scope":"project","scope_id":"p","role":"viewer"}]' WHERE user_id='ci-owner'`)
	ciSubmit(t, f, token, event, 401)
	metadata := f.call("root", "default", "GET", "/api/projects/p/preview-images", nil, 200)
	if !strings.Contains(metadata.Body.String(), `"matches_scope":false`) {
		t.Fatal("revoked owner appeared usable", metadata.Body.String())
	}
	token = jsonString(t, f.call("root", "default", "PUT", "/api/projects/p/preview-images", map[string]any{"resource_ids": []string{base}, "days": 7}, 201), "token")
	f.sql(`UPDATE nodes SET capabilities=capabilities- 'preview_ci_images' WHERE id='n1'`)
	ciSubmit(t, f, token, event, 409)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"preview_ci_images":true}' WHERE id='n1'`)
	changed := workloadInput{Name: "web", NodeID: "n1", Kind: "service", Compose: "services:\n  web:\n    image: busybox:1.37\n", Environment: map[string]string{"SCOPE": "changed"}}
	f.call("root", "default", "PUT", "/api/workloads/"+base, changed, 200)
	ciSubmit(t, f, token, event, 409)
	registry := jsonString(t, f.call("root", "default", "POST", "/api/registries", connectionConfig{Name: "Ambiguous registry", Registry: &worker.Registry{Server: "registry", Username: "fixture", Password: "fixture"}}, 201), "id")
	f.call("root", "default", "PUT", "/api/workloads/"+base+"/registry", map[string]string{"registry_id": registry}, 200)
	refused := f.call("root", "default", "PUT", "/api/projects/p/preview-images", map[string]any{"resource_ids": []string{base}, "days": 7}, 409)
	if !strings.Contains(refused.Body.String(), "single-label prefix would select Docker Hub") {
		t.Fatal("ambiguous registry was not explained", refused.Body.String())
	}
}

func TestPreviewCIHistoryBoundsAndActiveLimit(t *testing.T) {
	f, _, token, event := newCIPreviewFixture(t)
	preview := jsonString(t, ciSubmit(t, f, token, event, 202), "preview_id")
	f.sql(`INSERT INTO preview_ci_deliveries(project_id,delivery_id,fingerprint) SELECT 'p','old-'||n,'old' FROM generate_series(1,$1) n`, previewCIMaxDeliveries-1)
	event.Sequence = 2
	event.Delivery = "next-delivery"
	ciSubmit(t, f, token, event, 202)
	var count int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM preview_ci_deliveries WHERE project_id='p'`).Scan(&count); err != nil || count != previewCIMaxDeliveries {
		t.Fatal("unbounded delivery ledger", count, err)
	}
	f.sql(`INSERT INTO preview_ci_slots(project_id,suffix,sequence,delivery_id,fingerprint) SELECT 'p','closed-'||n,1,'fixture','fixture' FROM generate_series(1,$1) n`, previewCIMaxSlots-1)
	event.Preview = "new-key"
	event.Sequence = 1
	event.Delivery = "new-key-delivery"
	ciSubmit(t, f, token, event, 409)
	f.sql(`DELETE FROM preview_ci_slots WHERE suffix LIKE 'closed-%'`)
	var snapshot previewSnapshot
	row, _ := f.s.loadPreviewRow(context.Background(), f.s.DB, preview, false)
	snapshot = row.Snapshot
	snapshot.Recipe.MaxPreviews = 1
	raw, _ := f.s.sealJSON(snapshot.Recipe)
	f.sql(`UPDATE preview_recipes SET configuration=$1 WHERE project_id='p'`, raw)
	ciSubmit(t, f, token, event, 409)
	var previews int
	f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM environment_previews`).Scan(&previews)
	if previews != 1 {
		t.Fatal("active limit bypassed", previews)
	}
}

func TestPreviewCIPendingRegistryReferencesAndApprovalRace(t *testing.T) {
	f, base, token, event := newCIPreviewFixture(t)
	preview := jsonString(t, ciSubmit(t, f, token, event, 202), "preview_id")
	tx, err := f.s.DB.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	refs, err := f.s.previewCIRegistryReferences(context.Background(), tx, "default", "registry.example.test")
	tx.Rollback(context.Background())
	want := strings.TrimPrefix(event.Images[base]["web"], "registry.example.test/")
	if err != nil || len(refs) != 1 || refs[0] != want {
		t.Fatal("pending approval pin not protected", refs, err)
	}
	ciApprove(t, f, preview)
	f.settle()
	copy := f.copyOf(preview, base)
	old, _ := f.s.loadPreviewRow(context.Background(), f.s.DB, preview, false)
	event.Sequence = 2
	event.Delivery = "new-approval"
	event.Revision = sha("new commit")
	ciSubmit(t, f, token, event, 202)
	// The reconciler may have loaded the old deploy stage before the new submission took its lock.
	old.State, old.Stage = "updating", "deploy"
	before := len(f.submittedFor(copy, "deploy"))
	if err = f.s.deployPreviewStage(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	f.settle()
	if len(f.submittedFor(copy, "deploy")) != before {
		t.Fatal("racing submission bypassed approval")
	}
	p, _ := f.s.loadPreviewRow(context.Background(), f.s.DB, preview, false)
	p.Snapshot.CI.Images[base] = map[string]string{}
	raw, _ := f.s.sealJSON(p.Snapshot)
	f.sql(`UPDATE environment_previews SET configuration=$1 WHERE id=$2`, raw, preview)
	tx, _ = f.s.DB.Begin(context.Background())
	_, err = f.s.previewCIRegistryReferences(context.Background(), tx, "default", "registry.example.test")
	tx.Rollback(context.Background())
	if err == nil {
		t.Fatal("incomplete CI registry inventory accepted")
	}
}

func TestPreviewCIBoundedPayloadAndConcurrentDelivery(t *testing.T) {
	f, _, token, event := newCIPreviewFixture(t)
	raw, _ := json.Marshal(event)
	for _, body := range [][]byte{append(raw, []byte(" {}")...), []byte(`{"delivery_id":"oversized","images":{"x":{"web":"` + strings.Repeat("x", 513<<10) + `"}}}`)} {
		r := httptest.NewRequest("POST", "/hooks/preview-images/p", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal("unbounded body accepted", w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/hooks/preview-images/p", bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json-invalid")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatal("wrong media type accepted", w.Code)
	}
	codes := make(chan int, 8)
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			request := httptest.NewRequest("POST", "/hooks/preview-images/p", bytes.NewReader(raw))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			f.handler.ServeHTTP(response, request)
			codes <- response.Code
		}()
	}
	group.Wait()
	close(codes)
	for code := range codes {
		if code != 202 {
			t.Fatal("concurrent exact retry rejected", code)
		}
	}
	var previews, deliveries int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM environment_previews),(SELECT count(*) FROM preview_ci_deliveries)`).Scan(&previews, &deliveries); err != nil || previews != 1 || deliveries != 1 {
		t.Fatal("concurrent retry duplicated durable work", previews, deliveries, err)
	}
}

func TestPreviewCIRefusesDuringManagedRegistryMaintenance(t *testing.T) {
	f, _, token, event := newCIPreviewFixture(t)
	id := f.resource("registry-fixture", workloadInput{Compose: "services:\n  app:\n    image: busybox:1.37\n"}, 0, sha("registry"))
	f.sql(`INSERT INTO managed_registries(id,team_id,hostname,credentials,retention,state) VALUES($1,'default','managed.example.test',$2,'{}','maintenance')`, id, []byte("injected-fixture-state"))
	ciSubmit(t, f, token, event, 409)
	var count int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM preview_ci_deliveries`).Scan(&count); err != nil || count != 0 {
		t.Fatal("refusal consumed a delivery", count, err)
	}
	f.sql(`UPDATE managed_registries SET state='removed',data_retained=false WHERE id=$1`, id)
	ciSubmit(t, f, token, event, 202)
}

func TestPreviewCIWaitsForPreparationAndIdenticalInputsKeepReady(t *testing.T) {
	f, _, token, event := newCIPreviewFixture(t)
	preview := jsonString(t, ciSubmit(t, f, token, event, 202), "preview_id")
	ciApprove(t, f, preview)
	if err := f.s.advancePreview(context.Background(), preview); err != nil {
		t.Fatal(err)
	}
	event.Sequence = 2
	event.Delivery = "during-prepare"
	event.Revision = sha("replacement")
	ciSubmit(t, f, token, event, 409)
	var sequence int64
	if err := f.s.DB.QueryRow(context.Background(), `SELECT sequence FROM preview_ci_slots WHERE project_id='p' AND suffix=$1`, event.Preview).Scan(&sequence); err != nil || sequence != 1 {
		t.Fatal("held delivery consumed sequence", sequence, err)
	}
	f.settle()
	event.Revision = sha("CI build")
	ciSubmit(t, f, token, event, 202)
	state, _, _ := f.previewState(preview)
	if state != "ready" {
		t.Fatal("identical inputs reopened approval", state)
	}
	f.sql(`UPDATE environment_previews SET last_activity_at=now()-interval '100 hours' WHERE id=$1`, preview)
	event.Sequence = 3
	event.Delivery = "fresh-activity"
	event.Revision = sha("fresh")
	ciSubmit(t, f, token, event, 202)
	if err := f.s.expirePreview(context.Background(), preview); err != nil {
		t.Fatal(err)
	}
	state, _, _ = f.previewState(preview)
	if state != "awaiting_approval" {
		t.Fatal("stale expiry removed fresh inputs", state)
	}
}

func TestPreviewCIReadyPollCannotClearNewApproval(t *testing.T) {
	f, base, token, event := newCIPreviewFixture(t)
	preview := jsonString(t, ciSubmit(t, f, token, event, 202), "preview_id")
	ciApprove(t, f, preview)
	f.settle()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	barrier, err := f.s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(context.Background())
	if _, err = barrier.Exec(ctx, `LOCK TABLE environment_preview_resources IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- f.s.advancePreview(ctx, preview) }()
	for {
		var blocked int
		if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%r.baseline_revision%'`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			break
		}
		select {
		case err = <-finished:
			t.Fatal("ready poll did not reach the controlled race", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	p, err := f.s.loadPreviewRow(ctx, barrier, preview, true)
	if err != nil {
		t.Fatal(err)
	}
	p.Snapshot.CI.Revision = sha("new approval")
	p.Snapshot.CI.Images[base]["web"] = "registry.example.test/team/app@" + imageID("new approval")
	p.Snapshot.CI.Identity = previewCIInputIdentity(p.Snapshot.CI.Revision, p.Snapshot.CI.Images)
	raw, err := f.s.sealJSON(p.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = barrier.Exec(ctx, `UPDATE environment_previews SET configuration=$2,state='awaiting_approval',stage='approval',last_activity_at=now() WHERE id=$1`, preview, raw); err != nil {
		t.Fatal(err)
	}
	if err = barrier.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	state, _, _ := f.previewState(preview)
	if state != "awaiting_approval" {
		t.Fatal("old ready poll bypassed new approval", state)
	}
}
