package control

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

type githubDirectoryAPI struct {
	mu       sync.Mutex
	members  []githubAccount
	failPage int
	status   int
	pages    []int
	scope    string
	token    string
}

func newGithubDirectoryAPI(t *testing.T, f *accountFixture) *githubDirectoryAPI {
	t.Helper()
	state := &githubDirectoryAPI{members: []githubAccount{}, scope: "read:user,user:email,read:org", token: "fixture-directory-token"}
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/oauth/access_token":
			state.mu.Lock()
			defer state.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"access_token": state.token, "token_type": "bearer", "scope": state.scope})
		case "/user":
			json.NewEncoder(w).Encode(map[string]any{"id": 100, "login": "directory-owner"})
		case "/user/emails":
			json.NewEncoder(w).Encode([]map[string]any{{"email": "owner@example.invalid", "verified": true, "primary": true}})
		case "/app/installations/41":
			json.NewEncoder(w).Encode(map[string]any{"id": 41, "account": map[string]any{"id": 900, "login": "AcMe"}})
		case "/app/installations/41/access_tokens":
			var input struct {
				Permissions map[string]string `json:"permissions"`
			}
			if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&input) != nil || len(input.Permissions) != 1 || input.Permissions["members"] != "read" {
				http.Error(w, "unexpected token scope", 400)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"token": "fixture-directory-token"})
		case "/orgs/acme/teams/engineering/members":
			if r.Header.Get("Authorization") != "Bearer fixture-directory-token" || r.URL.Query().Get("per_page") != "100" {
				http.Error(w, "fixture authorization rejected", 401)
				return
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			state.mu.Lock()
			defer state.mu.Unlock()
			state.pages = append(state.pages, page)
			if page == state.failPage {
				w.WriteHeader(state.status)
				w.Write([]byte(`{"message":"fixture provider failure"}`))
				return
			}
			start := (page - 1) * 100
			if start < 0 {
				http.Error(w, "invalid fixture page", 400)
				return
			}
			end := min(start+100, len(state.members))
			if start > len(state.members) {
				start = len(state.members)
			}
			json.NewEncoder(w).Encode(state.members[start:end])
		default:
			http.Error(w, "unexpected fixture route", 404)
		}
	}))
	t.Cleanup(fixture.Close)
	target, _ := url.Parse(fixture.URL)
	transport := fixture.Client().Transport
	f.s.OAuthClient = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.github.com" && r.URL.Host != "github.com" {
			return nil, fmt.Errorf("unexpected provider host")
		}
		copy := r.Clone(r.Context())
		address := *r.URL
		address.Scheme, address.Host = target.Scheme, target.Host
		copy.URL = &address
		return transport.RoundTrip(copy)
	})}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	credentials, _ := json.Marshal(githubAppCredentials{PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))})
	sealed, err := seal(f.s.Key, credentials)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("INSERT INTO github_app(id,app_id,slug,credentials) VALUES(1,12,'directory-fixture',$1)", sealed)
	f.sql("INSERT INTO github_installations(id,account) VALUES(41,'acme')")
	return state
}

func (s *githubDirectoryAPI) set(members []githubAccount, page, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.members = append([]githubAccount{}, members...)
	s.failPage, s.status = page, status
}

