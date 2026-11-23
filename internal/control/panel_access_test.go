package control

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPanelAccessRequiresTrustedForwarderIntegration(t *testing.T) {
	f := newAccountFixture(t)
	t.Setenv("COVE_TRUSTED_PROXIES", "")
	call := func(want int) {
		r := httptest.NewRequest("PUT", "/api/settings/panel-access", bytes.NewBufferString(`{"allowlist":["198.51.100.0/24"]}`))
		r.RemoteAddr = "10.1.2.3:1234"
		r.Header.Set("Authorization", "Bearer "+f.s.AdminToken)
		r.Header.Set("X-Forwarded-For", "198.51.100.1")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("got %d want %d: %s", w.Code, want, w.Body.String())
		}
	}
	call(409)
	t.Setenv("COVE_TRUSTED_PROXIES", "10.1.2.3/32")
	call(200)
}

func TestPanelClientAddress(t *testing.T) {
	for _, tt := range []struct {
		proxy, peer, forwarded, want string
		invalid                      bool
	}{
		{"", "192.0.2.1:80", "198.51.100.1", "192.0.2.1", false},
		{"10.0.0.0/8", "192.0.2.1:80", "198.51.100.1", "192.0.2.1", false},
		{"10.0.0.0/8", "10.0.0.1:80", "198.51.100.1, 10.0.0.2", "198.51.100.1", false},
		{"10.0.0.0/8", "10.0.0.1:80", "192.0.2.1, 198.51.100.1", "198.51.100.1", false},
		{"10.0.0.0/8", "10.0.0.1:80", "invalid", "", true},
		{"invalid", "10.0.0.1:80", "198.51.100.1", "", true},
		{"::1/128", "[::1]:80", "2001:db8::1", "2001:db8::1", false},
	} {
		t.Run(tt.peer+tt.forwarded, func(t *testing.T) {
			t.Setenv("COVE_TRUSTED_PROXIES", tt.proxy)
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.peer
			r.Header.Set("X-Forwarded-For", tt.forwarded)
			got, err := panelClientAddress(r)
			if (err != nil) != tt.invalid || err == nil && got.String() != tt.want {
				t.Fatalf("address=%v err=%v", got, err)
			}
		})
	}
}

func TestPanelAccessIntegration(t *testing.T) {
	f := newAccountFixture(t)
	f.call("root", "", "PUT", "/api/settings/panel-access", map[string]any{"allowlist": []string{"127.0.0.1"}}, 409)
	f.call("root", "", "PUT", "/api/settings/panel-access", map[string]any{"allowlist": []string{"invalid"}}, 400)
	member := f.user("member", "member@example.com")
	f.call(member, "", "GET", "/api/settings/panel-access", nil, 403)
	f.call("root", "", "PUT", "/api/settings/panel-access", map[string]any{"allowlist": []string{"192.0.2.0/24"}}, 200)
	f.sql("UPDATE control_settings SET panel_allowlist=ARRAY['198.51.100.0/24'] WHERE id=1")
	for _, path := range []string{"/", "/auth/config", "/api/session"} {
		f.call("root", "", "GET", path, nil, 403)
	}
	f.call("", "", "GET", "/healthz", nil, 200)
	f.call("", "", "POST", "/bootstrap/register", nil, 400)
	t.Setenv("COVE_PANEL_ALLOWLIST", "invalid")
	f.call("root", "", "GET", "/api/settings/panel-access", nil, 503)
	t.Setenv("COVE_PANEL_ALLOWLIST", "")
	w := f.call("root", "", "GET", "/api/settings/panel-access", nil, 200)
	if !strings.Contains(w.Body.String(), `"environment_override":true`) {
		t.Fatal(w.Body.String())
	}
	f.call("root", "", "PUT", "/api/settings/panel-access", map[string]any{"allowlist": []string{}}, 200)
}
