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
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGitProviderWebhookVerification(t *testing.T) {
	provider := (&Server{}).githubProvider()
	body, secret := []byte(`{"ref":"refs/heads/main"}`), []byte("test-only-webhook-secret")
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	signed := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	for _, test := range []struct {
		name, signature string
		body            []byte
		valid           bool
	}{
		{"valid", signed, body, true},
		{"changed body", signed, []byte(`{"ref":"refs/heads/other"}`), false},
		{"missing", "", body, false},
		{"wrong algorithm", strings.Replace(signed, "sha256=", "sha1=", 1), body, false},
		{"malformed", "sha256=not-hex", body, false},
		{"truncated", signed[:20], body, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := http.Header{}
			headers.Set("X-Hub-Signature-256", test.signature)
			if got := provider.VerifyWebhook(secret, test.body, headers); got != test.valid {
				t.Fatalf("verification = %v", got)
			}
		})
	}
	if _, err := provider.PullRequest([]byte(`{`)); err == nil {
		t.Fatal("accepted malformed pull request")
	}
	event, err := provider.PullRequest([]byte(`{"action":"opened","number":7,"installation":{"id":41},"repository":{"id":99}}`))
	if err != nil || event.Action != "opened" || event.Number != 7 || event.Installation.ID != 41 || event.Repository.ID != 99 {
		t.Fatalf("pull request: %+v, %v", event, err)
	}
}