func TestGithubTeamSyncCompleteRosterPreviewAndFailure(t *testing.T) {
	f := newAccountFixture(t)
	viewer := f.user("member", "member@example.com")
	f.user("second", "second@example.com")
	f.sql("UPDATE users SET github_id=CASE id WHEN 'member' THEN '101' WHEN 'second' THEN '202' END WHERE id IN ('member','second')")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','member','member','[{"scope":"team","scope_id":"default","role":"viewer"}]')`)
	provider := newGithubDirectoryAPI(t, f)
	members := []githubAccount{}
	for id := int64(101); id <= 200; id++ {
		members = append(members, githubAccount{ID: id, Login: fmt.Sprintf("fixture-%d", id)})
	}
	members = append(members, githubAccount{ID: 202, Login: "second"})
	provider.set(members, 0, 0)
	body := map[string]any{"name": "Engineering", "mode": "app", "account": "acme", "installation_id": 41, "enabled": true, "mappings": []directoryMapping{{Group: "engineering", Team: "default", Role: "editor"}}}
	f.call(viewer, "default", "POST", "/api/settings/github-team-sync", body, 403)
	id := jsonString(t, f.call("root", "", "POST", "/api/settings/github-team-sync", body, 200), "id")
	path := "/api/settings/github-team-sync/" + id
	preview := func() githubDirectoryReport {
		t.Helper()
		w := f.call("root", "", "POST", path+"/preview", map[string]any{"version": 1}, 200)
		var report githubDirectoryReport
		if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		return report
	}
	first := preview()
	if first.Applied || first.Unmatched != 99 || len(first.Changes) != 2 || len(first.Hash) != 64 {
		t.Fatal("incorrect preview", first)
	}
	for _, change := range first.Changes {
		if change.Name == "" || change.Email == "" {
			t.Fatal("preview lacks reviewable user identity")
		}
	}
	var present bool
	if err := f.s.DB.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM team_members WHERE user_id='second')").Scan(&present); err != nil || present {
		t.Fatal("preview mutated membership", err)
	}
	f.call("root", "", "POST", path+"/reconcile", map[string]any{"version": 1, "roster_hash": first.Hash, "confirm": true}, 200)
	for _, status := range []int{401, 403, 404, 429, 500} {
		provider.set(members[:100], 2, status)
		f.call("root", "", "POST", path+"/reconcile", map[string]any{"version": 1, "roster_hash": first.Hash, "confirm": true}, 502)
		if err := f.s.DB.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM team_members WHERE user_id='second')").Scan(&present); err != nil || !present {
			t.Fatal("partial/failed roster removed access", status, err)
		}
	}
	provider.set(members[:100], 0, 0)
	f.call("root", "", "POST", path+"/reconcile", map[string]any{"version": 1, "roster_hash": first.Hash, "confirm": true}, 409)
	fresh := preview()
	f.call("root", "", "POST", path+"/reconcile", map[string]any{"version": 1, "roster_hash": fresh.Hash, "confirm": true}, 200)
	if err := f.s.DB.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM team_members WHERE user_id='second')").Scan(&present); err != nil || present {
		t.Fatal("complete successful roster did not remove managed access", err)
	}
	f.call("root", "default", "DELETE", "/api/teams/default/members/member", nil, 409)
	f.call("root", "", "DELETE", path, map[string]any{"version": 1, "confirm": true}, 200)
	var grants []grant
	if err := f.s.DB.QueryRow(t.Context(), "SELECT grants FROM team_members WHERE user_id='member'").Scan(&grants); err != nil || len(grants) != 1 || grants[0].Role != "viewer" {
		t.Fatal("removing sync erased manual access", grants, err)
	}
}

