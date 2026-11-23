package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJournalPruningProtectsRecoveryAndRetainedReferences(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ids := []string{"retained", "backup", "recovery", "unfinished-reference", "obsolete"}
	candidates := []JournalCandidate{}
	for _, id := range ids {
		j := journal{Operation: Operation{ID: id, Workload: "app", State: "succeeded", FinishedAt: &now}, Request: Request{ID: id, Workload: "app"}}
		if err := writeJSON(e.path(id), j); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.Root, "operations", id+".log"), []byte("output"), 0600); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, JournalCandidate{ID: id, Workload: "app"})
	}
	if err := writeJSON(e.path("unfinished"), journal{Operation: Operation{ID: "unfinished", Workload: "app", State: "running"}, Request: Request{Release: "unfinished-reference"}}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", "app")
	if err := os.MkdirAll(filepath.Join(dir, "release-states"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "backups"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]any{
		filepath.Join(dir, "current.json"):           deployedState{ComposePath: filepath.Join(dir, "release-states", "retained.compose.json")},
		filepath.Join(dir, "backups", "backup.json"): map[string]string{"id": "backup"},
		filepath.Join(dir, "copy-recovery.json"):     map[string]string{"operation": "recovery"},
	} {
		if err := writeJSON(path, value); err != nil {
			t.Fatal(err)
		}
	}
	result, err := e.PruneJournals(context.Background(), JournalPruneRequest{Candidates: candidates})
	if err != nil || len(result.Pruned) != 1 || result.Pruned[0] != "obsolete" || result.Protected != 4 {
		t.Fatal(result, err)
	}
	if _, err = os.Stat(e.path("obsolete")); !os.IsNotExist(err) {
		t.Fatal("journal remains", err)
	}
	if _, err = os.Stat(filepath.Join(e.Root, "operations", "obsolete.log")); !os.IsNotExist(err) {
		t.Fatal("output remains", err)
	}
	result, err = e.PruneJournals(context.Background(), JournalPruneRequest{Candidates: []JournalCandidate{{ID: "obsolete", Workload: "app"}}})
	if err != nil || len(result.Pruned) != 1 {
		t.Fatal("lost response cannot be retried", result, err)
	}
	e.active["app"] = true
	if _, err = e.PruneJournals(context.Background(), JournalPruneRequest{Candidates: candidates}); err == nil {
		t.Fatal("pruning overlapped active execution")
	}
}
