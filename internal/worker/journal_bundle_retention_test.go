package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJournalPruningRemovesOnlyUnreferencedOperationBundles(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	candidates := []JournalCandidate{{ID: "retained-bundle", Workload: "app"}, {ID: "obsolete-bundle", Workload: "app"}}
	create := func(workload, id string) {
		t.Helper()
		dir := filepath.Join(e.Root, "workloads", workload, "compose-bundles", id)
		if err := os.MkdirAll(filepath.Join(dir, "assets"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cove-retained-images-"+id+".yaml"), []byte("services: {}"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "assets", "data"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range candidates {
		create(c.Workload, c.ID)
		if err := writeJSON(e.path(c.ID), journal{Operation: Operation{ID: c.ID, Workload: c.Workload, State: "succeeded", FinishedAt: &now}, Request: Request{ID: c.ID, Workload: c.Workload}}); err != nil {
			t.Fatal(err)
		}
	}
	create("other", "obsolete-bundle")
	if err := writeJSON(filepath.Join(e.Root, "workloads", "app", "current.json"), deployedState{ComposePath: filepath.Join(e.Root, "workloads", "app", "compose-bundles", "retained-bundle", "compose.yaml")}); err != nil {
		t.Fatal(err)
	}
	result, err := e.PruneJournals(t.Context(), JournalPruneRequest{Candidates: candidates})
	if err != nil || len(result.Pruned) != 1 || result.Pruned[0] != "obsolete-bundle" || result.Protected != 1 {
		t.Fatal("bundle path references were not protected", result, err)
	}
	for _, name := range []string{e.path("obsolete-bundle"), filepath.Join(e.Root, "workloads", "app", "compose-bundles", "obsolete-bundle")} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Fatal("obsolete operation state remains", name, err)
		}
	}
	for _, name := range []string{e.path("retained-bundle"), filepath.Join(e.Root, "workloads", "app", "compose-bundles", "retained-bundle"), filepath.Join(e.Root, "workloads", "other", "compose-bundles", "obsolete-bundle")} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatal("live or another workload's state was removed", name, err)
		}
	}
	create("app", "obsolete-bundle")
	result, err = e.PruneJournals(context.Background(), JournalPruneRequest{Candidates: []JournalCandidate{{ID: "obsolete-bundle", Workload: "app"}}})
	if err != nil || len(result.Pruned) != 1 {
		t.Fatal("journal-missing retry failed", result, err)
	}
	if _, err := os.Lstat(filepath.Join(e.Root, "workloads", "app", "compose-bundles", "obsolete-bundle")); !os.IsNotExist(err) {
		t.Fatal("retry left bundle state", err)
	}
}

func TestJournalBundleNativeResolutionPruning(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("Docker Compose required")
	}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", "app")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	request := Request{ID: "resolved-bundle", Workload: "app", Action: "deploy", Compose: "services:\n  app:\n    image: busybox:1.37\n    volumes: [./assets:/fixture:ro]\n", ComposeBundle: &ComposeBundle{Files: []ComposeFile{{Path: "cove-retained-images-resolved-bundle.yaml", Content: "services:\n  app:\n    image: busybox:1.37\n"}}, Assets: map[string]string{"assets/value": "fixture"}}}
	resolved, err := e.prepareComposeBundle(t.Context(), request, dir, "unused", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if json.Unmarshal(raw, &value) != nil || !strings.Contains(string(raw), "resolved-bundle") {
		t.Fatal("real Compose did not resolve the operation's asset path", string(raw))
	}
	now := time.Now()
	if err := writeJSON(e.path(request.ID), journal{Operation: Operation{ID: request.ID, Workload: request.Workload, State: "succeeded", FinishedAt: &now}, Request: request}); err != nil {
		t.Fatal(err)
	}
	// Subsequent transforms retain the bundle's absolute asset paths in their own snapshots.
	for _, name := range []string{"builtin-" + request.ID + ".json", request.ID + ".promotion.compose.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := e.PruneJournals(t.Context(), JournalPruneRequest{Candidates: []JournalCandidate{{ID: request.ID, Workload: request.Workload}}})
	if err != nil || len(result.Pruned) != 1 {
		t.Fatal("resolved bundle retained itself", result, err)
	}
	for _, path := range []string{resolved, filepath.Join(dir, "builtin-"+request.ID+".json"), filepath.Join(dir, request.ID+".promotion.compose.json"), filepath.Join(dir, "compose-bundles", request.ID), e.path(request.ID)} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("obsolete generated bundle remains", path, err)
		}
	}
}

