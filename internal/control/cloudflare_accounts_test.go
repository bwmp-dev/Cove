package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func cfResponse(value any) *http.Response {
	raw, _ := json.Marshal(value)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw))}
}

func TestCloudflareMultiZoneConnection(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("cf-owner", "owner@example.test")
	other := f.user("cf-other", "other@example.test")
	f.sql("INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','cf-owner','owner','[]')")
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	token := strings.Repeat("secret", 8)
	f.s.CloudflareTransport = dnsTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("incorrect connection credential")
		}
		if strings.HasSuffix(r.URL.Path, "/"+a) {
			return cfResponse(map[string]any{"success": true, "result": map[string]string{"name": "example.com"}}), nil
		}
		result := []discoveredCloudflareZone{{a, "example.com"}}
		if r.URL.Query().Get("page") == "2" {
			result = []discoveredCloudflareZone{{b, "example.net"}}
		}
		return cfResponse(map[string]any{"success": true, "result": result, "result_info": map[string]int{"total_pages": 2}}), nil
	})
	out := f.call(owner, "", "POST", "/api/cloudflare/connections", map[string]string{"name": "Personal", "token": token}, 200)
	id := jsonString(t, out, "id")
	if strings.Contains(out.Body.String(), token) || !strings.Contains(out.Body.String(), "example.net") {
		t.Fatal("discovery or secret handling failed")
	}
	if got := f.call(other, "", "GET", "/api/cloudflare/connections", nil, 200).Body.String(); got != "[]\n" {
		t.Fatal("other user saw connection")
	}
	f.call(other, "", "GET", "/api/cloudflare/connections/"+id+"/zones", nil, 404)
	f.call(other, "", "PUT", "/api/cloudflare/connections/"+id+"/credentials", map[string]string{"name": "Stolen", "token": token}, 404)
	path := "/api/cloudflare/connections/" + id + "/zones"
	f.call(owner, "", "POST", path, map[string]any{"zones": []string{a, strings.Repeat("f", 32)}}, 400)
	f.call(owner, "", "POST", path, map[string]any{"zones": []string{a, b}}, 200)
	var imported []cloudflareZone
	if err := json.Unmarshal(f.call(owner, "", "GET", "/api/cloudflare", nil, 200).Body.Bytes(), &imported); err != nil || len(imported) != 2 || !imported[0].Enabled || !imported[1].Enabled {
		t.Fatal("new domains should enable automatic DNS", err)
	}
	f.call(owner, "", "PUT", "/api/cloudflare", cloudflareZone{ZoneID: a, Domain: "example.com", Nodes: map[string]string{}, Proxied: true}, 200)
	var count int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM cloudflare_zone_teams").Scan(&count); err != nil || count != 0 {
		t.Fatal("import automatically shared zones", err)
	}
	f.call(owner, "", "PUT", "/api/cloudflare/"+a+"/shares", map[string]any{"shares": []cloudflareShare{{Team: "default", Domains: []string{"app.example.com"}}}}, 200)
	f.call(owner, "", "POST", path, map[string]any{"zones": []string{a}}, 200)
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM cloudflare_zone_teams WHERE zone_id=$1", a).Scan(&count); err != nil || count != 1 {
		t.Fatal("reconnect removed grants", err)
	}
	listed := f.call(owner, "", "GET", "/api/cloudflare", nil, 200).Body.String()
	if strings.Contains(listed, token) || !strings.Contains(listed, `"enabled":false`) {
		t.Fatal("unsafe imported settings")
	}
	if !strings.Contains(listed, `"proxied":true`) {
		t.Fatal("reconnect lost DNS settings")
	}
	out = f.call(other, "", "POST", "/api/cloudflare/connections", map[string]string{"name": "Other", "token": token}, 200)
	otherID := jsonString(t, out, "id")
	f.call(other, "", "POST", "/api/cloudflare/connections/"+otherID+"/zones", map[string]any{"zones": []string{a, b}}, 409)
	var raw []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT credentials FROM cloudflare_accounts WHERE id=$1", id).Scan(&raw); err != nil || bytes.Contains(raw, []byte(token)) {
		t.Fatal("credentials not encrypted", err)
	}
	got, err := f.s.cloudflareConnectionToken(context.Background(), id)
	if err != nil || got != token {
		t.Fatal("connection lookup failed", err)
	}
	f.s.CloudflareTransport = dnsTransport(func(r *http.Request) (*http.Response, error) {
		return cfResponse(map[string]any{"success": true, "result": []discoveredCloudflareZone{{a, "example.com"}}}), nil
	})
	f.call(owner, "", "PUT", "/api/cloudflare/connections/"+id+"/credentials", map[string]string{"name": "Reduced", "token": strings.Repeat("replacement", 4)}, 409)
	got, err = f.s.cloudflareConnectionToken(context.Background(), id)
	if err != nil || got != token {
		t.Fatal("failed rotation replaced credential", err)
	}
}

