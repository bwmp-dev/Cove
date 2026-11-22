package worker

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestReleaseStorage(t *testing.T) {
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	for _, tc := range []struct {
		name, kind, source string
		readOnly, valid    bool
	}{
		{"named volume", "volume", "database", false, true},
		{"stable bind", "bind", filepath.Join(root, "data"), false, true},
		{"release data", "bind", filepath.Join(releases, "new", "data"), false, false},
		{"release root", "bind", releases, false, false},
		{"read only config", "bind", filepath.Join(releases, "new", "config"), true, true},
		{"unresolved relative", "bind", "./data", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"services": map[string]any{"app": map[string]any{"volumes": []any{map[string]any{"type": tc.kind, "source": tc.source, "read_only": tc.readOnly}}}}})
			if err != nil {
				t.Fatal(err)
			}
			err = validateReleaseStorage(data, releases)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	if validateReleaseStorage([]byte("invalid"), releases) == nil {
		t.Fatal("invalid config accepted")
	}
}
