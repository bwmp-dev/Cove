package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientTLSOriginHeadersAndRedactedErrors(t *testing.T) {
	token := "fixture-token"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-Cove-Team") != "team" {
			t.Fatal("missing auth scope")
		}
		if r.URL.Path == "/api/v1/failure" {
			http.Error(w, "rejected "+token, 403)
			return
		}
		if r.URL.Path == "/api/v1/redirect" {
			http.Redirect(w, r, "https://elsewhere.invalid", 302)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	api, err := New(server.URL, token, "team")
	if err != nil {
		t.Fatal(err)
	}
	api.HTTP.Transport = server.Client().Transport
	var body map[string]bool
	if err = api.Do(context.Background(), "GET", "/success", nil, &body); err != nil || !body["ok"] {
		t.Fatal(err)
	}
	for _, path := range []string{"/failure", "/redirect"} {
		if err = api.Do(context.Background(), "GET", path, nil, nil); err == nil || strings.Contains(err.Error(), token) {
			t.Fatal("unsafe API error", err)
		}
	}
	for _, address := range []string{"http://localhost", "https://user:pass@example.com", "https://example.com/path/../other", "https://example.com?token=bad"} {
		if _, err = New(address, token, "team"); err == nil {
			t.Fatal("unsafe panel origin accepted")
		}
	}
}

func TestClientPreservesPanelBasePath(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/control/cove/api/v1/session" {
			t.Fatal(r.URL.Path)
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	api, err := New(server.URL+"/control/cove", "fixture-token", "team")
	if err != nil {
		t.Fatal(err)
	}
	api.HTTP = server.Client()
	if err := api.Do(t.Context(), "GET", "/session", nil, nil); err != nil {
		t.Fatal(err)
	}
}
