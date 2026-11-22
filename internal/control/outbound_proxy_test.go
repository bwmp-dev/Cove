package control

import (
	"context"
	"cove/internal/buildinfo"
	"cove/internal/outbound"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyDoesNotBypassWebhookDestinationValidation(t *testing.T) {
	called := false
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(502) }))
	defer proxy.Close()
	transport := publicWebhookTransport(outbound.Config{HTTPS: proxy.URL})
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	if response, err := client.Get("https://127.0.0.1/private"); err == nil {
		response.Body.Close()
		t.Fatal("private webhook destination accepted")
	}
	if called {
		t.Fatal("private destination reached proxy")
	}
}

func TestSecurityObservationsAreMeasuredIntegration(t *testing.T) {
	f := newAccountFixture(t)
	w := f.call("root", "", "GET", "/api/settings/security-posture", nil, 200)
	if !strings.Contains(w.Body.String(), `"id":"panel_backup","status":"warn"`) {
		t.Fatal(w.Body.String())
	}
	f.sql("UPDATE control_settings SET panel_backup_at=now(),panel_update_checked_at=now(),latest_panel_version=$1,panel_update_channel=update_channel WHERE id=1", buildinfo.Version)
	w = f.call("root", "", "GET", "/api/settings/security-posture", nil, 200)
	for _, id := range []string{"panel_backup", "updates", "port_exposure"} {
		if !strings.Contains(w.Body.String(), `"id":"`+id+`","status":"pass"`) {
			t.Fatal(w.Body.String())
		}
	}
	f.sql("UPDATE control_settings SET panel_backup_at=now()-interval '3 days',panel_update_checked_at=now()-interval '3 hours' WHERE id=1")
	w = f.call("root", "", "GET", "/api/settings/security-posture", nil, 200)
	for _, id := range []string{"panel_backup", "updates"} {
		if !strings.Contains(w.Body.String(), `"id":"`+id+`","status":"warn"`) {
			t.Fatal(w.Body.String())
		}
	}
}

func TestOutboundProxySealedAndScopedIntegration(t *testing.T) {
	f := newAccountFixture(t)
	member := f.user("proxy-viewer", "viewer@example.com")
	f.call(member, "", "GET", "/api/settings/outbound-proxy", nil, 403)
	secret := "private-proxy-password"
	w := f.call("root", "", "PUT", "/api/settings/outbound-proxy", map[string]string{"http_proxy": "http://proxy.example:3128", "https_proxy": "http://user:" + secret + "@proxy.example:3128", "no_proxy": "localhost,10.0.0.0/8"}, 200)
	if strings.Contains(w.Body.String(), secret) || !strings.Contains(w.Body.String(), `"https_credentials_set":true`) {
		t.Fatal(w.Body.String())
	}
	var encrypted []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT outbound_proxy FROM control_settings WHERE id=1").Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encrypted), secret) {
		t.Fatal("proxy credentials are plaintext")
	}
	f.call("root", "", "PUT", "/api/settings/outbound-proxy", map[string]string{"no_proxy": "*.example.com"}, 200)
	c, err := f.s.outboundConfig(context.Background())
	if err != nil || !strings.Contains(c.HTTPS, secret) {
		t.Fatal("partial update lost credentials", err)
	}
	f.call("root", "", "PUT", "/api/settings/outbound-proxy", map[string]string{"https_proxy": "socks5://proxy.example"}, 400)
}