func TestCloudflarePublicAddressDefaults(t *testing.T) {
	for _, test := range []struct {
		ips  []string
		want string
	}{
		{[]string{"127.0.0.1", "10.0.0.1", "::1"}, ""},
		{[]string{"8.8.8.8", "8.8.8.8", "2606:4700:4700::1111"}, "8.8.8.8"},
		{[]string{"8.8.8.8", "1.1.1.1"}, ""},
		{[]string{"2606:4700:4700::1111"}, "2606:4700:4700::1111"},
	} {
		ips := []netip.Addr{}
		for _, ip := range test.ips {
			ips = append(ips, netip.MustParseAddr(ip))
		}
		if got := cloudflarePublicAddress(ips); got != test.want {
			t.Errorf("%v: got %q want %q", test.ips, got, test.want)
		}
	}
}

func setupCloudflareOAuth(t *testing.T) (*accountFixture, string) {
	f := newAccountFixture(t)
	user := f.user("cf-oauth-user", "oauth@example.test")
	f.call(user, "", "PUT", "/api/cloudflare/oauth", cloudflareOAuthConfig{ClientID: "client", Secret: strings.Repeat("s", 32)}, 403)
	f.call("root", "", "PUT", "/api/cloudflare/oauth", cloudflareOAuthConfig{ClientID: "client", Secret: strings.Repeat("s", 32)}, 200)
	if out := f.call(user, "", "GET", "/api/cloudflare/oauth", nil, 200).Body.String(); strings.Contains(out, "client_id") || strings.Contains(out, "client_secret") {
		t.Fatal("client credentials disclosed")
	}
	f.s.CloudflareTransport = dnsTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oauth2/token" {
			id, secret, ok := r.BasicAuth()
			if !ok || id != "client" || secret != strings.Repeat("s", 32) {
				t.Fatal("incorrect OAuth client authentication")
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code_verifier") == "" || r.Form.Get("redirect_uri") != f.s.PublicURL+"/api/cloudflare/oauth/callback" {
				t.Fatal("OAuth exchange missing binding")
			}
			return cfResponse(map[string]any{"access_token": strings.Repeat("access", 8), "refresh_token": strings.Repeat("refresh", 8), "token_type": "Bearer", "expires_in": 3600, "scope": cloudflareOAuthScopes}), nil
		}
		return cfResponse(map[string]any{"success": true, "result": []discoveredCloudflareZone{}}), nil
	})
	return f, user
}

