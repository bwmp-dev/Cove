package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestFirstUserSetupIntegration(t *testing.T) {
	f := newAccountFixture(t)
	f.s.SendMail = nil
	if err := f.s.InitAdministrator(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	w := f.call("", "", "GET", "/auth/config", nil, 200)
	if !strings.Contains(w.Body.String(), `"setup_required":true`) {
		t.Fatal(w.Body.String())
	}
	f.call("", "", "POST", "/auth/register", map[string]string{"email": "admin@example.com", "name": "Admin", "password": "short"}, 400)
	w = f.call("", "", "POST", "/auth/register", map[string]string{"email": "ADMIN@example.com", "name": "Admin", "password": randomToken()}, 200)
	if !strings.Contains(w.Body.String(), `"setup_complete":true`) {
		t.Fatal(w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Fatal("setup did not issue a secure session")
	}
	w = f.call(cookies[0].Value, "", "GET", "/api/session", nil, 200)
	if !strings.Contains(w.Body.String(), `"is_admin":true`) {
		t.Fatal(w.Body.String())
	}
	f.call(cookies[0].Value, "", "GET", "/api/settings/integrations", nil, 200)
	f.call("", "", "POST", "/auth/register", map[string]string{"email": "other@example.com", "name": "Other", "password": randomToken()}, 403)
	if err := f.s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	w = f.call("", "", "GET", "/auth/config", nil, 200)
	if !strings.Contains(w.Body.String(), `"setup_required":false`) {
		t.Fatal("setup reopened after restart", w.Body.String())
	}
}

func TestConcurrentFirstUserSetupIntegration(t *testing.T) {
	f := newAccountFixture(t)
	f.s.SendMail = nil
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, _ := json.Marshal(map[string]string{"email": fmt.Sprintf("admin%d@example.com", i), "name": "Admin", "password": randomToken()})
			r := httptest.NewRequest("POST", "/auth/register", bytes.NewReader(body))
			r.Header.Set("Origin", f.s.PublicURL)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			results <- w
		}()
	}
	wg.Wait()
	close(results)
	var successes, closed int
	for w := range results {
		switch w.Code {
		case 200:
			successes++
		case 403:
			closed++
		default:
			t.Fatalf("unexpected setup result %d: %s", w.Code, w.Body.String())
		}
	}
	if successes != 1 || closed != 1 {
		t.Fatalf("successes=%d closed=%d", successes, closed)
	}
	var accounts int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM users WHERE password_hash IS NOT NULL AND is_admin").Scan(&accounts); err != nil || accounts != 1 {
		t.Fatalf("administrators=%d error=%v", accounts, err)
	}
}

func TestExistingAccountPreventsSetupIntegration(t *testing.T) {
	f := newAccountFixture(t)
	f.user("existing", "existing@example.com")
	f.call("", "", "POST", "/auth/register", map[string]string{"email": "attacker@example.com", "name": "Attacker", "password": randomToken()}, 403)
	if err := f.s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.sql("DELETE FROM sessions WHERE user_id='existing'")
	f.sql("DELETE FROM users WHERE id='existing'")
	f.call("", "", "POST", "/auth/register", map[string]string{"email": "attacker@example.com", "name": "Attacker", "password": randomToken()}, 403)
}

