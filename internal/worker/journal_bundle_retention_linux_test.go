package worker

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJournalBundlePruningRejectsSymlinks(t *testing.T) {
	for _, location := range []string{"workload", "bundles", "operation", "child"} {
		t.Run(location, func(t *testing.T) {
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "keep")
			if err = os.WriteFile(sentinel, []byte("unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			workload := filepath.Join(e.Root, "workloads", "app")
			bundles := filepath.Join(workload, "compose-bundles")
			dir := filepath.Join(bundles, "obsolete")
			link := map[string]string{"workload": workload, "bundles": bundles, "operation": dir, "child": filepath.Join(dir, "linked")}[location]
			if err = os.MkdirAll(filepath.Dir(link), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if err = writeJSON(e.path("obsolete"), journal{Operation: Operation{ID: "obsolete", Workload: "app", State: "succeeded", FinishedAt: &now}, Request: Request{ID: "obsolete", Workload: "app"}}); err != nil {
				t.Fatal(err)
			}
			if result, err := e.PruneJournals(t.Context(), JournalPruneRequest{Candidates: []JournalCandidate{{ID: "obsolete", Workload: "app"}}}); err == nil {
				t.Fatal("symlink accepted", result)
			}
			if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "unchanged" {
				t.Fatal("external file changed", err)
			}
			if _, err := os.Lstat(e.path("obsolete")); err != nil {
				t.Fatal("refused journal removed", err)
			}
		})
	}
}