func startCFOAuth(t *testing.T, f *accountFixture, user string) (string, *http.Cookie) {
	t.Helper()
	w := f.call(user, "", "POST", "/api/cloudflare/oauth/start", map[string]any{}, 200)
	u, err := url.Parse(jsonString(t, w, "url"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "dash.cloudflare.com" || u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("scope") != cloudflareOAuthScopes {
		t.Fatal("invalid authorization URL")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatal("unsafe OAuth browser cookie")
	}
	return u.Query().Get("state"), cookies[0]
}

func callbackCFOAuth(f *accountFixture, state string, cookie *http.Cookie, session string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/api/cloudflare/oauth/callback?state="+url.QueryEscape(state)+"&code=test-code", nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if session != "" {
		r.AddCookie(&http.Cookie{Name: "__Host-cove", Value: session})
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func TestCloudflareOAuthSessionBinding(t *testing.T) {
	f, user := setupCloudflareOAuth(t)
	state, cookie := startCFOAuth(t, f, user)
	if got := callbackCFOAuth(f, state, nil, "").Header().Get("Location"); !strings.HasSuffix(got, "expired") {
		t.Fatal("callback lacked browser binding")
	}
	if got := callbackCFOAuth(f, state, cookie, "").Header().Get("Location"); !strings.HasSuffix(got, "connected") {
		t.Fatal("valid OAuth callback failed", got)
	}
	if got := callbackCFOAuth(f, state, cookie, "").Header().Get("Location"); !strings.HasSuffix(got, "expired") {
		t.Fatal("OAuth state replay accepted")
	}
	var count int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM cloudflare_accounts WHERE owner_id='cf-oauth-user' AND kind='oauth'").Scan(&count); err != nil || count != 1 {
		t.Fatal("OAuth connection not saved", err)
	}
	state, cookie = startCFOAuth(t, f, user)
	if got := callbackCFOAuth(f, state, cookie, "different-session").Header().Get("Location"); !strings.HasSuffix(got, "expired") {
		t.Fatal("different browser session accepted")
	}
	state, cookie = startCFOAuth(t, f, user)
	f.sql("UPDATE cloudflare_oauth_states SET expires_at=now()-interval '1 second'")
	if got := callbackCFOAuth(f, state, cookie, "").Header().Get("Location"); !strings.HasSuffix(got, "expired") {
		t.Fatal("expired state accepted")
	}
	state, cookie = startCFOAuth(t, f, user)
	f.sql("DELETE FROM sessions WHERE user_id='cf-oauth-user'")
	if got := callbackCFOAuth(f, state, cookie, "").Header().Get("Location"); !strings.HasSuffix(got, "expired") {
		t.Fatal("revoked session accepted")
	}
	f.call("root", "", "PUT", "/api/cloudflare/oauth", cloudflareOAuthConfig{ClientID: "other-client", Secret: strings.Repeat("s", 32)}, 409)
}

func TestCloudflareRefreshRotation(t *testing.T) {
	f, _ := setupCloudflareOAuth(t)
	config := f.s.DB.Config()
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.s.DB = pool
	oldRefresh := strings.Repeat("old-refresh", 4)
	newToken := strings.Repeat("new-token", 5)
	raw, _ := json.Marshal(cloudflareCredentials{Token: strings.Repeat("expired", 8), Refresh: oldRefresh, Expires: time.Now().Add(-time.Minute), ClientID: "client"})
	encrypted, err := seal(f.s.Key, raw)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("INSERT INTO cloudflare_accounts(id,owner_id,name,kind,credentials) VALUES('refresh','cf-oauth-user','Refresh','oauth',$1)", encrypted)
	var exchanges atomic.Int32
	f.s.CloudflareTransport = dnsTransport(func(r *http.Request) (*http.Response, error) {
		exchanges.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("refresh_token") != oldRefresh || r.Form.Get("grant_type") != "refresh_token" {
			t.Error("incorrect refresh token")
		}
		return cfResponse(map[string]any{"access_token": newToken, "refresh_token": strings.Repeat("new-refresh", 4), "token_type": "Bearer", "expires_in": 3600}), nil
	})
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := f.s.cloudflareConnectionToken(ctx, "refresh")
			if err != nil || token != newToken {
				t.Error("concurrent refresh failed", err)
			}
		}()
	}
	wg.Wait()
	if exchanges.Load() != 1 {
		t.Fatal("refresh token used more than once", exchanges.Load())
	}
	if err = f.s.DB.QueryRow(context.Background(), "SELECT credentials FROM cloudflare_accounts WHERE id='refresh'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	plain, err := unseal(f.s.Key, raw)
	if err != nil || !bytes.Contains(plain, []byte("new-refresh")) {
		t.Fatal("rotated refresh credential not persisted", err)
	}
}

func TestCloudflareDiscoveryRejectsPartialResults(t *testing.T) {
	s := &Server{CloudflareTransport: dnsTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("page") == "2" {
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("sensitive provider message"))}, nil
		}
		return cfResponse(map[string]any{"success": true, "result": []discoveredCloudflareZone{{strings.Repeat("a", 32), "example.com"}}, "result_info": map[string]int{"total_pages": 2}}), nil
	})}
	zones, err := s.discoverCloudflareZones(context.Background(), strings.Repeat("t", 32))
	if err == nil || zones != nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("partial or sensitive discovery result", err)
	}
}
