package control

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTeamGitHubConnection(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("owner", "owner@example.com")
	member := f.user("member", "member@example.com")
	f.sql(`INSERT INTO teams(id,name) VALUES('other','Other')`)
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','owner','owner','[]'),('default','member','member','[]')`)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(githubAppCredentials{ClientID: "Iv.fixture", ClientSecret: strings.Repeat("s", 40), PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))})
	encrypted, err := seal(f.s.Key, raw)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO github_app(id,app_id,slug,credentials) VALUES(1,12,'fixture',$1)`, encrypted)
	orgRole := "admin"
	visible := true
	suspended := false
	revoke := false
	f.s.OAuthClient = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		var result any
		installation := map[string]any{"id": 41, "account": map[string]any{"id": 7, "login": "alice"}}
		if suspended {
			installation["suspended_at"] = "2026-01-01T00:00:00Z"
		}
		switch r.URL.Path {
		case "/login/oauth/access_token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("code_verifier") == "" || r.Form.Get("client_id") != "Iv.fixture" {
				t.Fatal("missing PKCE or app client")
			}
			if revoke {
				f.sql(`DELETE FROM sessions WHERE token_hash=$1`, tokenHash(owner))
			}
			result = map[string]string{"access_token": "private-user-token", "token_type": "bearer"}
		case "/app":
			result = map[string]string{"client_id": "Iv.fixture"}
		case "/user":
			result = map[string]any{"id": 7, "login": "alice"}
		case "/user/memberships/orgs":
			result = []any{map[string]any{"state": "active", "role": orgRole, "organization": map[string]any{"id": 8, "login": "org"}}}
		case "/users/alice/installation":
			result = installation
		case "/orgs/org/installation":
			result = map[string]any{"id": 42, "account": map[string]any{"id": 8, "login": "org"}}
		case "/user/installations":
			items := []any{}
			if visible {
				items = []any{installation, map[string]any{"id": 42, "account": map[string]any{"id": 8, "login": "org"}}}
			}
			result = map[string]any{"installations": items}
		default:
			t.Fatalf("unexpected provider call %s", r.URL.Path)
		}
		raw, _ := json.Marshal(result)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})}
	start := func(session, account string) (string, *http.Cookie) {
		w := f.call(session, "default", "POST", "/api/github/app/connect", map[string]string{"account": account}, 200)
		u, err := url.Parse(jsonString(t, w, "url"))
		if err != nil {
			t.Fatal(err)
		}
		if u.Host != "github.com" || u.Query().Get("code_challenge_method") != "S256" {
			t.Fatal("invalid authorization URL")
		}
		return u.Query().Get("state"), w.Result().Cookies()[0]
	}
	callback := func(session, state string, cookie *http.Cookie, want string) {
		r := httptest.NewRequest("GET", "/api/github/app/connect/callback?state="+url.QueryEscape(state)+"&code=fixture", nil)
		r.AddCookie(cookie)
		if session != "" {
			r.AddCookie(&http.Cookie{Name: "__Host-cove", Value: session})
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != 303 || w.Header().Get("Location") != "/integrations?github="+want {
			t.Fatalf("callback: %d %s", w.Code, w.Header().Get("Location"))
		}
	}
	for _, path := range []string{"/api/github/repositories"} {
		f.call(member, "default", "GET", path, nil, 403)
	}
	f.call(member, "default", "POST", "/api/github/app/connect", map[string]string{"account": "alice"}, 200)
	f.sql(`UPDATE team_members SET grants='[{"scope":"project","scope_id":"p","role":"editor"}]' WHERE user_id='member'`)
	f.call(member, "default", "POST", "/api/github/app/connect", map[string]string{"account": "alice"}, 200)
	f.sql(`UPDATE team_members SET grants='[{"scope":"team","role":"editor"}]' WHERE user_id='member'`)
	f.call(member, "default", "GET", "/api/github/app", nil, 200)
	for _, path := range []string{"/api/github/app/setup", "/api/github/app/sync"} {
		f.call(member, "default", "POST", path, nil, 403)
	}
	f.call(member, "default", "PUT", "/api/github/app/user-auth", nil, 403)
	f.call(member, "default", "PUT", "/api/github/app/installations/41", map[string]any{"team_ids": []string{"default"}}, 403)
	f.call("root", "default", "PUT", "/api/github/app/user-auth", map[string]string{"client_id": "wrong", "client_secret": strings.Repeat("s", 40)}, 400)
	f.call("root", "default", "PUT", "/api/github/app/user-auth", map[string]string{"client_id": "Iv.fixture", "client_secret": strings.Repeat("s", 40)}, 200)
	state, cookie := start(member, "alice")
	wrong := *cookie
	wrong.Value = "wrong"
	callback(member, state, &wrong, "expired")
	callback("", state, cookie, "connected")
	callback(member, state, cookie, "expired")
	var shares int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM github_installation_teams WHERE installation_id=41").Scan(&shares); err != nil || shares != 0 {
		t.Fatal("personal authorization implicitly shared repositories", err, shares)
	}

	f.sql(`INSERT INTO github_installations(id,account) VALUES(99,'hidden'),(100,'unassigned')`)
	f.sql(`INSERT INTO github_installation_teams VALUES(99,'other')`)
	f.call(member, "default", "PUT", "/api/github/app/installations/41", map[string]any{"team_ids": []string{"default"}}, 200)
	info := f.call(member, "default", "GET", "/api/github/app", nil, 200).Body.String()
	if strings.Contains(info, "hidden") || strings.Contains(info, "unassigned") || strings.Contains(info, "private-user-token") || strings.Contains(info, strings.Repeat("s", 40)) {
		t.Fatal("cross-team or secret disclosure")
	}
	f.call(member, "default", "DELETE", "/api/github/app/installations/99", nil, 404)
	f.call(member, "default", "DELETE", "/api/github/app/installations/41", nil, 200)
	state, cookie = start(owner, "org")
	orgRole = "member"
	callback(owner, state, cookie, "access_denied")
	orgRole = "admin"
	state, cookie = start(owner, "org")
	callback("", state, cookie, "connected")
	f.sql(`INSERT INTO github_installation_teams VALUES(42,'other')`)
	state, cookie = start(owner, "org")
	callback(owner, state, cookie, "connected")
	visible = false
	state, cookie = start(owner, "alice")
	callback(owner, state, cookie, "access_denied")
	visible = true
	suspended = true
	state, cookie = start(owner, "alice")
	callback(owner, state, cookie, "access_denied")
	suspended = false
	state, cookie = start(member, "alice")
	f.sql(`UPDATE team_members SET grants='[]' WHERE user_id='member'`)
	callback(member, state, cookie, "connected")
	state, cookie = start(owner, "alice")
	callback(member, state, cookie, "expired")
	state, cookie = start(owner, "alice")
	f.sql(`UPDATE github_installation_states SET expires_at=now()-interval '1 minute'`)
	callback(owner, state, cookie, "expired")
	state, cookie = start(owner, "alice")
	revoke = true
	callback("", state, cookie, "access_denied")
}
