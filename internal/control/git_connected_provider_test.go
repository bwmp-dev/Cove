package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"cove/internal/worker"
)

func TestConnectedGitProviders(t *testing.T) {
	for _, kind := range []string{"gitlab", "gitea", "bitbucket"} {
		t.Run(kind, func(t *testing.T) {
			f := newAccountFixture(t)
			ctx := context.Background()
			host, base, account := "provider.example.com", "https://provider.example.com", "fixture"
			if kind == "bitbucket" {
				host, base, account = "bitbucket.org", "https://api.bitbucket.org", "{fixture-user}"
			}
			clone := "https://" + host + "/team/app.git"
			revision := strings.Repeat("a", 40)
			statusCalls, commentCreates, commentUpdates := 0, 0, 0
			commentBody := ""
			denyStatus := false
			f.s.OAuthClient = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer fixture-access-token" {
					t.Fatal("missing provider credential")
				}
				path := r.URL.Path
				var result any = map[string]any{}
				status := 200
				switch {
				case strings.HasSuffix(path, "/user"):
					result = map[string]string{"username": account, "login": account, "uuid": account}
				case strings.HasSuffix(path, "/projects"):
					result = []any{map[string]any{"id": 42, "path_with_namespace": "team/app", "http_url_to_repo": clone, "default_branch": "main"}}
				case strings.HasSuffix(path, "/user/repos"):
					if r.URL.Query().Get("page") != "1" {
						result = []any{}
						break
					}
					result = []any{map[string]any{"id": 42, "full_name": "team/app", "clone_url": clone, "default_branch": "main"}}
				case strings.HasSuffix(path, "/repositories/team"):
					result = map[string]any{"values": []any{map[string]any{"uuid": "{repo}", "full_name": "team/app", "mainbranch": map[string]string{"name": "main"}, "links": map[string]any{"clone": []any{map[string]string{"name": "https", "href": "https://fixture@bitbucket.org/team/app.git"}}}}}}
				case strings.Contains(path, "/tags/"):
					result = map[string]any{"name": "v1", "commit": map[string]string{"id": revision, "sha": revision}, "target": map[string]string{"hash": revision}}
				case strings.HasSuffix(path, "/branches/main"):
					result = map[string]any{"name": "main", "commit": map[string]string{"id": revision}, "target": map[string]string{"hash": revision}}
				case strings.HasSuffix(path, "/branches"):
					if kind == "gitea" && r.URL.Query().Get("page") != "1" {
						result = []any{}
						break
					}
					branches := []any{map[string]string{"name": "main"}, map[string]string{"name": "feature/slash"}}
					result = branches
					if kind == "bitbucket" {
						result = map[string]any{"values": branches}
					}
				case strings.Contains(path, "/statuses/"):
					statusCalls++
					if denyStatus {
						status = 403
					}
				case strings.HasSuffix(path, "/notes") || strings.HasSuffix(path, "/comments"):
					if r.Method == "GET" {
						comments := []any{}
						if commentBody != "" {
							comments = append(comments, map[string]any{"id": 81, "body": commentBody, "content": map[string]string{"raw": commentBody}, "author": map[string]string{"username": account}, "user": map[string]string{"login": account, "uuid": account}})
						}
						result = comments
						if kind == "bitbucket" {
							result = map[string]any{"values": comments}
						}
					} else {
						var input struct {
							Body    string `json:"body"`
							Content struct {
								Raw string `json:"raw"`
							} `json:"content"`
						}
						if json.NewDecoder(r.Body).Decode(&input) != nil {
							t.Fatal("invalid comment")
						}
						commentBody = input.Body
						if kind == "bitbucket" {
							commentBody = input.Content.Raw
						}
						commentCreates++
						result = map[string]int{"id": 81}
					}
				case strings.HasSuffix(path, "/81") && (r.Method == "PATCH" || r.Method == "PUT"):
					var input struct {
						Body    string `json:"body"`
						Content struct {
							Raw string `json:"raw"`
						} `json:"content"`
					}
					if json.NewDecoder(r.Body).Decode(&input) != nil {
						t.Fatal("invalid update")
					}
					commentBody = input.Body
					if kind == "bitbucket" {
						commentBody = input.Content.Raw
					}
					commentUpdates++
					result = map[string]int{"id": 81}
				default:
					t.Fatalf("unexpected provider request %s %s", r.Method, r.URL)
				}
				raw, _ := json.Marshal(result)
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
			})}
			created := f.call("root", "default", "POST", "/api/git/connections", map[string]string{"name": "Fixture", "kind": kind, "url": base, "workspace": "team", "token": "fixture-access-token"}, 201)
			var connection struct {
				ID     int64  `json:"id"`
				Secret string `json:"webhook_secret"`
			}
			if json.Unmarshal(created.Body.Bytes(), &connection) != nil {
				t.Fatal("invalid connection response")
			}
			listed := f.call("root", "default", "GET", "/api/git/connections", nil, 200).Body.String()
			if strings.Contains(listed, connection.Secret) || strings.Contains(listed, "fixture-access-token") {
				t.Fatal("connection credentials exposed")
			}
			repositoriesPath := fmt.Sprintf("/api/git/connections/%d/repositories", connection.ID)
			repositories := f.call("root", "default", "GET", repositoriesPath, nil, 200)
			var repos []appRepository
			if json.Unmarshal(repositories.Body.Bytes(), &repos) != nil || len(repos) != 1 || repos[0].CloneURL != clone {
				t.Fatalf("repositories %s", repositories.Body)
			}
			f.call("root", "default", "GET", fmt.Sprintf("%s/%d/branches", repositoriesPath, repos[0].ID), nil, 200)
			provider, err := f.s.connectedGitProvider(ctx, connection.ID)
			if err != nil {
				t.Fatal(err)
			}
			head, err := provider.BranchHead(ctx, connection.ID, repos[0].ID, "main")
			if err != nil || head != revision {
				t.Fatalf("head %s %v", head, err)
			}
			f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('n','Node','localhost',22,'',$1,'ready','{"previews":true}')`, []byte{})
			f.sql(`INSERT INTO projects(id,name,team_id) VALUES('p','Project','default')`)
			config := workloadInput{Name: "App", NodeID: "n", ProjectID: "p", Source: &worker.GitSource{Repository: clone, Branch: "main", ComposePath: "compose.yaml"}}
			workload := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
			sourcePath := "/api/workloads/" + workload + "/git-provider"
			f.call("root", "default", "PUT", sourcePath, map[string]any{"connection_id": connection.ID, "repository_id": repos[0].ID, "branch": "main", "compose_path": "compose.yaml", "automatic": true, "reporting": true}, 200)
			f.call("root", "default", "PUT", "/api/workloads/"+workload+"/previews", previewPolicy{Enabled: true, Hours: 24, Domain: "preview.example.com", Service: "app", Port: 80}, 200)
			push := func(sha string) any {
				switch kind {
				case "gitlab":
					return map[string]any{"project": map[string]int{"id": 42}, "ref": "refs/heads/main", "after": sha}
				case "gitea":
					return map[string]any{"repository": map[string]int{"id": 42}, "ref": "refs/heads/main", "after": sha}
				default:
					return map[string]any{"repository": map[string]string{"uuid": "{repo}"}, "push": map[string]any{"changes": []any{map[string]any{"new": map[string]any{"type": "branch", "name": "main", "target": map[string]string{"hash": sha}}}}}}
				}
			}
			send := func(event, delivery string, payload any, valid bool, want int) {
				t.Helper()
				body, _ := json.Marshal(payload)
				request := httptest.NewRequest("POST", fmt.Sprintf("/hooks/git/%d", connection.ID), bytes.NewReader(body))
				mac := hmac.New(sha256.New, []byte(connection.Secret))
				switch kind {
				case "gitlab":
					key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(connection.Secret, "whsec_"))
					if err != nil {
						t.Fatal(err)
					}
					timestamp := strconv.FormatInt(time.Now().Unix(), 10)
					mac = hmac.New(sha256.New, key)
					mac.Write([]byte(delivery + "." + timestamp + "."))
					mac.Write(body)
					request.Header.Set("webhook-id", delivery)
					request.Header.Set("webhook-timestamp", timestamp)
					request.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
					request.Header.Set("X-Gitlab-Event", event)
				case "gitea":
					mac.Write(body)
					request.Header.Set("X-Gitea-Signature", hex.EncodeToString(mac.Sum(nil)))
					request.Header.Set("X-Gitea-Delivery", delivery)
					request.Header.Set("X-Gitea-Event", event)
				case "bitbucket":
					mac.Write(body)
					request.Header.Set("X-Hub-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
					request.Header.Set("X-Request-UUID", delivery)
					request.Header.Set("X-Event-Key", event)
				}
				if !valid {
					request.Header.Del("webhook-signature")
					request.Header.Del("X-Gitea-Signature")
					request.Header.Del("X-Hub-Signature")
				}
				response := httptest.NewRecorder()
				f.handler.ServeHTTP(response, request)
				if response.Code != want {
					t.Fatalf("%s webhook %d want %d: %s", kind, response.Code, want, response.Body)
				}
			}
			pushEvent := map[string]string{"gitlab": "Push Hook", "gitea": "push", "bitbucket": "repo:push"}[kind]
			send(pushEvent, "invalid", push(revision), false, 401)
			send(pushEvent, "push-1", push(revision), true, 202)
			send(pushEvent, "push-1", push(revision), true, 202)
			send(pushEvent, "push-2", push(strings.Repeat("b", 40)), true, 202)
			if err = f.s.dispatchWebhook(ctx, workload); err != nil {
				t.Fatal(err)
			}
			var payload []byte
			var count int
			if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE workload_id=$1`, workload).Scan(&count); err != nil || count != 2 {
				t.Fatalf("dedup count %d %v", count, err)
			}
			if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE workload_id=$1 AND superseded_by IS NOT NULL`, workload).Scan(&count); err != nil || count != 1 {
				t.Fatalf("supersede count %d %v", count, err)
			}
			if err = f.s.DB.QueryRow(ctx, `SELECT payload FROM operations WHERE workload_id=$1`, workload).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var request worker.Request
			if err = f.s.unsealJSON(payload, &request); err != nil {
				t.Fatal(err)
			}
			if request.Source.Revision != strings.Repeat("b", 40) || request.Source.Token != "" {
				t.Fatal("webhook commit not pinned without credentials")
			}
			if err = f.s.prepareAppDeployment(ctx, &request); err != nil {
				t.Fatal(err)
			}
			if request.Source.Token != "fixture-access-token" {
				t.Fatal("clone credential unavailable")
			}
			f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id=$1`, workload)
			denyStatus = true
			if err = f.s.dispatchConnectedGitFeedback(ctx); err != nil {
				t.Fatal(err)
			}
			var message string
			if err = f.s.DB.QueryRow(ctx, `SELECT error FROM git_provider_feedback WHERE operation_id=$1`, request.ID).Scan(&message); err != nil || message == "" || strings.Contains(message, "fixture-access-token") {
				t.Fatalf("status failure not reported safely %q %v", message, err)
			}
			denyStatus = false
			f.sql(`UPDATE git_provider_feedback SET next_attempt_at=now()`)
			if err = f.s.dispatchConnectedGitFeedback(ctx); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			var pr any
			switch kind {
			case "gitea":
				event := previewEvent{Action: "opened", Number: 7}
				event.Repository.ID = 42
				event.PullRequest.Head.Repo.ID = 42
				event.PullRequest.Base.Repo.ID = 42
				event.PullRequest.Head.Ref = "feature/slash"
				event.PullRequest.Base.Ref = "main"
				event.PullRequest.Head.SHA = revision
				event.PullRequest.State = "open"
				event.PullRequest.UpdatedAt = now
				pr = event
			case "gitlab":
				pr = map[string]any{"project": map[string]int{"id": 42}, "object_attributes": map[string]any{"iid": 7, "action": "open", "state": "opened", "source_project_id": 42, "target_project_id": 42, "source_branch": "feature/slash", "target_branch": "main", "updated_at": now.Format(time.RFC3339Nano), "last_commit": map[string]string{"id": revision}}}
			case "bitbucket":
				pr = map[string]any{"repository": map[string]string{"uuid": "{repo}"}, "pullrequest": map[string]any{"id": 7, "state": "OPEN", "updated_on": now, "source": map[string]any{"repository": map[string]string{"uuid": "{repo}"}, "branch": map[string]string{"name": "feature/slash"}, "commit": map[string]string{"hash": revision}}, "destination": map[string]any{"repository": map[string]string{"uuid": "{repo}"}, "branch": map[string]string{"name": "main"}}}}
			}
			prEvent := map[string]string{"gitlab": "Merge Request Hook", "gitea": "pull_request", "bitbucket": "pullrequest:created"}[kind]
			send(prEvent, "pr-1", pr, true, 200)
			send(prEvent, "pr-1", pr, true, 200)
			if kind == "gitea" {
				event := pr.(previewEvent)
				event.Action = "synchronized"
				event.PullRequest.UpdatedAt = now.Add(time.Second)
				event.PullRequest.Head.SHA = strings.Repeat("b", 40)
				send("pull_request_sync", "pr-sync", event, true, 200)
				var pinned string
				if err = f.s.DB.QueryRow(ctx, `SELECT revision FROM previews WHERE source_id=$1 AND number=7`, workload).Scan(&pinned); err != nil || pinned != strings.Repeat("b", 40) {
					t.Fatalf("Gitea synchronized preview not updated: %s %v", pinned, err)
				}
			}

			if err = f.s.reconcilePreviews(ctx); err != nil {
				t.Fatal(err)
			}
			if err = f.s.dispatchConnectedGitFeedback(ctx); err != nil {
				t.Fatal(err)
			}
			if commentCreates != 1 || !strings.Contains(commentBody, "https://pr-7-") {
				t.Fatalf("preview comment missing: %d %s", commentCreates, commentBody)
			}
			f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id IN (SELECT workload_id FROM previews WHERE source_id=$1)`, workload)
			if err = f.s.dispatchConnectedGitFeedback(ctx); err != nil {
				t.Fatal(err)
			}
			if commentUpdates != 1 || !strings.Contains(commentBody, "**succeeded**") || statusCalls < 3 {
				t.Fatal("terminal preview status/comment not updated")
			}
			f.call("root", "default", "PUT", "/api/workloads/"+workload+"/git-trigger", gitSourceTrigger{Kind: "tags", Pattern: "v*"}, 200)
			tagPush := push(revision)
			switch kind {
			case "gitlab", "gitea":
				tagPush.(map[string]any)["ref"] = "refs/tags/v1"
			case "bitbucket":
				tagPush.(map[string]any)["push"] = map[string]any{"changes": []any{map[string]any{"new": map[string]any{"type": "tag", "name": "v1", "target": map[string]string{"hash": revision}}}}}
			}
			send(pushEvent, "tag-1", tagPush, true, 202)
			if err = f.s.dispatchWebhook(ctx, workload); err != nil {
				t.Fatal(err)
			}
			var branch, tagRevision string
			if err = f.s.DB.QueryRow(ctx, `SELECT payload FROM operations WHERE workload_id=$1 AND state='queued'`, workload).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			if err = f.s.unsealJSON(payload, &request); err != nil {
				t.Fatal(err)
			}
			branch, tagRevision = request.Source.Branch, request.Source.Revision
			if branch != "v1" || tagRevision != revision {
				t.Fatalf("tag deploy not pinned: %s %s", branch, tagRevision)
			}
			f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id=$1`, workload)
			if kind == "bitbucket" {
				f.call("root", "default", "PUT", "/api/workloads/"+workload+"/git-trigger", gitSourceTrigger{Kind: "releases", Pattern: "v*"}, 409)
			} else {
				f.call("root", "default", "PUT", "/api/workloads/"+workload+"/git-trigger", gitSourceTrigger{Kind: "releases", Pattern: "v*"}, 200)
				var release any
				releaseEvent := "release"
				if kind == "gitlab" {
					releaseEvent = "Release Hook"
					release = map[string]any{"action": "create", "tag": "v1", "project": map[string]int{"id": 42}}
				} else {
					release = map[string]any{"action": "published", "repository": map[string]int{"id": 42}, "release": map[string]any{"tag_name": "v1", "draft": false}}
				}
				send(releaseEvent, "release-1", release, true, 202)
			}
			f.call("root", "default", "DELETE", fmt.Sprintf("/api/git/connections/%d", connection.ID), nil, 409)
			f.sql(`INSERT INTO teams(id,name) VALUES('other','Other')`)
			f.call("root", "other", "GET", repositoriesPath, nil, 404)
		})
	}
}