func TestInstanceIntegrationsIntegration(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	member := f.user("member", "member@example.com")
	f.call("", "", "GET", "/api/settings/integrations", nil, 401)
	for _, method := range []string{"GET", "PUT"} {
		f.call(member, "", method, "/api/settings/integrations", nil, 403)
	}
	f.call(member, "", "POST", "/api/settings/smtp/test", map[string]string{"email": "member@example.com"}, 403)
	smtpPassword, githubSecret := randomToken(), randomToken()
	smtp := map[string]string{"host": "smtp.example.com", "port": "465", "username": "mailer", "password": smtpPassword, "from": "cove@example.com"}
	w := f.call("root", "", "PUT", "/api/settings/integrations", map[string]any{"smtp": smtp}, 200)
	if strings.Contains(w.Body.String(), smtpPassword) {
		t.Fatal("SMTP password leaked")
	}
	f.call("root", "", "PUT", "/api/settings/integrations", map[string]any{"github": map[string]string{"client_id": "fixture", "client_secret": githubSecret}}, 200)
	delete(smtp, "password")
	smtp["host"] = "smtp2.example.com"
	f.call("root", "", "PUT", "/api/settings/integrations", map[string]any{"smtp": smtp}, 200)
	config, saved, err := f.s.integrationConfig(ctx, f.s.DB)
	if err != nil || !saved || config.SMTP.Password != smtpPassword || config.Github.ClientSecret != githubSecret {
		t.Fatalf("saved credentials not preserved: saved=%t error=%v", saved, err)
	}
	var encrypted []byte
	if err = f.s.DB.QueryRow(ctx, "SELECT integration_settings FROM control_settings WHERE id=1").Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte(smtpPassword)) || bytes.Contains(encrypted, []byte(githubSecret)) {
		t.Fatal("credentials stored in plaintext")
	}
	w = f.call("root", "", "GET", "/api/settings/integrations", nil, 200)
	if strings.Contains(w.Body.String(), smtpPassword) || strings.Contains(w.Body.String(), githubSecret) {
		t.Fatal("credentials returned to client")
	}
	f.s.SMTP = SMTPConfig{Host: "stale.example.com"}
	f.s.GithubClientID = "stale"
	if err = f.s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	w = f.call("", "", "POST", "/auth/github/start", map[string]any{}, 200)
	if !strings.Contains(jsonString(t, w, "url"), "client_id=fixture") {
		t.Fatal("GitHub still uses environment credentials")
	}
	f.call("root", "", "PUT", "/api/settings", map[string]bool{"registration_enabled": true}, 200)
	f.call("root", "", "PUT", "/api/settings/integrations", map[string]any{"smtp": map[string]string{}}, 400)
	f.call("root", "", "PUT", "/api/settings/integrations", map[string]any{"smtp": map[string]string{"password": ""}}, 409)
	f.call("root", "", "PUT", "/api/settings", map[string]bool{"registration_enabled": false}, 200)
	f.call("root", "", "PUT", "/api/settings/integrations", map[string]any{"smtp": map[string]string{"password": ""}}, 200)
	f.call("root", "", "POST", "/api/settings/smtp/test", map[string]string{"email": "member@example.com"}, 409)
	f.call("root", "", "PUT", "/api/settings/integrations", map[string]any{"github": map[string]string{"client_id": "", "client_secret": ""}}, 200)
	f.call("", "", "POST", "/auth/github/start", map[string]any{}, 409)
}

func TestIntegrationEnvironmentImportAndEmailTest(t *testing.T) {
	f := newAccountFixture(t)
	f.s.SMTP = SMTPConfig{Host: "smtp.example.com", Port: "587", From: "cove@example.com"}
	f.s.GithubClientID, f.s.GithubClientSecret = "imported-client", randomToken()
	if err := f.s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	config, saved, err := f.s.integrationConfig(context.Background(), f.s.DB)
	if err != nil || !saved || config.SMTP.Host != "smtp.example.com" || config.Github.ClientID != "imported-client" {
		t.Fatal("environment not imported", err)
	}
	f.sql("UPDATE control_settings SET integration_settings=NULL")
	f.s.SMTP = SMTPConfig{}
	f.call("root", "", "POST", "/api/settings/smtp/test", map[string]string{"email": "invalid"}, 400)
	f.call("root", "", "POST", "/api/settings/smtp/test", map[string]string{"email": "member@example.com"}, http.StatusOK)
	if len(f.mail) != 1 {
		t.Fatal("test email was not sent")
	}
}
