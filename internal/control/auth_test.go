package control

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestSessionSecurityIntegration(t *testing.T) {
	database := os.Getenv("COVE_TEST_DATABASE_URL")
	if database == "" {
		t.Skip("COVE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db := openTestDatabase(t, database)
	var err error
	s := &Server{DB: db, Key: bytes.Repeat([]byte{9}, 32), AdminToken: strings.Repeat("b", 32), PublicURL: "https://cove.example.com"}
	if err = s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	password := randomToken()
	if err = s.InitAdministrator(ctx, password); err != nil {
		t.Fatal(err)
	}
	defer func() {
		db.Exec(ctx, "DELETE FROM sessions")
		db.Exec(ctx, "DELETE FROM administrators")
		db.Exec(ctx, "DELETE FROM login_attempts WHERE address='192.0.2.1'")
	}()
	handler := s.Handler()
	login := func(origin, password string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("POST", "/auth/login", strings.NewReader(`{"password":"`+password+`"}`))
		request.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if got := login("https://attacker.example", password).Code; got != 403 {
		t.Fatalf("cross-origin login accepted: %d", got)
	}
	if got := login(s.PublicURL, "incorrect-password").Code; got != 401 {
		t.Fatalf("bad password accepted: %d", got)
	}
	response := login(s.PublicURL, password)
	if response.Code != 200 {
		t.Fatalf("login failed: %s", response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("expected session and device cookies, got %d", len(cookies))
	}
	var cookie *http.Cookie
	for _, candidate := range cookies {
		if !candidate.Secure || !candidate.HttpOnly || candidate.Path != "/" || candidate.SameSite != http.SameSiteStrictMode {
			t.Fatal("insecure authentication cookie")
		}
		if candidate.Name == "__Host-cove" {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("missing session cookie")
	}
	request := httptest.NewRequest("GET", "/api/session", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal("session rejected")
	}
	request = httptest.NewRequest("POST", "/api/nodes", strings.NewReader(`{"name":"bad","address":"localhost","port":22}`))
	request.AddCookie(cookie)
	request.Header.Set("Origin", "https://attacker.example")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("cross-site mutation accepted")
	}
	request = httptest.NewRequest("POST", "/auth/logout", nil)
	request.AddCookie(cookie)
	request.Header.Set("Origin", s.PublicURL)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal("logout failed")
	}
	request = httptest.NewRequest("GET", "/api/session", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("revoked session accepted")
	}
	response = login(s.PublicURL, password)
	cookie = response.Result().Cookies()[0]
	newPassword := randomToken()
	request = httptest.NewRequest("POST", "/api/password", strings.NewReader(`{"current_password":"`+password+`","new_password":"`+newPassword+`"}`))
	request.AddCookie(cookie)
	request.Header.Set("Origin", s.PublicURL)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal("password rotation failed", response.Body.String())
	}
	request = httptest.NewRequest("GET", "/api/session", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("rotation did not revoke prior session")
	}
	if login(s.PublicURL, password).Code != 401 {
		t.Fatal("old password still accepted")
	}
	if login(s.PublicURL, newPassword).Code != 200 {
		t.Fatal("new password rejected")
	}
	for attempt := 0; attempt < 10; attempt++ {
		response = login(s.PublicURL, "incorrect-password")
	}
	if response.Code != 429 {
		t.Fatal("login attempts were not limited")
	}
}
