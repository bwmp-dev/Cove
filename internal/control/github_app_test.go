package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestGitHubAppSetupAndDeployments(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("gh-owner", "owner@example.com")
	editor := f.user("gh-editor", "editor@example.com")
	f.sql("INSERT INTO teams(id,name) VALUES('other','Other')")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','gh-owner','owner','[]'),('default','gh-editor','member','[{"scope":"team","role":"editor"}]')`)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	tokenRequests := 0
	f.s.OAuthClient = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		var result any
		switch r.URL.Path {
		case "/app-manifests/code/conversions":
			result = map[string]any{"id": 12, "slug": "cove-fixture", "pem": private, "webhook_secret": "fixture-webhook"}
		case "/app/installations":
			result = []map[string]any{{"id": 41, "account": map[string]string{"login": "personal"}}, {"id": 42, "account": map[string]string{"login": "organization"}}}
		case "/app/installations/41/access_tokens", "/app/installations/42/access_tokens":
			tokenRequests++
			var input struct {
				RepositoryIDs []int64           `json:"repository_ids"`
				Permissions   map[string]string `json:"permissions"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || input.Permissions["contents"] != "read" {
				t.Fatal("unsafe permissions")
			}
			if len(input.RepositoryIDs) > 0 && input.RepositoryIDs[0] != 99 {
				t.Fatal("wrong repository scope")
			}
			result = map[string]string{"token": "ephemeral-token"}
		case "/repositories/99":
			result = map[string]any{"id": 99, "full_name": "personal/tracker", "clone_url": "https://github.com/personal/tracker.git", "default_branch": "main"}
		case "/installation/repositories":
			result = map[string]any{"repositories": []map[string]any{{"id": 99, "full_name": "personal/tracker", "clone_url": "https://github.com/personal/tracker.git", "default_branch": "main"}}}
		default:
			t.Fatalf("unexpected GitHub request %s", r.URL)
		}
		b, _ := json.Marshal(result)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(b))}, nil
	})}
	f.call(owner, "default", "POST", "/api/github/app/setup", map[string]string{"name": "Cove"}, 403)
	start := f.call("root", "default", "POST", "/api/github/app/setup", map[string]string{"name": "Cove", "description": "Deploy", "organization": "example"}, 200)
	action, _ := url.Parse(jsonString(t, start, "action"))
	state := action.Query().Get("state")
	manifest := jsonString(t, start, "manifest")
	if !strings.Contains(manifest, `"public":true`) || !strings.Contains(manifest, `"contents":"read"`) || !strings.Contains(manifest, `"members":"read"`) {
		t.Fatal(manifest)
	}
	callback := func(state string, cookie *http.Cookie, status int) {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/github/app/callback?code=code&state="+state, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("callback %d %s", w.Code, w.Body)
		}
	}
	callback(state, nil, 400)
	callback("wrong", start.Result().Cookies()[0], 400)
	callback(state, start.Result().Cookies()[0], 303)
	callback(state, start.Result().Cookies()[0], 400)
	info := f.call("root", "default", "GET", "/api/github/app", nil, 200).Body.String()
	if strings.Contains(info, private) || strings.Contains(info, "fixture-webhook") {
		t.Fatal("credentials exposed")
	}
	f.call("root", "default", "POST", "/api/github/app/sync", map[string]any{}, 200)
	f.call("root", "default", "PUT", "/api/github/app/installations/41", map[string]any{"team_ids": []string{"default"}}, 200)
	f.call("root", "default", "PUT", "/api/github/app/installations/42", map[string]any{"team_ids": []string{"other"}}, 200)
	f.call(editor, "default", "GET", "/api/github/repositories", nil, 200)
	f.call(owner, "default", "GET", "/api/github/repositories", nil, 200)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('n','Node','host',22,'',$1,'ready')", []byte{})
	config, _ := seal(f.s.Key, []byte(`{"name":"Tracker","node_id":"n","kind":"service","compose":"services:\n  app:\n    image: busybox\n","environment":{"SECRET":"keep-me"}}`))
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('p','Tracker','default')")
	f.sql("INSERT INTO workloads(id,name,node_id,configuration,team_id,project_id) VALUES('w','Tracker','n',$1,'default','p')", config)
	input := map[string]any{"installation_id": 42, "repository_id": 99, "branch": "main", "compose_path": "compose.yaml", "automatic": true}
	f.call(owner, "default", "PUT", "/api/workloads/w/github", input, 403)
	input["installation_id"] = 41
	f.call(editor, "default", "PUT", "/api/workloads/w/github", input, 200)
	f.call(editor, "default", "DELETE", "/api/github/app/installations/41", nil, 409)
	f.call(owner, "default", "PUT", "/api/workloads/w/github", input, 200)
	f.call("root", "default", "PUT", "/api/github/app/installations/41", map[string]any{"team_ids": []string{"other"}}, 409)
	f.call("root", "default", "PUT", "/api/github/app/installations/41", map[string]any{"team_ids": []string{"default", "other"}}, 200)
	f.call("root", "other", "GET", "/api/github/repositories", nil, 200)
	f.call("root", "other", "DELETE", "/api/github/app/installations/41", nil, 200)
	f.call(owner, "default", "GET", "/api/github/repositories", nil, 200)
	var changedFiles []string
	send := func(branch, delivery, secret string, want int) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"ref": branch, "after": strings.Repeat("a", 40), "installation": map[string]int{"id": 41}, "repository": map[string]int{"id": 99}})
		if changedFiles != nil {
			var event map[string]any
			if err := json.Unmarshal(body, &event); err != nil {
				t.Fatal(err)
			}
			event["commits"] = []map[string]any{{"added": changedFiles, "modified": []string{}, "removed": []string{}}}
			body, _ = json.Marshal(event)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		r := httptest.NewRequest("POST", "/hooks/github-app", bytes.NewReader(body))
		r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		r.Header.Set("X-GitHub-Event", "push")
		r.Header.Set("X-GitHub-Delivery", delivery)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("webhook %d %s", w.Code, w.Body)
		}
	}
	var sourceConfig workloadInput
	var sourceEncrypted []byte
	if err = f.s.DB.QueryRow(context.Background(), "SELECT configuration FROM workloads WHERE id='w'").Scan(&sourceEncrypted); err != nil {
		t.Fatal(err)
	}
	sourceRaw, err := unseal(f.s.Key, sourceEncrypted)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(sourceRaw, &sourceConfig); err != nil {
		t.Fatal(err)
	}
	sourceConfig.Source.WatchPaths = []string{"src/**"}
	sourceConfig.Source.InlineCompose = true
	sourceConfig.Source.ComposePath = ""
	sourceConfig.Compose = "services:\n  app:\n    build: .\n"
	sourceRaw, err = json.Marshal(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	sourceEncrypted, err = seal(f.s.Key, sourceRaw)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("UPDATE workloads SET configuration=$1 WHERE id='w'", sourceEncrypted)
	f.call(owner, "default", "PUT", "/api/workloads/w/github", map[string]any{"installation_id": 41, "repository_id": 99, "branch": "main", "compose_path": "", "inline_compose": true, "automatic": true}, 200)
	changedFiles = []string{"README.md"}
	send("refs/heads/main", "unwatched", "fixture-webhook", 202)
	changedFiles = nil
	send("refs/heads/main", "bad", "wrong", 401)
	send("refs/heads/other", "other", "fixture-webhook", 202)
	send("refs/heads/main", "push-1", "fixture-webhook", 202)
	send("refs/heads/main", "push-1", "fixture-webhook", 202)
	var count int
	if err = f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM webhook_deliveries WHERE workload_id='w'").Scan(&count); err != nil || count != 1 {
		t.Fatal("delivery isolation/deduplication", count, err)
	}
	if err = f.s.dispatchWebhook(context.Background(), "w"); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err = f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE workload_id='w'").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	raw, err := unseal(f.s.Key, payload)
	if err != nil {
		t.Fatal(err)
	}
	var request worker.Request
	if err = json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	if request.Source.Token != "" || request.Source.Revision != strings.Repeat("a", 40) || request.Environment["SECRET"] != "keep-me" {
		t.Fatal("invalid queued deployment")
	}
	if !request.Source.InlineCompose || request.Compose != sourceConfig.Compose || request.Source.ComposePath != "" {
		t.Fatal("GitHub connection or webhook lost Cove-managed Compose")
	}
	before := tokenRequests
	if err = f.s.prepareAppDeployment(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if request.Source.Token != "ephemeral-token" || tokenRequests != before+1 {
		t.Fatal("token not minted")
	}
	if err = f.s.prepareAppDeployment(context.Background(), &request); err != nil || tokenRequests != before+1 {
		t.Fatal("retry changed worker input")
	}
	f.call(owner, "default", "DELETE", "/api/workloads/w/github", nil, 409)
	f.sql("UPDATE operations SET state='succeeded' WHERE workload_id='w'")
	send("refs/heads/main", "pending-before-stop", "fixture-webhook", 202)
	f.call(owner, "default", "POST", "/api/workloads/w/operations", map[string]string{"action": "stop"}, 202)
	f.call(owner, "default", "POST", "/api/workloads/w/operations", map[string]string{"action": "start"}, 409)
	f.sql("UPDATE operations SET state='succeeded' WHERE workload_id='w'")
	send("refs/heads/main", "push-while-stopped", "fixture-webhook", 202)
	if err = f.s.dispatchWebhook(context.Background(), "w"); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM webhook_deliveries WHERE workload_id='w' AND operation_id IS NULL").Scan(&count); err != nil || count != 0 {
		t.Fatal("manual stop retained automatic deployments", count, err)
	}
	if err = f.s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var stopped bool
	if err = f.s.DB.QueryRow(context.Background(), "SELECT manually_stopped FROM workloads WHERE id='w'").Scan(&stopped); err != nil || !stopped {
		t.Fatal("manual stop did not survive restart", err)
	}
	f.call(owner, "default", "POST", "/api/workloads/w/operations", map[string]string{"action": "start"}, 202)
	f.sql("UPDATE operations SET state='succeeded' WHERE workload_id='w'")
	send("refs/heads/main", "push-after-start", "fixture-webhook", 202)
	if err = f.s.dispatchWebhook(context.Background(), "w"); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM operations WHERE workload_id='w' AND state='queued' AND action='deploy'").Scan(&count); err != nil || count != 1 {
		t.Fatal("explicit start did not resume automatic deployments", count, err)
	}
	f.sql("UPDATE operations SET state='succeeded' WHERE workload_id='w'")
	f.call(owner, "default", "DELETE", "/api/workloads/w/github", nil, 200)
	send("refs/heads/main", "push-2", "fixture-webhook", 202)
	f.call(owner, "default", "PUT", "/api/workloads/w/github", input, 200)
	f.sql("INSERT INTO workloads(id,name,node_id,configuration,team_id,project_id) VALUES('migration','Migration','n',$1,'default','p')", config)
	f.call(owner, "default", "PUT", "/api/workloads/migration/github", input, 200)
	sequence := map[string]any{"workload_ids": []string{"migration", "w"}, "automatic": true}
	viewer := f.user("gh-viewer", "viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','gh-viewer','member','[{"scope":"team","role":"viewer"}]')`)
	f.call(viewer, "default", "PUT", "/api/projects/p/deployment-sequence", sequence, 403)
	f.call(owner, "default", "PUT", "/api/projects/p/deployment-sequence", map[string]any{"workload_ids": []string{"w", "w"}}, 400)
	f.call(owner, "default", "PUT", "/api/projects/p/deployment-sequence", sequence, 200)
	saved := f.call(owner, "default", "GET", "/api/projects/p/deployment-sequence", nil, 200)
	if !strings.Contains(saved.Body.String(), `"automatic":true`) {
		t.Fatal("sequence not saved")
	}
	f.sql("UPDATE workloads SET manually_stopped=true WHERE id='migration'")
	send("refs/heads/main", "sequence-stopped", "fixture-webhook", 202)
	if err = f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM webhook_deliveries WHERE delivery='sequence-stopped'").Scan(&count); err != nil || count != 0 {
		t.Fatal("stopped sequence ran partially", err, count)
	}
	f.sql("UPDATE workloads SET manually_stopped=false WHERE id='migration'")
	send("refs/heads/main", "sequence-run", "fixture-webhook", 202)
	send("refs/heads/main", "sequence-run", "fixture-webhook", 202)
	if err = f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM webhook_deliveries WHERE delivery='sequence-run'").Scan(&count); err != nil || count != 2 {
		t.Fatal("sequence duplicate or missing", err, count)
	}
	if err = f.s.dispatchWebhook(context.Background(), "w"); err != nil {
		t.Fatal(err)
	}
	var dependent *string
	if err = f.s.DB.QueryRow(context.Background(), "SELECT operation_id FROM webhook_deliveries WHERE workload_id='w' AND delivery='sequence-run'").Scan(&dependent); err != nil || dependent != nil {
		t.Fatal("dependent queued before migration", err)
	}
	if err = f.s.dispatchWebhook(context.Background(), "migration"); err != nil {
		t.Fatal(err)
	}
	if err = f.s.dispatchWebhook(context.Background(), "w"); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow(context.Background(), "SELECT operation_id FROM webhook_deliveries WHERE workload_id='w' AND delivery='sequence-run'").Scan(&dependent); err != nil || dependent == nil {
		t.Fatal("dependent missing", err)
	}
	if ready, err := f.s.dependencyReady(context.Background(), *dependent); err != nil || ready {
		t.Fatal("dependent ran before migration succeeded", err)
	}
	f.sql("UPDATE operations SET state='failed' WHERE workload_id='migration'")
	if ready, err := f.s.dependencyReady(context.Background(), *dependent); err != nil || ready {
		t.Fatal("failed migration released application", err)
	}
	var finalState string
	if err = f.s.DB.QueryRow(context.Background(), "SELECT state,payload FROM operations WHERE id=$1", *dependent).Scan(&finalState, &payload); err != nil || finalState != "cancelled" {
		t.Fatal("dependent not cancelled", err)
	}
	raw, err = unseal(f.s.Key, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &request); err != nil || request.Source.Revision != strings.Repeat("a", 40) {
		t.Fatal("sequence lost pushed revision", err)
	}
	send("refs/heads/main", "sequence-abandoned", "fixture-webhook", 202)
	f.call(owner, "default", "POST", "/api/workloads/migration/operations", map[string]string{"action": "stop"}, 202)
	f.sql("UPDATE operations SET state='succeeded' WHERE workload_id='migration' AND state='queued'")
	if err = f.s.dispatchWebhook(context.Background(), "w"); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM webhook_deliveries WHERE workload_id='w' AND delivery='sequence-abandoned'").Scan(&count); err != nil || count != 0 {
		t.Fatal("abandoned sequence remained pending", err, count)
	}
	if err = f.s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved = f.call(owner, "default", "GET", "/api/projects/p/deployment-sequence", nil, 200)
	if !strings.Contains(saved.Body.String(), `"automatic":true`) {
		t.Fatal("sequence did not survive restart")
	}

	f.call("root", "default", "PUT", "/api/github/app/installations/41", map[string]any{"team_ids": []string{"default", "other"}}, 200)
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('other-project','Other','other')")
	f.sql("INSERT INTO workloads(id,name,node_id,configuration,team_id,project_id) VALUES('other-workload','Other','n',$1,'other','other-project')", config)
	f.call("root", "other", "PUT", "/api/workloads/other-workload/github", input, 200)
	f.sql("UPDATE workloads SET manually_stopped=false WHERE id='migration'")
	send("refs/heads/main", "shared-installation", "fixture-webhook", 202)
	if err = f.s.DB.QueryRow(context.Background(), "SELECT count(DISTINCT w.team_id) FROM webhook_deliveries d JOIN workloads w ON w.id=d.workload_id WHERE d.delivery='shared-installation'").Scan(&count); err != nil || count != 2 {
		t.Fatal("shared installation did not deploy to both teams", count, err)
	}
	f.call("root", "other", "DELETE", "/api/github/app/installations/41", nil, 409)
	request.Workload = "other-workload"
	request.Source.Token = ""
	if err = f.s.prepareAppDeployment(context.Background(), &request); err != nil || request.Source.Token != "ephemeral-token" {
		t.Fatal("shared team deployment authentication failed", err)
	}

}
