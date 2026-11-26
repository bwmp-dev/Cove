package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestManagedRegistryRemovalCleansWorkerState(t *testing.T) {
	for _, mode := range []string{"keep-data", "delete-data", "already-purged", "queued-restart"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if unprivileged := os.Getenv("COVE_TEST_REGISTRY_REMOVAL_NONROOT_ROOT"); unprivileged != "" {
				root = unprivileged
			}
			e, err := NewEngine(root)
			if err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("a", 32)
			dir := filepath.Join(e.Root, "workloads", id)
			snapshot := filepath.Join(dir, "release-states", "deployed.compose.json")
			if err := os.MkdirAll(filepath.Dir(snapshot), 0700); err != nil {
				t.Fatal(err)
			}
			volume := "cove-" + id + "_data"
			config := map[string]any{"services": map[string]any{"registry": map[string]any{"volumes": []any{map[string]any{"type": "volume", "source": "data", "target": "/var/lib/registry"}}}}, "volumes": map[string]any{"data": map[string]any{"name": volume}}}
			if err := writeJSON(snapshot, config); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(filepath.Join(dir, "current.json"), deployedState{ComposePath: snapshot}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "deploy.secrets.compose.json"), []byte("synthetic private override"), 0600); err != nil {
				t.Fatal(err)
			}
			secretDir := filepath.Join(e.Root, "file-secrets", id)
			if err := os.MkdirAll(secretDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(secretDir, "registry_auth"), []byte("synthetic auth"), 0600); err != nil {
				t.Fatal(err)
			}
			r := Request{ID: "registry-removal", Workload: id, Action: "registry_maintenance", Kind: "service", ManagedRegistry: &ManagedRegistryOperation{Mode: "remove", DeleteData: mode != "keep-data" && mode != "queued-restart", Policy: ManagedRegistryRetention{KeepPerRepository: 1, MinimumAgeDays: 7}}}
			if _, err := e.Submit(r); err != nil {
				t.Fatal(err)
			}
			e.pending = nil
			e.active[id] = true
			j, err := e.read(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			j.RegistryCleanup = &managedRegistryCleanup{Removing: true}
			if err := writeJSON(e.path(r.ID), j); err != nil {
				t.Fatal(err)
			}
			exists := mode != "already-purged"
			removedTag := false
			imageInventoryUnavailable := false
			image := "sha256:" + strings.Repeat("b", 64)
			tag := "cove-release/" + id + ":deploy-fixture"
			e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
				command := strings.Join(args, " ")
				switch {
				case strings.HasPrefix(command, "docker volume ls"):
					if exists {
						_, err := io.WriteString(out, volume)
						return err
					}
					return nil
				case strings.HasPrefix(command, "docker volume inspect"):
					if !exists {
						return errors.New("volume not found")
					}
					return json.NewEncoder(out).Encode(map[string]any{"Name": volume, "Driver": "local", "Labels": map[string]string{"dev.cove.managed-registry": id, "com.docker.compose.project": "cove-" + id, "com.docker.compose.volume": "data"}})
				case strings.HasPrefix(command, "docker volume rm"):
					exists = false
					return nil
				case strings.HasPrefix(command, "docker image ls"):
					if imageInventoryUnavailable {
						return errors.New("fixture image inventory unavailable")
					}
					if !removedTag {
						_, err := io.WriteString(out, image)
						return err
					}
					return nil
				case strings.HasPrefix(command, "docker image inspect"):
					return json.NewEncoder(out).Encode(map[string]any{"Id": image, "RepoTags": []string{tag, "user-owned:retained"}})
				case strings.HasPrefix(command, "docker image rm "):
					if command != "docker image rm "+tag {
						t.Fatal("removed a foreign image name", command)
					}
					removedTag = true
					return nil
				default:
					return nil
				}
			}
			if mode == "queued-restart" {
				j.Operation.State = "running"
				if err := writeJSON(e.path(r.ID), j); err != nil {
					t.Fatal(err)
				}
				waiting := Request{ID: "waiting-deploy", Workload: "other", Action: "deploy", Kind: "service", Compose: "services:\n  app:\n    image: " + tag + "\n"}
				if _, err := e.Submit(waiting); err != nil {
					t.Fatal(err)
				}
				if err := e.finishManagedRegistryRemoval(t.Context(), j); err == nil {
					t.Fatal("live removal bypassed the busy image fence", err)
				}
				imageInventoryUnavailable = true
				restarted, err := NewEngine(e.Root)
				if err != nil {
					t.Fatal(err)
				}
				restarted.Run = e.Run
				if err := restarted.Recover(); err != nil {
					t.Fatal("unstarted queued work blocked startup recovery", err)
				}
				status, err := restarted.Status(r.ID)
				if err != nil || status.RegistryRecoveryPending || status.ManagedRegistry == nil || status.ManagedRegistry.Cleanup == nil || status.ManagedRegistry.Cleanup.WorkerFilesRetained || !strings.Contains(status.ManagedRegistry.Cleanup.CleanupWarning, "Image retirement was deferred during worker startup") || removedTag {
					t.Fatal("startup did not preserve queued image references with a cleanup warning", status, err)
				}
				for _, path := range []string{dir, secretDir} {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatal("startup recovery left registry private files", path, err)
					}
				}
				queued, err := restarted.Status(waiting.ID)
				if err != nil || queued.State != "queued" || !exists {
					t.Fatal("recovery executed queued work or removed retained data", queued, err)
				}
				imageInventoryUnavailable = false
				if _, err := restarted.Cancel(waiting); err != nil {
					t.Fatal(err)
				}
				var jobs sync.WaitGroup
				if err := restarted.dispatch(t.Context(), &jobs); err != nil {
					t.Fatal(err)
				}
				jobs.Wait()
				if _, err := restarted.RemoveWorkload(t.Context(), id); err != nil || !removedTag || !exists {
					t.Fatal("record cleanup could not retire deferred tags after queued work settled", err)
				}
				return
			}
			if err := e.finishManagedRegistryRemoval(t.Context(), j); err != nil {
				t.Fatal(err)
			}
			if os.Getenv("COVE_TEST_REGISTRY_REMOVAL_NONROOT_ROOT") != "" {
				status, err := e.Status(r.ID)
				if err != nil || status.ManagedRegistry == nil || status.ManagedRegistry.Cleanup == nil || !status.ManagedRegistry.Cleanup.WorkerFilesRetained || status.ManagedRegistry.Cleanup.CleanupWarning == "" || removedTag {
					t.Fatal("unprivileged cleanup did not retain inaccessible state with a warning", status, err)
				}
				if _, err := os.Stat(filepath.Join(dir, "root-created")); err != nil {
					t.Fatal("inaccessible container files lost", err)
				}
				if _, err := os.Lstat(secretDir); !os.IsNotExist(err) {
					t.Fatal("worker-owned registry credentials remain", err)
				}
				return
			}
			for _, path := range []string{dir, secretDir} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Error("registry private worker files remain", path, err)
				}
			}
			if !removedTag || exists != (mode == "keep-data") {
				t.Error("generated tag or data volume cleanup incorrect", removedTag, exists)
			}
			if err := e.finishManagedRegistryRemoval(t.Context(), j); err != nil {
				t.Fatal("recovery replay is not idempotent", err)
			}
		})
	}
}

