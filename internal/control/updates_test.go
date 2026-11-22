package control

import (
	"cove/internal/buildinfo"
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

func TestWorkerRelease(t *testing.T) {
	dir := t.TempDir()
	data := []byte("test worker release")
	if err := os.WriteFile(filepath.Join(dir, "cove-worker-linux-amd64"), data, 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{BinaryDir: dir}
	handler := s.Handler()
	sum := fmt.Sprintf("%x", sha256.Sum256(data))
	for _, test := range []struct {
		path   string
		status int
		body   string
	}{
		{"/releases/worker/amd64", 200, sum},
		{"/releases/worker/amd64/" + sum, 200, string(data)},
		{"/releases/worker/amd64/incorrect", 404, ""},
		{"/releases/worker/arm64", 503, ""},
		{"/releases/worker/unknown", 404, ""},
		{"/releases/worker/install.sh", 200, "cove-worker-update.timer"},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", test.path, nil))
		if w.Code != test.status || !strings.Contains(w.Body.String(), test.body) {
			t.Fatalf("%s: %d %s", test.path, w.Code, w.Body.String())
		}
	}
}

func TestInstallerIncludesUpdater(t *testing.T) {
	tmpl, err := template.ParseFS(assets, "install.sh.tmpl", "worker-update*.sh.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	var result strings.Builder
	if err = tmpl.Execute(&result, map[string]string{"URL": "https://panel.example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"https://panel.example.com/releases/worker/", "systemctl enable --now cove-worker-update.timer", "sha256sum -c -", "Worker already in maintenance"} {
		if !strings.Contains(result.String(), expected) {
			t.Fatalf("installer missing %s", expected)
		}
	}
}

func TestUpdateChannelAuthorizationAndPersistence(t *testing.T) {
	f := newAccountFixture(t)
	if got := jsonString(t, f.call("root", "", "GET", "/api/updates", nil, 200), "channel"); got != "release" {
		t.Fatal("default channel:", got)
	}
	user := f.user("update-viewer", "viewer@example.com")
	f.call(user, "", "GET", "/api/updates", nil, 403)
	f.call(user, "", "PUT", "/api/updates", map[string]string{"channel": "beta"}, 403)
	f.call("root", "", "PUT", "/api/updates", map[string]string{"channel": "unknown"}, 400)
	f.sql("INSERT INTO panel_channel_observations(channel,version,revision,schema_hash,revision_time) VALUES('beta',$1,$2,$3,now()),('release',$1,$2,$3,now())", buildinfo.Version, strings.Repeat("a", 40), schemaFingerprint())
	for _, channel := range []string{"beta", "release"} {
		f.call("root", "", "PUT", "/api/updates", map[string]string{"channel": channel}, 200)
		if got := jsonString(t, f.call("root", "", "GET", "/api/updates", nil, 200), "channel"); got != channel {
			t.Fatal("saved channel:", got)
		}
	}
}
