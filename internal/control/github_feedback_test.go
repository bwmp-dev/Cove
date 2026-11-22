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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestGithubDeploymentFeedback(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := f.s.sealPreview(githubAppCredentials{WebhookSecret: "fixture-webhook", PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO github_app(id,app_id,slug,credentials) VALUES(1,12,'fixture',$1)`, credentials)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('n','Node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('p','Project','default')`)
	f.sql(`INSERT INTO environments(id,project_id,team_id,name) VALUES('preview-env','p','default','Preview')`)
	f.sql(`INSERT INTO github_installations(id,account) VALUES(41,'example')`)
	f.sql(`INSERT INTO github_installation_teams VALUES(41,'default')`)
	config := workloadInput{Name: "App", NodeID: "n", ProjectID: "p", Source: &worker.GitSource{Repository: "https://github.com/example/app.git", Branch: "main", ComposePath: "compose.yaml"}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	f.sql(`INSERT INTO github_sources(workload_id,installation_id,repository_id,repository) VALUES($1,41,99,'https://github.com/example/app.git')`, id)
	path := "/api/workloads/" + id + "/github/reporting"
	viewer := f.user("viewer", "viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','viewer','member','[{"scope":"team","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", path, nil, 200)
	f.call(viewer, "default", "PUT", path, map[string]bool{"enabled": true}, 403)
	apiCalls, creates, commentCreates, patches, statuses := 0, 0, 0, 0, 0
	deploymentID, commentID := int64(0), int64(0)
	deploymentPayload := map[string]string{}
	commentBody := ""
	denyToken, loseDeployment, loseComment := false, true, true
	f.s.OAuthClient = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		apiCalls++
		code := 200
		var output any = map[string]any{}
		switch {
		case r.URL.Path == "/app/installations/41/access_tokens":
			var input struct {
				Permissions  map[string]string `json:"permissions"`
				Repositories []int64           `json:"repository_ids"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || len(input.Repositories) != 1 || input.Repositories[0] != 99 {
				t.Fatal("unsafe repository token scope")
			}
			for key, value := range input.Permissions {
				if key == "contents" && value == "read" {
					continue
				}
				if value != "write" || (key != "statuses" && key != "deployments" && key != "pull_requests") {
					t.Fatalf("unsafe feedback permission %s=%s", key, value)
				}
			}
			if denyToken {
				code = 403
			}
			output = map[string]string{"token": "fixture-write-token"}
		case strings.HasPrefix(r.URL.Path, "/repositories/99/commits/"):
			output = map[string]string{"sha": strings.Repeat("a", 40)}
		case strings.HasPrefix(r.URL.Path, "/repositories/99/statuses/"):
			var input gitCommitStatus
			if json.NewDecoder(r.Body).Decode(&input) != nil {
				t.Fatal("invalid status")
			}
			if !strings.HasPrefix(input.Context, "cove/") || strings.Contains(input.Description, "fixture-write-token") {
				t.Fatal("invalid status payload")
			}
			statuses++
		case r.URL.Path == "/repos/example/app/deployments" && r.Method == "GET":
			if deploymentID == 0 {
				output = []any{map[string]any{"id": 999, "payload": map[string]string{"cove_operation": "op"}, "creator": map[string]string{"login": "attacker"}}}
			} else {
				output = []any{map[string]any{"id": deploymentID, "payload": deploymentPayload, "creator": map[string]string{"login": "fixture[bot]"}}}
			}
		case r.URL.Path == "/repos/example/app/deployments" && r.Method == "POST":
			var input struct {
				Ref       string            `json:"ref"`
				AutoMerge bool              `json:"auto_merge"`
				Contexts  []string          `json:"required_contexts"`
				Payload   map[string]string `json:"payload"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || input.AutoMerge || input.Contexts == nil || len(input.Contexts) != 0 || input.Ref != strings.Repeat("a", 40) {
				t.Fatal("deployment did not use immutable revision without merge/check side effects")
			}
			creates++
			deploymentID = 71
			deploymentPayload = input.Payload
			if loseDeployment {
				loseDeployment = false
				return nil, errors.New("fixture lost response")
			}
			output = map[string]int64{"id": deploymentID}
		case r.URL.Path == "/repos/example/app/deployments/71/statuses":
			var input map[string]any
			if json.NewDecoder(r.Body).Decode(&input) != nil || input["auto_inactive"] != false {
				t.Fatal("unsafe deployment status")
			}
		case r.URL.Path == "/repos/example/app/issues/7/comments" && r.Method == "GET":
			output = []any{map[string]any{"id": 999, "body": "<!-- cove-preview:preview -->\nSpoofed", "user": map[string]string{"login": "attacker"}}}
			if commentID != 0 {
				output = []any{map[string]any{"id": commentID, "body": commentBody, "user": map[string]string{"login": "fixture[bot]"}}}
			}
		case r.URL.Path == "/repos/example/app/issues/7/comments" && r.Method == "POST":
			var input map[string]string
			if json.NewDecoder(r.Body).Decode(&input) != nil {
				t.Fatal("invalid comment")
			}
			commentBody = input["body"]
			commentID = 81
			commentCreates++
			if loseComment {
				loseComment = false
				return nil, errors.New("fixture lost comment response")
			}
			output = map[string]int64{"id": commentID}
		case r.URL.Path == "/repos/example/app/issues/comments/81" && r.Method == "PATCH":
			var input map[string]string
			if json.NewDecoder(r.Body).Decode(&input) != nil {
				t.Fatal("invalid comment update")
			}
			commentBody = input["body"]
			patches++
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		raw, _ := json.Marshal(output)
		return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})}
	denyToken = true
	f.call("root", "default", "PUT", path, map[string]bool{"enabled": true}, 502)
	denyToken = false
	f.call("root", "default", "PUT", path, map[string]bool{"enabled": true}, 200)
	// A preview inherits its parent's permission-checked opt-in.
	encrypted, err := f.s.sealPreview(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,kind,team_id,project_id,environment_id) VALUES('preview','n','Preview',$1,'service','default','p','preview-env')`, encrypted)
	f.sql(`INSERT INTO github_sources(workload_id,installation_id,repository_id,repository) VALUES('preview',41,99,'https://github.com/example/app.git')`)
	f.sql(`INSERT INTO previews(source_id,number,workload_id,environment_id,revision,expires_at) VALUES($1,7,'preview','preview-env',$2,now()+interval '1 day')`, id, strings.Repeat("a", 40))
	request := worker.Request{ID: "op", Workload: "preview", Action: "deploy", Source: &worker.GitSource{Repository: config.Source.Repository, Branch: "feature", ComposePath: "compose.yaml", Revision: strings.Repeat("a", 40)}, Routes: []worker.Route{{Domain: "pr.example.com", Service: "app", Port: 80}}}
	payload, err := f.s.sealPreview(request)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES('op','preview','deploy',$1)`, payload)
	dispatch := func() {
		t.Helper()
		if err := f.s.dispatchGithubFeedback(ctx); err != nil {
			t.Fatal(err)
		}
	}
	dispatch()
	var message string
	var attempts int
	if err = f.s.DB.QueryRow(ctx, `SELECT error,attempts FROM github_operation_feedback WHERE operation_id='op'`).Scan(&message, &attempts); err != nil || attempts != 1 || strings.Contains(message, "fixture-write-token") {
		t.Fatalf("retry receipt %d %q %v", attempts, message, err)
	}
	before := apiCalls
	dispatch()
	if apiCalls != before {
		t.Fatal("ignored backoff")
	}
	f.sql(`UPDATE github_operation_feedback SET next_attempt_at=now()`)
	dispatch()
	f.sql(`UPDATE github_operation_feedback SET next_attempt_at=now()`)
	dispatch()
	if creates != 1 || commentCreates != 1 || patches != 1 {
		t.Fatalf("duplicate remote creates: %d deployments %d comments %d updates", creates, commentCreates, patches)
	}
	before = apiCalls
	dispatch()
	if apiCalls != before {
		t.Fatal("repeated already reported state")
	}
	f.sql(`UPDATE operations SET state='succeeded' WHERE id='op'`)
	dispatch()
	if !strings.Contains(commentBody, "**succeeded**") || !strings.Contains(commentBody, "https://pr.example.com") || patches != 2 || statuses < 2 {
		t.Fatalf("missing terminal preview feedback: %s", commentBody)
	}
	f.sql(`UPDATE operations SET state='failed' WHERE id='op'`)
	f.sql(`DELETE FROM github_installation_teams WHERE installation_id=41`)
	before = apiCalls
	dispatch()
	if apiCalls != before {
		t.Fatal("reporting ignored sharing revocation")
	}
	f.sql(`INSERT INTO github_installation_teams VALUES(41,'default')`)
	f.call("root", "default", "PUT", path, map[string]bool{"enabled": false}, 200)
	before = apiCalls
	dispatch()
	if apiCalls != before {
		t.Fatal("reporting continued after disablement")
	}
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/git-trigger", gitSourceTrigger{Kind: "releases", Pattern: "v*"}, 200)
	release := map[string]any{"action": "published", "installation": map[string]int{"id": 41}, "repository": map[string]int{"id": 99}, "release": map[string]any{"tag_name": "v1", "draft": false}}
	sendRelease := func(delivery string, want int) {
		t.Helper()
		raw, _ := json.Marshal(release)
		mac := hmac.New(sha256.New, []byte("fixture-webhook"))
		mac.Write(raw)
		request := httptest.NewRequest("POST", "/hooks/github-app", bytes.NewReader(raw))
		request.Header.Set("X-GitHub-Event", "release")
		request.Header.Set("X-GitHub-Delivery", delivery)
		request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("release response %d: %s", response.Code, response.Body)
		}
	}
	sendRelease("release-1", 202)
	sendRelease("release-1", 202)
	var count int
	if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE workload_id=$1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("release dedup %d %v", count, err)
	}
	if err = f.s.dispatchWebhook(ctx, id); err != nil {
		t.Fatal(err)
	}
	var queued []byte
	if err = f.s.DB.QueryRow(ctx, `SELECT payload FROM operations WHERE workload_id=$1 AND state='queued'`, id).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	var deployment worker.Request
	if err = f.s.unsealJSON(queued, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Source.Revision != strings.Repeat("a", 40) || deployment.Source.Branch != "v1" {
		t.Fatal("release not pinned to resolved tag")
	}
	release["release"].(map[string]any)["draft"] = true
	sendRelease("draft-release", 200)

}
