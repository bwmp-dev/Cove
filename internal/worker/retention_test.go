package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReleaseRetentionProtectsCurrentRollbackAndBackup(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", "app")
	if err = os.MkdirAll(filepath.Join(dir, "release-states"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(dir, "backups"), 0700); err != nil {
		t.Fatal(err)
	}
	states := map[int]deployedState{}
	for i := 0; i < 15; i++ {
		id := fmt.Sprintf("release-%02d", i)
		state := deployedState{ComposePath: filepath.Join(dir, "release-states", id+".compose.json")}
		states[i] = state
		if err = writeJSON(state.ComposePath, map[string]any{}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "release-states", id+".json")
		if err = writeJSON(path, state); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(time.Duration(i-15) * time.Hour)
		if err = os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err = writeJSON(filepath.Join(dir, "current.json"), states[0]); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(dir, "backups", "snapshot.json"), backupManifest{Record: BackupRecord{ID: "snapshot"}, State: states[1]}); err != nil {
		t.Fatal(err)
	}
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error { return nil }
	if err = e.pruneReleases(context.Background(), "app", io.Discard); err != nil {
		t.Fatal(err)
	}
	retained, err := e.Releases("app")
	if err != nil {
		t.Fatal(err)
	}
	if len(retained) != 12 {
		t.Fatalf("got %d releases, want 10 recent plus current and backup", len(retained))
	}
	for _, id := range []string{"release-00", "release-01", "release-14"} {
		if _, err = os.Stat(filepath.Join(dir, "release-states", id+".json")); err != nil {
			t.Fatal(id, err)
		}
	}
	if _, err = os.Stat(filepath.Join(dir, "release-states", "release-02.json")); !os.IsNotExist(err) {
		t.Fatal("old release retained", err)
	}
}
