package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeComposeKeepsSourceAndMergeTags(t *testing.T) {
	content := "services:\n  web:\n    image: busybox\n    ports: !override ['80:80']\nnetworks:\n  private:\n    name: null\n  other:\n    name: explicit\n  reset:\n    name: !reset null\n"
	source := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(source, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	path, err := normalizeComposeFile(source)
	if err != nil || path == source || filepath.Dir(path) != filepath.Dir(source) {
		t.Fatal(path, err)
	}
	original, err := os.ReadFile(source)
	if err != nil || string(original) != content {
		t.Fatal("source changed", err)
	}
	normalized, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(normalized), "name: null") || !strings.Contains(string(normalized), "!override") || !strings.Contains(string(normalized), "name: explicit") || !strings.Contains(string(normalized), "!reset") {
		t.Fatal("normalization changed unrelated configuration", err)
	}
	unchanged, err := normalizeComposeFile(path)
	if err != nil || unchanged != path {
		t.Fatal("normalization was not stable", err)
	}
}