func TestDeletionMissingVolumeRequiresConfirmedAbsence(t *testing.T) {
	for _, mode := range []string{"absent", "present", "list-failed"} {
		t.Run(mode, func(t *testing.T) {
			e, dir, calls := deletionFixture(t, false, false)
			config := map[string]any{"services": map[string]any{"app": map[string]any{"volumes": []any{map[string]any{"type": "volume", "source": "data", "target": "/data"}}}}, "volumes": map[string]any{"data": map[string]any{"name": "cove-app_data"}}}
			if err := writeJSON(filepath.Join(dir, "release-states", "deployed.compose.json"), config); err != nil {
				t.Fatal(err)
			}
			normal := e.Run
			e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
				switch {
				case strings.HasPrefix(strings.Join(args, " "), "docker volume inspect"):
					return errors.New("fixture inspection unavailable")
				case strings.HasPrefix(strings.Join(args, " "), "docker volume ls"):
					if mode == "present" {
						_, err := io.WriteString(out, "cove-app_data")
						return err
					}
					if mode == "list-failed" {
						return errors.New("fixture list unavailable")
					}
					return nil
				default:
					return normal(ctx, dir, args, env, out)
				}
			}
			result, err := e.RemoveWorkload(t.Context(), "app")
			if err != nil {
				t.Fatal(err)
			}
			if result.WorkerFilesRetained != (mode != "absent") || (result.CleanupWarning != "") != (mode != "absent") {
				t.Fatal("volume inspection failure was mistaken for absence", result)
			}
			if mode != "absent" && strings.Contains(strings.Join(*calls, "\n"), "docker image rm") {
				t.Fatal("images removed without verified storage layout")
			}
		})
	}
}

func TestManagedRegistryPurgeDoesNotRecreatePrivateState(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "already-removed-registry-purge", Workload: strings.Repeat("a", 32), Action: "registry_maintenance", Kind: "service", ManagedRegistry: &ManagedRegistryOperation{Mode: "remove", DeleteData: true, Policy: ManagedRegistryRetention{KeepPerRepository: 1, MinimumAgeDays: 7}}}
	if _, err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	e.pending = nil
	e.Run = func(context.Context, string, []string, []string, io.Writer) error { return nil }
	if err := e.perform(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(e.Root, "workloads", r.Workload)); !os.IsNotExist(err) {
		t.Fatal("purging an already removed registry recreated its private worker directory", err)
	}
}
