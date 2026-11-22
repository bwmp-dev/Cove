package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPanelComposeCannotEnterPreviewExecution(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('n','Node','localhost',22,'',$1,'ready','{"previews":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('p','Project','default')`)
	config := workloadInput{Name: "App", NodeID: "n", ProjectID: "p", Source: &worker.GitSource{Repository: "https://github.com/example/app.git", Branch: "main", ComposePath: "compose.yaml"}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	f.sql(`INSERT INTO github_installations(id,account) VALUES(91,'example')`)
	f.sql(`INSERT INTO github_installation_teams VALUES(91,'default')`)
	f.sql(`INSERT INTO github_sources(workload_id,installation_id,repository_id,repository) VALUES($1,91,92,'https://github.com/example/app.git')`, id)
	path := "/api/workloads/" + id
	policy := previewPolicy{Enabled: true, Hours: 24}
	f.call("root", "default", "PUT", path+"/previews", policy, 200)
	config.ProjectID = ""
	config.Source.InlineCompose = true
	config.Source.ComposePath = ""
	config.Compose = "services:\n  app:\n    build: .\n"
	f.call("root", "default", "PUT", path, config, 409)
	policy.Enabled = false
	f.call("root", "default", "PUT", path+"/previews", policy, 200)
	f.call("root", "default", "PUT", path, config, 200)
	policy.Enabled = true
	f.call("root", "default", "PUT", path+"/previews", policy, 409)
}

func TestPreviewLifecycleIsolationAndManualStop(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('preview-node','Node','localhost',22,'',$1,'ready','{"previews":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('preview-project','Project','default')`)
	config := workloadInput{Name: "Production", NodeID: "preview-node", ProjectID: "preview-project", Source: &worker.GitSource{Repository: "https://github.com/example/app.git", Branch: "main", ComposePath: "compose.yaml"}, Environment: map[string]string{"SECRET": "production-value"}, Routes: []worker.Route{{Domain: "production.example.com", Service: "app", Port: 80}}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	f.sql(`INSERT INTO github_installations(id,account) VALUES(91,'example')`)
	f.sql(`INSERT INTO github_installation_teams VALUES(91,'default')`)
	f.sql(`INSERT INTO github_sources(workload_id,installation_id,repository_id,repository) VALUES($1,91,92,'https://github.com/example/app.git')`, id)
	credentials, err := f.s.sealPreview(githubAppCredentials{WebhookSecret: "preview-webhook-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO github_app(id,app_id,slug,credentials) VALUES(1,90,'fixture',$1)`, credentials)
	invalid := httptest.NewRequest("POST", "/hooks/github-app", strings.NewReader(`{}`))
	invalid.Header.Set("X-GitHub-Event", "pull_request")
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, invalid)
	if response.Code != 401 {
		t.Fatal("unsigned preview event accepted", response.Code)
	}
	path := "/api/workloads/" + id + "/previews"
	editor := f.user("preview-editor", "preview-editor@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','preview-editor','member','[{"scope":"team","role":"viewer"}]')`)
	policy := previewPolicy{Enabled: true, Hours: 24, Environment: map[string]string{"PREVIEW": "separate-value"}}
	f.call(editor, "default", "PUT", path, policy, 403)
	f.call("root", "default", "PUT", path, policy, 200)
	if strings.Contains(f.call("root", "default", "GET", path, nil, 200).Body.String(), "separate-value") {
		t.Fatal("preview secret exposed")
	}
	event := previewEvent{Action: "opened", Number: 7}
	event.PullRequest.UpdatedAt = time.Now().UTC()
	event.Installation.ID = 91
	event.Repository.ID = 92
	event.PullRequest.State = "open"
	event.PullRequest.Head.Repo.ID = 93
	event.PullRequest.Base.Repo.ID = 92
	event.PullRequest.Base.Ref = "main"
	event.PullRequest.Head.Ref = "feature"
	event.PullRequest.Head.SHA = strings.Repeat("a", 40)
	send := func(delivery string) {
		t.Helper()
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, []byte("preview-webhook-fixture"))
		mac.Write(raw)
		req := httptest.NewRequest("POST", "/hooks/github-app", bytes.NewReader(raw))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, req)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	count := func(query string, want int) {
		t.Helper()
		var got int
		if err := f.s.DB.QueryRow(ctx, query, id).Scan(&got); err != nil || got != want {
			t.Fatal(query, got, err)
		}
	}
	send("fork")
	count(`SELECT count(*) FROM previews WHERE source_id=$1`, 0)
	event.PullRequest.Head.Repo.ID = 92
	send("trusted")
	send("trusted")
	count(`SELECT count(*) FROM previews WHERE source_id=$1`, 1)
	var target, environment string
	var encrypted []byte
	if err := f.s.DB.QueryRow(ctx, `SELECT p.workload_id,p.environment_id,w.configuration FROM previews p JOIN workloads w ON w.id=p.workload_id WHERE source_id=$1`, id).Scan(&target, &environment, &encrypted); err != nil {
		t.Fatal(err)
	}
	raw, err := unseal(f.s.Key, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	var preview workloadInput
	if err = json.Unmarshal(raw, &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Routes) != 0 || preview.Environment["SECRET"] != "" || preview.Environment["PREVIEW"] != "separate-value" || preview.Source.Revision != event.PullRequest.Head.SHA || environment == "" {
		t.Fatal("production configuration leaked into preview")
	}
	f.call("root", "default", "PUT", "/api/workloads/"+target, config, 409)
	if err = f.s.reconcilePreviews(ctx); err != nil {
		t.Fatal(err)
	}
	var operation string
	if err = f.s.DB.QueryRow(ctx, `SELECT id,payload FROM operations WHERE workload_id=$1`, target).Scan(&operation, &encrypted); err != nil {
		t.Fatal(err)
	}
	raw, err = unseal(f.s.Key, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	var request worker.Request
	if err = json.Unmarshal(raw, &request); err != nil || !request.Preview {
		t.Fatal("preview isolation flag missing", err)
	}
	f.sql(`UPDATE operations SET state='succeeded' WHERE id=$1`, operation)
	f.sql(`UPDATE workloads SET manually_stopped=true WHERE id=$1`, target)
	event.Action = "synchronize"
	event.PullRequest.UpdatedAt = event.PullRequest.UpdatedAt.Add(time.Second)
	event.PullRequest.Head.SHA = strings.Repeat("b", 40)
	send("new-push")
	if err = f.s.reconcilePreviews(ctx); err != nil {
		t.Fatal(err)
	}
	var active int
	if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM operations WHERE workload_id=$1 AND state='queued'`, target).Scan(&active); err != nil || active != 0 {
		t.Fatal("manual stop ignored", err)
	}
	f.call("root", "default", "POST", path, map[string]int{"number": 7}, 202)
	if err = f.s.reconcilePreviews(ctx); err != nil {
		t.Fatal(err)
	}
	var activeID string
	if err = f.s.DB.QueryRow(ctx, `SELECT id FROM operations WHERE workload_id=$1 AND state='queued'`, target).Scan(&activeID); err != nil {
		t.Fatal("explicit preview resume did not queue deployment", err)
	}
	f.call("root", "default", "POST", path, map[string]int{"number": 7}, 409)
	f.sql(`UPDATE previews SET expires_at=now()-interval '1 second' WHERE workload_id=$1`, target)
	if err = f.s.reconcilePreviews(ctx); err != nil {
		t.Fatal(err)
	}
	var cancelled bool
	if err = f.s.DB.QueryRow(ctx, `SELECT cancel_requested FROM operations WHERE id=$1`, activeID).Scan(&cancelled); err != nil || !cancelled {
		t.Fatal("expiry did not cancel active deployment", err)
	}
	f.sql(`UPDATE operations SET state='cancelled' WHERE id=$1`, activeID)
	if err = f.s.reconcilePreviews(ctx); err != nil {
		t.Fatal(err)
	}
	var cleanup string
	if err = f.s.DB.QueryRow(ctx, `SELECT cleanup_operation FROM previews WHERE workload_id=$1`, target).Scan(&cleanup); err != nil || cleanup == "" {
		t.Fatal("expiry did not queue cleanup", err)
	}
	count(`SELECT count(*) FROM previews WHERE source_id=$1`, 1)
	f.sql(`UPDATE operations SET state='succeeded' WHERE id=$1`, cleanup)
	if err = f.s.reconcilePreviews(ctx); err != nil {
		t.Fatal(err)
	}
	count(`SELECT count(*) FROM previews WHERE source_id=$1`, 0)
	count(`SELECT count(*) FROM workloads WHERE id=$1`, 1)
	send("stale-open-after-cleanup")
	count(`SELECT count(*) FROM previews WHERE source_id=$1`, 0)
}
