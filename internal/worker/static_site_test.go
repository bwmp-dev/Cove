package worker

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStaticSiteServesOnlyPublicOutput(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"index.html": "public index", ".env": "private", "site.css": "body{}", "nested/index.html": "nested index", "nested/.private": "private"} {
		file := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	handler, err := staticSiteHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.root.Close()
	for _, test := range []struct {
		method, path string
		code         int
		body         string
	}{{"GET", "/", 200, "public index"}, {"HEAD", "/", 200, ""}, {"GET", "/nested/", 200, "nested index"}, {"GET", "/nested", 404, "404 page not found\n"}, {"GET", "/missing", 404, "404 page not found\n"}, {"GET", "/.env", 404, "404 page not found\n"}, {"GET", "/nested/.private", 404, "404 page not found\n"}, {"GET", "/../.env", 404, "404 page not found\n"}, {"GET", "/%2e%2e/index.html", 404, "404 page not found\n"}, {"GET", "/nested%5c..%5c.env", 404, "404 page not found\n"}, {"POST", "/", 405, "method not allowed\n"}} {
		t.Run(test.method+test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			if response.Code != test.code || response.Body.String() != test.body || response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Cache-Control") != "no-cache" || response.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Fatal(response.Code, response.Body.String(), response.Header())
			}
		})
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/site.css", nil)
	request.Header.Set("Range", "bytes=0-3")
	handler.ServeHTTP(response, request)
	if response.Code != 206 || response.Body.String() != "body" || response.Header().Get("Content-Type") != "text/css; charset=utf-8" {
		t.Fatal(response.Code, response.Body.String(), response.Header())
	}
}

func TestStaticSiteCannotFollowSymlinkOutsideOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permission depends on developer mode")
	}
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("private outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	handler, err := staticSiteHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.root.Close()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/escape", nil))
	if response.Code != 404 {
		t.Fatal("served outside output", response.Body.String())
	}
}