func TestGitProviderGitHubRepositoryAndBranchContract(t *testing.T) {
	f := newAccountFixture(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := f.s.sealPreview(githubAppCredentials{PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))})
	if err != nil {
		t.Fatal(err)
	}
	f.sql("INSERT INTO github_app(id,app_id,slug,credentials) VALUES(1,12,'fixture',$1)", credentials)
	f.sql("INSERT INTO github_installations(id,account) VALUES(41,'fixture')")
	f.sql("INSERT INTO github_installation_teams(installation_id,team_id) VALUES(41,'default')")
	tokenStatus, listingStatus, statusResponse := 200, 200, 201
	statusCalls := 0
	statusPermissions := false
	pages := []string{}
	tokenScopes := []int64{}
	status, branch, commit := 200, "feature/slash", strings.Repeat("a", 40)
	f.s.OAuthClient = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		var result any
		responseStatus := 200
		switch r.URL.Path {
		case "/app/installations/41/access_tokens":
			responseStatus = tokenStatus
			var input struct {
				Permissions  map[string]string `json:"permissions"`
				Repositories []int64           `json:"repository_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if len(input.Permissions) == 1 && input.Permissions["statuses"] == "write" {
				statusPermissions = true
			} else if len(input.Permissions) != 1 || input.Permissions["contents"] != "read" {
				t.Fatalf("unsafe token permissions: %v", input.Permissions)
			}
			scope := int64(0)
			if len(input.Repositories) != 0 {
				if len(input.Repositories) != 1 {
					t.Fatal("token not repository scoped")
				}
				scope = input.Repositories[0]
			}
			tokenScopes = append(tokenScopes, scope)
			result = map[string]string{"token": "fixture-token"}
		case "/installation/repositories":
			responseStatus = listingStatus
			if r.Header.Get("Authorization") != "Bearer fixture-token" {
				t.Fatal("missing installation token")
			}
			page := r.URL.Query().Get("page")
			pages = append(pages, page)
			repos := []appRepository{}
			if page == "1" {
				for i := 1; i <= 100; i++ {
					repos = append(repos, appRepository{ID: int64(i)})
				}
			} else if page == "2" {
				repos = append(repos, appRepository{ID: 101})
			} else {
				t.Fatalf("unexpected page %s", page)
			}
			result = map[string]any{"repositories": repos}
		case "/repositories/99/git/ref/heads/feature/slash":
			responseStatus = status
			result = map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"sha": commit}}
		case "/repositories/99/git/matching-refs/heads/feature/":
			refs := []gitBranch{}
			for i := 0; i < 201; i++ {
				refs = append(refs, gitBranch{Ref: fmt.Sprintf("refs/heads/feature/%d", i)})
			}
			result = refs
		case "/repositories/99/statuses/" + strings.Repeat("a", 40):
			statusCalls++
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer fixture-token" {
				t.Fatal("invalid status request")
			}
			var input gitCommitStatus
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input.State != "success" || input.Context != "cove/fixture" || input.TargetURL != "https://cove.example.com/operations/fixture" {
				t.Fatalf("status payload: %+v", input)
			}
			responseStatus, result = statusResponse, map[string]int{"id": 1}
		default:
			t.Fatalf("unexpected provider request %s", r.URL)
		}
		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: responseStatus, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})}
	ctx := context.Background()
	provider := f.s.githubProvider()
	repos, err := provider.Repositories(ctx, 41)
	if err != nil || len(repos) != 101 || len(pages) != 2 {
		t.Fatalf("repositories %d, pages %v, error %v", len(repos), pages, err)
	}
	for _, repo := range repos {
		if repo.InstallationID != 41 {
			t.Fatal("lost installation identity")
		}
	}
	head, err := provider.BranchHead(ctx, 41, 99, "feature/slash")
	if err != nil || head != commit {
		t.Fatalf("head %q, %v", head, err)
	}
	status = 404
	head, err = provider.BranchHead(ctx, 41, 99, "feature/slash")
	if err != nil || head != "" {
		t.Fatalf("missing head %q, %v", head, err)
	}
	status, branch = 200, "wrong-branch"
	if _, err = provider.BranchHead(ctx, 41, 99, "feature/slash"); err == nil {
		t.Fatal("accepted mismatched branch")
	}
	branch, commit = "feature/slash", "not-a-commit"
	if _, err = provider.BranchHead(ctx, 41, 99, "feature/slash"); err == nil {
		t.Fatal("accepted invalid revision")
	}
	status = 403
	if _, err = provider.BranchHead(ctx, 41, 99, "feature/slash"); err == nil {
		t.Fatal("suppressed denied repository access")
	}
	refs, err := provider.Branches(ctx, 41, 99, "feature/")
	if err != nil || len(refs) != 200 {
		t.Fatalf("branches %d, %v", len(refs), err)
	}
	tokenStatus = 403
	denied := f.call("root", "default", "GET", "/api/github/repositories", nil, 502)
	if strings.TrimSpace(denied.Body.String()) != "GitHub installation is unavailable; check its permissions or suspension" {
		t.Fatalf("changed installation error: %s", denied.Body)
	}
	tokenStatus, listingStatus = 200, 503
	unavailable := f.call("root", "default", "GET", "/api/github/repositories", nil, 502)
	if strings.TrimSpace(unavailable.Body.String()) != "could not list GitHub repositories" {
		t.Fatalf("changed listing error: %s", unavailable.Body)
	}
	for i, scope := range tokenScopes {
		if i == 0 {
			if scope != 0 {
				t.Fatal("repository listing narrowed")
			}
		} else if scope != 99 && i < len(tokenScopes)-2 {
			t.Fatal("branch credential not repository scoped")
		}
	}
	tokenStatus = 200
	feedback := gitCommitStatus{State: "success", Context: "cove/fixture", TargetURL: "https://cove.example.com/operations/fixture"}
	if err := provider.CommitStatus(ctx, 41, 99, strings.Repeat("a", 40), feedback); err != nil {
		t.Fatal(err)
	}
	if !statusPermissions || statusCalls != 1 {
		t.Fatal("status was not posted with scoped write permission")
	}
	statusResponse = 403
	if err := provider.CommitStatus(ctx, 41, 99, strings.Repeat("a", 40), feedback); err == nil {
		t.Fatal("suppressed status permission denial")
	}
	for _, invalid := range []gitCommitStatus{
		{State: "running", Context: "cove/fixture"},
		{State: "success"},
		{State: "success", Context: "cove/fixture", TargetURL: "http://cove.example.com/"},
		{State: "success", Context: "cove/fixture", TargetURL: "https://secret@example.com/"},
		{State: "success", Context: "cove/fixture", Description: strings.Repeat("x", 141)},
	} {
		if err := provider.CommitStatus(ctx, 41, 99, strings.Repeat("a", 40), invalid); err == nil {
			t.Fatalf("accepted invalid status: %+v", invalid)
		}
	}
	if err := provider.CommitStatus(ctx, 41, 99, strings.Repeat("0", 40), feedback); err == nil {
		t.Fatal("accepted null revision")
	}
	if statusCalls != 2 {
		t.Fatal("invalid status reached provider")
	}

}
