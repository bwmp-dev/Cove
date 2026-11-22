package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFileSecretsSelectedServicesAndNoInlineValues(t *testing.T) {
	root := t.TempDir()
	e, err := NewEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		_, err := io.WriteString(out, `{"services":{"app":{"image":"busybox:1.37","build":{"context":"/source"}},"other":{"image":"busybox:1.37"}}}`)
		return err
	}
	dir := filepath.Join(root, "workloads", "app")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "release", Workload: "app", Environment: map[string]string{"TOKEN": "private-$-token"}, FileSecrets: []FileSecret{{Name: "token", Variable: "TOKEN", Services: []string{"app"}, BuildServices: []string{"app"}}}}
	path, err := e.prepareFileSecrets(context.Background(), r, dir, []string{"docker", "compose"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), r.Environment["TOKEN"]) {
		t.Fatal("value embedded in Compose")
	}
	var config map[string]any
	if err = json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	services := config["services"].(map[string]any)
	if services["other"].(map[string]any)["secrets"] != nil {
		t.Fatal("secret granted to unrelated service")
	}
	file := filepath.Join(root, "file-secrets", "app", "release", "token")
	contents, err := os.ReadFile(file)
	if err != nil || string(contents) != r.Environment["TOKEN"] {
		t.Fatal("secret contents changed", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0444 {
			t.Fatal("non-root service cannot read mounted secret", err)
		}
		parent, err := os.Stat(filepath.Dir(file))
		if err != nil || parent.Mode().Perm() != 0700 {
			t.Fatal("host secret directory is not private", err)
		}
	}
	if strings.HasPrefix(file, dir+string(os.PathSeparator)) {
		t.Fatal("secret inside workload build context")
	}
	r.FileSecrets[0].Services = []string{"missing"}
	r.ID = "invalid"
	if _, err = e.prepareFileSecrets(context.Background(), r, dir, nil, nil); err == nil {
		t.Fatal("missing service accepted")
	}
}

func TestFileSecretValidation(t *testing.T) {
	for _, secret := range []FileSecret{{Name: "../escape", Variable: "TOKEN", Services: []string{"app"}}, {Name: "token", Variable: "TOKEN"}, {Name: "token", Variable: "TOKEN", Services: []string{"app", "app"}}} {
		if ValidateFileSecrets([]FileSecret{secret}) == nil {
			t.Fatal("invalid secret accepted", secret)
		}
	}
}