func TestJournalBundlePruningRefusesUnsafeInventory(t *testing.T) {
	for _, scenario := range []string{"file instead of directory", "byte bound", "unfinished operation", "recovery reference", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			c := JournalCandidate{ID: "obsolete", Workload: "app"}
			dir := filepath.Join(e.Root, "workloads", "app", "compose-bundles", c.ID)
			if err = os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			finished := &now
			if scenario == "file instead of directory" {
				err = os.WriteFile(dir, []byte("keep"), 0600)
			} else {
				err = os.Mkdir(dir, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "byte bound" {
				file, err := os.Create(filepath.Join(dir, "oversize"))
				if err != nil {
					t.Fatal(err)
				}
				err = file.Truncate((64 << 20) + 1)
				closeErr := file.Close()
				if err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
			}
			if scenario == "unfinished operation" {
				finished = nil
			}
			if err = writeJSON(e.path(c.ID), journal{Operation: Operation{ID: c.ID, Workload: c.Workload, State: "succeeded", FinishedAt: finished}, Request: Request{ID: c.ID, Workload: c.Workload}}); err != nil {
				t.Fatal(err)
			}
			if scenario == "recovery reference" {
				if err = writeJSON(filepath.Join(e.Root, "workloads", "app", "copy-recovery.json"), map[string]string{"path": filepath.Join(dir, "compose.yaml")}); err != nil {
					t.Fatal(err)
				}
			}
			ctx := t.Context()
			if scenario == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			result, err := e.PruneJournals(ctx, JournalPruneRequest{Candidates: []JournalCandidate{c}})
			protected := scenario == "unfinished operation" || scenario == "recovery reference"
			if protected {
				if err != nil || result.Protected != 1 || len(result.Pruned) != 0 {
					t.Fatal(result, err)
				}
			} else if err == nil {
				t.Fatal("unsafe inventory pruned", result)
			}
			for _, path := range []string{dir, e.path(c.ID)} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatal("refusal removed state", path, err)
				}
			}
		})
	}
}

func TestJournalBundlePruningProtectsTransformedCurrentSnapshot(t *testing.T) {
	for _, name := range []string{"builtin-live.json", "live.promotion.compose.json", "live-links.yaml", "live-private.yaml"} {
		t.Run(name, func(t *testing.T) {
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(e.Root, "workloads", "app")
			bundle := filepath.Join(dir, "compose-bundles", "live")
			if err = os.MkdirAll(bundle, 0700); err != nil {
				t.Fatal(err)
			}
			if err = writeJSON(filepath.Join(dir, "builtin-live.json"), map[string]string{"asset": filepath.Join(bundle, "assets", "data")}); err != nil {
				t.Fatal(err)
			}
			if err = writeJSON(filepath.Join(dir, "current.json"), deployedState{ComposePath: filepath.Join(dir, name)}); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if err = writeJSON(e.path("live"), journal{Operation: Operation{ID: "live", Workload: "app", State: "succeeded", FinishedAt: &now}, Request: Request{ID: "live", Workload: "app"}}); err != nil {
				t.Fatal(err)
			}
			result, err := e.PruneJournals(t.Context(), JournalPruneRequest{Candidates: []JournalCandidate{{ID: "live", Workload: "app"}}})
			if err != nil || result.Protected != 1 || len(result.Pruned) != 0 {
				t.Fatal("live transformed snapshot lost its bundle", result, err)
			}
			for _, path := range []string{bundle, e.path("live"), filepath.Join(dir, "builtin-live.json")} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatal("live state removed", path, err)
				}
			}
		})
	}
}
