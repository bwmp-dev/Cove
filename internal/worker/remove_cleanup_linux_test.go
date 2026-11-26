package worker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeletionRejectsSymlinkRecoveryPaths(t *testing.T) {
	for _, location := range []string{"workloads", "workload", "backups", "bind"} {
		t.Run(location, func(t *testing.T) {
			e, dir, _ := deletionFixture(t, false, false)
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "keep")
			if err := os.WriteFile(sentinel, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			switch location {
			case "workloads", "workload":
				path := dir
				if location == "workloads" {
					path = filepath.Dir(dir)
				}
				if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "backups":
				if err := os.Symlink(outside, filepath.Join(dir, "backups")); err != nil {
					t.Fatal(err)
				}
			case "bind":
				path := filepath.Join(dir, "data")
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
				if err := writeJSON(filepath.Join(dir, "release-states", "deployed.compose.json"), map[string]any{"services": map[string]any{"app": map[string]any{"volumes": []any{map[string]any{"type": "bind", "source": path, "target": "/data"}}}}}); err != nil {
					t.Fatal(err)
				}
			}
			result, err := e.RemoveWorkload(t.Context(), "app")
			if location == "bind" {
				if err != nil || !result.WorkerFilesRetained || result.CleanupWarning == "" {
					t.Fatal("uncertain bind was not retained", result, err)
				}
			} else if err == nil {
				t.Fatal("unowned recovery path accepted", result)
			}
			if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "untouched" {
				t.Fatal("external data changed", err)
			}
		})
	}
}
