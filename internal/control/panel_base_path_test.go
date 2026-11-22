package control

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPanelMountPathsRedirectsAndFrontendMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<html><head></head><body>panel</body></html>`), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{PublicURL: "https://example.com/control/cove", FrontendDir: dir}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.frontend)
	mux.HandleFunc("GET /redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/integrations?github=failed", 303) })
	handler := s.mountPanel(mux)
	call := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	if w := call("/control/cove/dashboard"); w.Code != 200 || !strings.Contains(w.Body.String(), `<base href="/control/cove/">`) || !strings.Contains(w.Body.String(), `name="cove-base-path" content="/control/cove"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("/control/cove/redirect"); w.Header().Get("Location") != "/control/cove/integrations?github=failed" {
		t.Fatal(w.Header())
	}
	if w := call("/control/cove"); w.Code != 308 || w.Header().Get("Location") != "/control/cove/" {
		t.Fatal(w.Code, w.Header())
	}
	if w := call("https://untrusted.example/control/cove?next=x"); w.Header().Get("Location") != "/control/cove/?next=x" {
		t.Fatal("mount redirect trusted the request authority", w.Header())
	}
	for _, path := range []string{"/dashboard", "/api/session", "/control/cove-other/dashboard"} {
		if w := call(path); w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/control/cove/api/account", nil)
	r.Header.Set("Origin", "https://example.com")
	if !s.sameOrigin(r) {
		t.Fatal("browser origin rejected")
	}
	r.Header.Set("Origin", s.PublicURL)
	if s.sameOrigin(r) {
		t.Fatal("path accepted as origin")
	}
}

func TestPanelBasePathAuthenticatedAPIAndCallbacks(t *testing.T) {
	f, _, cookie := publicFixture(t)
	f.s.PublicURL = "https://cove.example.com/control"
	f.handler = f.s.Handler()
	r := httptest.NewRequest("GET", "/control/api/session", nil)
	r.Header.Set("X-Cove-Team", "default")
	r.AddCookie(&http.Cookie{Name: "__Host-cove", Value: cookie})
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	r = httptest.NewRequest("PUT", "/control/api/preferences/timezone", strings.NewReader(`{"timezone":"UTC"}`))
	r.Header.Set("Origin", "https://cove.example.com")
	r.Header.Set("X-Cove-Team", "default")
	r.AddCookie(&http.Cookie{Name: "__Host-cove", Value: cookie})
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.s.panelOrigin() != "https://cove.example.com" {
		t.Fatal("wrong origin")
	}
	r = httptest.NewRequest("POST", "/control/api/github/app/setup", strings.NewReader(`{"name":"Cove fixture","description":"Local fixture"}`))
	r.Header.Set("Authorization", "Bearer "+f.s.AdminToken)
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "https://cove.example.com/control/api/github/app/callback") || !strings.Contains(w.Body.String(), "https://cove.example.com/control/hooks/github-app") {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Path != "/" || !cookie.Secure || !cookie.HttpOnly {
			t.Fatal("cookie protections changed")
		}
	}
}

func TestPanelRedirectsWhenPrefixMatchesApplicationPaths(t *testing.T) {
	for _, base := range []string{"/api", "/projects", "/integrations"} {
		s := &Server{PublicURL: "https://example.com" + base}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/integrations?github=failed", 303) })
		w := httptest.NewRecorder()
		s.mountPanel(mux).ServeHTTP(w, httptest.NewRequest("GET", base+"/redirect", nil))
		if w.Header().Get("Location") != base+"/integrations?github=failed" {
			t.Fatal(base, w.Header())
		}
	}
}
