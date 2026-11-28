package control

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type oauthTransport func(*http.Request) (*http.Response, error)

func (f oauthTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubOAuthIntegration(t *testing.T) {
	f := newAccountFixture(t)
	f.s.GithubClientID = "fixture-client"
	f.s.GithubClientSecret = randomToken()
	email := "github@example.com"
	verified := true
	githubID := int64(100)
	challenge := ""
	requests := 0
	f.s.OAuthClient = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		var body any
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if challenge != base64.RawURLEncoding.EncodeToString(hash[:]) || r.Form.Get("client_secret") != f.s.GithubClientSecret || r.Form.Get("redirect_uri") != f.s.PublicURL+"/auth/github/callback" {
				t.Fatal("invalid PKCE exchange")
			}
			body = map[string]string{"access_token": "fixture-provider-access-token", "token_type": "bearer"}
		case "https://api.github.com/user":
			body = map[string]any{"id": githubID, "login": "octocat"}
		case "https://api.github.com/user/emails":
			body = []map[string]any{{"email": email, "verified": verified, "primary": true}}
		default:
			t.Fatalf("unexpected provider request %s", r.URL)
		}
		if r.URL.Host == "api.github.com" && r.Header.Get("Authorization") != "Bearer fixture-provider-access-token" {
			t.Fatal("missing provider auth")
		}
		data, _ := json.Marshal(body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data))), Header: http.Header{}}, nil
	})}
	start := func(user string, input any) (string, *http.Cookie) {
		t.Helper()
		w := f.call(user, "", "POST", "/auth/github/start", input, 200)
		u, err := url.Parse(jsonString(t, w, "url"))
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		challenge = q.Get("code_challenge")
		if q.Get("code_challenge_method") != "S256" || q.Get("scope") != "read:user user:email" {
			t.Fatal("unsafe authorization request")
		}
		return q.Get("state"), w.Result().Cookies()[0]
	}
	callback := func(state string, cookie *http.Cookie, want string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", "/auth/github/callback?state="+url.QueryEscape(state)+"&code=fixture-code", nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != 303 || w.Header().Get("Location") != want {
			t.Fatalf("callback: %d %s", w.Code, w.Header().Get("Location"))
		}
		return w
	}
	state, cookie := start("", map[string]any{})
	callback(state, nil, "/login?error=oauth_failed")
	if requests != 0 {
		t.Fatal("provider called without browser binding")
	}
	callback(state, &http.Cookie{Name: cookie.Name, Value: randomToken()}, "/login?error=oauth_failed")
	if requests != 0 {
		t.Fatal("provider called with forged browser binding")
	}
	state, cookie = start("", map[string]any{})
	callback(state, cookie, "/login?error=registration_closed")
	f.call("root", "", "PUT", "/api/settings", map[string]bool{"registration_enabled": true}, 200)
	state, cookie = start("", map[string]any{})
	verified = false
	callback(state, cookie, "/login?error=oauth_failed")
	verified = true
	state, cookie = start("", map[string]any{})
	w := callback(state, cookie, "/")
	var session string
	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-cove" {
			session = c.Value
		}
	}
	f.call(session, "", "GET", "/api/session", nil, 200)
	calls := requests
	callback(state, cookie, "/login?error=oauth_failed")
	if requests != calls {
		t.Fatal("replayed OAuth state reached provider")
	}
	f.call("root", "", "PUT", "/api/settings", map[string]bool{"registration_enabled": false}, 200)
	state, cookie = start("", map[string]any{})
	callback(state, cookie, "/")
	secret, err := seal(f.s.Key, []byte("12345678901234567890"))
	if err != nil {
		t.Fatal(err)
	}
	f.sql("INSERT INTO account_totp(user_id,secret,confirmed) SELECT id,$1,true FROM users WHERE github_id='100'", secret)
	state, cookie = start("", map[string]any{})
	pendingResponse := callback(state, cookie, "/login?authenticated=1")
	for _, c := range pendingResponse.Result().Cookies() {
		if c.Name == "__Host-cove" {
			f.call(c.Value, "", "GET", "/api/session", nil, 401)
		}
	}
	f.sql("DELETE FROM account_totp WHERE user_id=(SELECT id FROM users WHERE github_id='100')")
	f.sql("UPDATE users SET disabled=true WHERE github_id='100'")
	state, cookie = start("", map[string]any{})
	callback(state, cookie, "/login?error=account_exists")
	f.user("existing", "existing@example.com")
	email = "existing@example.com"
	githubID = 200
	state, cookie = start("", map[string]any{})
	callback(state, cookie, "/login?error=account_exists")
	// A matching email can only attach an identity after password reauthentication.
	password := randomToken()
	hash, _ := passwordHash(password)
	f.sql("UPDATE users SET password_hash=$1 WHERE id='existing'", hash)
	login := f.call("", "", "POST", "/auth/login", map[string]string{"email": email, "password": password}, 200)
	existing := login.Result().Cookies()[0].Value
	state, cookie = start(existing, map[string]any{"link": true, "current_password": password})
	// A credential rotation can occur after the callback consumes its state but
	// before the provider responds. The captured version must still reject it.
	f.sql("UPDATE users SET auth_version=auth_version+1 WHERE id='existing'")
	callback(state, cookie, "/login?error=oauth_failed")
	state, cookie = start(existing, map[string]any{"link": true, "current_password": password})
	callback(state, cookie, "/")
	state, cookie = start("", map[string]any{})
	callback(state, cookie, "/")
	w = f.call("root", "default", "POST", "/api/teams/default/invitations", map[string]any{"email": "invite@example.com", "role": "member", "grants": []grant{{"team", "", "viewer"}}}, 201)
	inviteURL, _ := url.Parse(jsonString(t, w, "url"))
	email = "invite@example.com"
	githubID = 300
	state, cookie = start("", map[string]any{"invitation_token": inviteURL.Fragment})
	callback(state, cookie, "/")
	var count int
	if f.s.DB.QueryRow(t.Context(), "SELECT count(*) FROM team_members m JOIN users u ON u.id=m.user_id WHERE u.github_id='300' AND m.team_id='default'").Scan(&count) != nil || count != 1 {
		t.Fatal("GitHub invite did not enroll team member")
	}
}