func TestGithubTeamOAuthSealedConsentAndSignInRefresh(t *testing.T) {
	f := newAccountFixture(t)
	provider := newGithubDirectoryAPI(t, f)
	f.sql("UPDATE users SET github_id='100',email='owner@example.invalid',email_verified=true WHERE id='bootstrap'")
	f.user("member", "member@example.com")
	f.sql("UPDATE users SET github_id='101' WHERE id='member'")
	f.s.GithubClientID, f.s.GithubClientSecret = "fixture-client", "fixture-oauth-secret"
	id := jsonString(t, f.call("root", "", "POST", "/api/settings/github-team-sync", map[string]any{"name": "OAuth org", "mode": "oauth", "account": "acme", "enabled": true, "mappings": []directoryMapping{{Group: "engineering", Team: "default", Role: "operator"}}}, 200), "id")
	path := "/api/settings/github-team-sync/" + id
	f.call("root", "", "POST", path+"/preview", map[string]any{"version": 1}, 502)
	start := f.call("", "", "POST", "/auth/github/start", map[string]any{}, 200)
	address, _ := url.Parse(jsonString(t, start, "url"))
	if strings.Contains(address.Query().Get("scope"), "read:org") {
		t.Fatal("ordinary sign-in requested organization access")
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("directory-owner-password"), bcrypt.MinCost)
	f.sql("UPDATE users SET password_hash=$1 WHERE id='bootstrap'", hash)
	session := randomToken()
	f.sql("INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,'bootstrap',now()+interval '1 hour')", tokenHash(session))
	start = f.call(session, "", "POST", "/auth/github/start", map[string]any{"link": true, "current_password": "directory-owner-password"}, 200)
	address, _ = url.Parse(jsonString(t, start, "url"))
	if !strings.Contains(address.Query().Get("scope"), "read:org") {
		t.Fatal("organization consent was not requested")
	}
	callback := func(start *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		t.Helper()
		address, _ := url.Parse(jsonString(t, start, "url"))
		request := httptest.NewRequest("GET", "/auth/github/callback?state="+url.QueryEscape(address.Query().Get("state"))+"&code=fixture", nil)
		for _, cookie := range start.Result().Cookies() {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		if response.Code != 303 || strings.Contains(response.Header().Get("Location"), "error=") {
			t.Fatal("directory OAuth callback failed", response.Code, response.Header().Get("Location"))
		}
		return response
	}
	connected := callback(start)
	if connected.Header().Get("Location") != "/admin?github_team_sync=connected#github-team-sync" {
		t.Fatal("directory consent did not return to settings")
	}
	for _, cookie := range connected.Result().Cookies() {
		if cookie.Name == "__Host-cove" {
			t.Fatal("consent replaced the verified account session")
		}
	}
	var sealed []byte
	if err := f.s.DB.QueryRow(t.Context(), "SELECT configuration FROM github_team_sync_sources WHERE id=$1", id).Scan(&sealed); err != nil || bytes.Contains(sealed, []byte("fixture-directory-token")) {
		t.Fatal("OAuth token was stored in plaintext", err)
	}
	listing := f.call("root", "", "GET", "/api/settings/github-team-sync", nil, 200).Body.String()
	if strings.Contains(listing, "fixture-directory-token") || strings.Contains(listing, "oauth_token") || !strings.Contains(listing, `"credential_configured":true`) {
		t.Fatal("credential response was not redacted")
	}
	provider.mu.Lock()
	provider.scope = "read:user,user:email"
	provider.token = "fixture-limited-token"
	provider.mu.Unlock()
	missing := callback(f.call(session, "", "POST", "/auth/github/start", map[string]any{"link": true, "current_password": "directory-owner-password"}, 200))
	if missing.Header().Get("Location") != "/admin?github_team_sync=permission_required#github-team-sync" {
		t.Fatal("missing organization scope was reported as success")
	}
	callback(f.call("", "", "POST", "/auth/github/start", map[string]any{}, 200))
	tx, err := f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	source, err := loadGithubTeamSource(t.Context(), tx, f.s.Key, id)
	tx.Rollback(t.Context())
	if err != nil || source.Version != 2 || source.Config.OAuthToken != "fixture-directory-token" {
		t.Fatal("ordinary sign-in replaced the org grant", err)
	}
	expiring := f.call(session, "", "POST", "/auth/github/start", map[string]any{"link": true, "current_password": "directory-owner-password"}, 200)
	f.sql("DELETE FROM sessions WHERE token_hash=$1", tokenHash(session))
	expired := callback(expiring)
	if expired.Header().Get("Location") != "/admin?github_team_sync=error#github-team-sync" {
		t.Fatal("revoked consent session did not fail back to settings")
	}
	provider.set([]githubAccount{{ID: 101, Login: "renamed-member"}}, 0, 0)
	f.sql("UPDATE github_team_sync_sources SET next_check_at=now()-interval '1 minute' WHERE id=$1", id)
	f.s.githubTeamSignIn(t.Context(), "member")
	var due, member bool
	if err := f.s.DB.QueryRow(t.Context(), "SELECT next_check_at<=now() FROM github_team_sync_sources WHERE id=$1", id).Scan(&due); err != nil || !due {
		t.Fatal("individual sign-ins postponed full reconciliation", err)
	}
	if err := f.s.DB.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM team_members WHERE user_id='member')").Scan(&member); err != nil || !member {
		t.Fatal("sign-in did not refresh linked GitHub ID membership", err)
	}
	f.sql("UPDATE users SET disabled=true WHERE id='bootstrap'")
	f.call("root", "", "POST", path+"/preview", map[string]any{"version": 2}, 502)
	if err := f.s.DB.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM team_members WHERE user_id='member')").Scan(&member); err != nil || !member {
		t.Fatal("unavailable OAuth owner removed access", err)
	}
}

func TestGithubTeamConsentRequiresRetainedSourceAndCurrentOwner(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("UPDATE users SET github_id='100' WHERE id='bootstrap'")
	if stored, err := f.s.storeGithubTeamOAuth(t.Context(), "bootstrap", "100", "synthetic-token"); err != nil || stored {
		t.Fatal("consent without a surviving source reported connected", err)
	}
	f.call("root", "", "POST", "/api/settings/github-team-sync", map[string]any{"name": "Consent fixture", "mode": "oauth", "account": "acme", "enabled": false, "mappings": []directoryMapping{}}, 200)
	if stored, err := f.s.storeGithubTeamOAuth(t.Context(), "bootstrap", "100", "synthetic-token"); err != nil || !stored {
		t.Fatal("valid organization grant not retained", err)
	}
	f.sql("UPDATE users SET is_admin=false WHERE id='bootstrap'")
	if stored, err := f.s.storeGithubTeamOAuth(t.Context(), "bootstrap", "100", "synthetic-replacement"); err != nil || stored {
		t.Fatal("demoted owner replaced the grant", err)
	}
}
