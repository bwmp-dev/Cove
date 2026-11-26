package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func deletionFixture(t *testing.T, userNamed, shared bool) (*Engine, string, *[]string) {
	t.Helper()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := e.buildOwner()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", "app")
	snapshot := filepath.Join(dir, "release-states", "deployed.compose.json")
	if err := os.MkdirAll(filepath.Dir(snapshot), 0700); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(snapshot, map[string]any{"services": map[string]any{"app": map[string]any{"image": "sha256:" + strings.Repeat("a", 64)}}}); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(dir, "current.json"), deployedState{ComposePath: snapshot}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "disposable.log"), []byte("remove"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	own := "cove-app-app:latest"
	if userNamed {
		own = "user-chosen:fixture"
	}
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		command := strings.Join(args, " ")
		calls = append(calls, command)
		switch {
		case strings.HasPrefix(command, "docker ps --all"):
			_, err := io.WriteString(out, `{"ID":"owned-container","State":"exited"}`)
			return err
		case strings.HasPrefix(command, "docker inspect"):
			return json.NewEncoder(out).Encode(map[string]any{"Id": "owned-container", "Mounts": []any{}, "Config": map[string]any{"Labels": map[string]string{"com.docker.compose.project": "cove-app", "com.docker.compose.service": "app"}}})
		case strings.HasPrefix(command, "docker image ls"):
			_, err := fmt.Fprintln(out, "sha256:"+strings.Repeat("a", 64))
			return err
		case strings.HasPrefix(command, "docker image inspect"):
			return json.NewEncoder(out).Encode(map[string]any{"Id": "sha256:" + strings.Repeat("a", 64), "RepoTags": []string{own, "cove-release/app:deployed-fixture"}, "Config": map[string]any{"Labels": map[string]string{"dev.cove.build-owner": owner, "com.docker.compose.project": "cove-app", "com.docker.compose.service": "app"}}})
		case strings.HasPrefix(command, "docker ps -aq"):
			if shared {
				_, err := io.WriteString(out, "other-running-container")
				return err
			}
			return nil
		default:
			return nil
		}
	}
	return e, dir, &calls
}

func TestDeletionRemovesUnreferencedStateAndGeneratedBuildTags(t *testing.T) {
	e, dir, calls := deletionFixture(t, false, false)
	if _, err := e.RemoveWorkload(t.Context(), "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Error("deleted resource state remains", err)
	}
	joined := strings.Join(*calls, "\n")
	for _, tag := range []string{"cove-app-app:latest", "cove-release/app:deployed-fixture"} {
		if !strings.Contains(joined, "docker image rm "+tag) {
			t.Error("generated tag not retired", tag)
		}
	}
	if strings.Contains(joined, "image rm --force") || strings.Contains(joined, "volume rm") {
		t.Fatal("unsafe storage deletion", joined)
	}
}

func TestDeletionKeepsBackupRecoverySubset(t *testing.T) {
	e, dir, _ := deletionFixture(t, false, false)
	snapshot := filepath.Join(dir, "release-states", "deployed.compose.json")
	manifest := backupManifest{Record: BackupRecord{ID: "backup", Workload: "app", Repository: "local", Snapshot: strings.Repeat("a", 64)}, State: deployedState{ComposePath: snapshot}}
	if err := os.MkdirAll(filepath.Join(dir, "backups"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "backups", "backup.json"), manifest); err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(dir, "compose-bundles", "retained", "asset.txt")
	if err := os.MkdirAll(filepath.Dir(asset), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, []byte("backup asset"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(snapshot, map[string]any{"services": map[string]any{"app": map[string]any{"image": "sha256:" + strings.Repeat("a", 64), "volumes": []any{map[string]any{"type": "bind", "source": asset, "read_only": true, "target": "/config"}}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RemoveWorkload(t.Context(), "app"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{snapshot, asset, filepath.Join(dir, "backups", "backup.json")} {
		if _, err := os.Lstat(path); err != nil {
			t.Error("recovery metadata removed", path, err)
		}
	}
	for _, path := range []string{filepath.Join(dir, "current.json"), filepath.Join(dir, "disposable.log")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Error("non-recovery state remains", path, err)
		}
	}
}

func TestDeletionKeepsUserNamedAndSharedImages(t *testing.T) {
	for _, scenario := range []string{"user named", "shared"} {
		t.Run(scenario, func(t *testing.T) {
			e, dir, calls := deletionFixture(t, scenario == "user named", scenario == "shared")
			result, err := e.RemoveWorkload(t.Context(), "app")
			if err != nil || result.WorkerFilesRetained {
				t.Fatal(result, err)
			}
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Fatal("unreferenced state retained", err)
			}
			commands := strings.Join(*calls, "\n")
			if strings.Contains(commands, "docker image rm user-chosen:fixture") {
				t.Fatal("user image tag removed", commands)
			}
			if scenario == "shared" && strings.Contains(commands, "docker image rm ") {
				t.Fatal("shared image changed", commands)
			}
		})
	}
}

func TestDeletionPreservesBindDataIncludingServicesWithoutContainers(t *testing.T) {
	for _, scenario := range []string{"container bind", "snapshot bind", "local volume bind"} {
		t.Run(scenario, func(t *testing.T) {
			e, dir, _ := deletionFixture(t, false, false)
			data := filepath.Join(dir, "user-data")
			if err := os.MkdirAll(data, 0700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(data, "keep")
			if err := os.WriteFile(sentinel, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			snapshot := filepath.Join(dir, "release-states", "deployed.compose.json")
			if scenario == "snapshot bind" {
				if err := writeJSON(snapshot, map[string]any{"services": map[string]any{"scaled-zero": map[string]any{"image": "busybox:1.37", "scale": 0, "volumes": []any{map[string]any{"type": "bind", "source": data, "target": "/data"}}}}}); err != nil {
					t.Fatal(err)
				}
			}
			original := e.Run
			e.Run = func(ctx context.Context, cwd string, args, env []string, out io.Writer) error {
				command := strings.Join(args, " ")
				if scenario == "snapshot bind" && strings.HasPrefix(command, "docker ps --all") {
					return nil
				}
				if strings.HasPrefix(command, "docker inspect") {
					mount := map[string]string{"Type": "bind", "Source": data}
					if scenario == "snapshot bind" {
						return json.NewEncoder(out).Encode(map[string]any{"Id": "owned-container", "Mounts": []any{}})
					}
					if scenario == "local volume bind" {
						mount = map[string]string{"Type": "volume", "Name": "owned-data"}
					}
					return json.NewEncoder(out).Encode(map[string]any{"Id": "owned-container", "Mounts": []any{mount}})
				}
				if strings.HasPrefix(command, "docker volume inspect") {
					return json.NewEncoder(out).Encode(map[string]any{"Driver": "local", "Options": map[string]string{"device": data, "o": "bind", "type": "none"}})
				}
				return original(ctx, cwd, args, env, out)
			}
			result, err := e.RemoveWorkload(t.Context(), "app")
			if err != nil || !result.WorkerFilesRetained || result.CleanupWarning != "" {
				t.Fatal(result, err)
			}
			if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "untouched" {
				t.Fatal("bind data changed", err)
			}
			if _, err := os.Lstat(filepath.Join(dir, "disposable.log")); !os.IsNotExist(err) {
				t.Fatal("disposable state retained", err)
			}
		})
	}
}

func TestDeletionUncertainLayoutKeepsTreeAndReportsWarning(t *testing.T) {
	for _, scenario := range []string{"missing snapshot", "unresolved bind", "inspection error", "restore-copy checkpoint"} {
		t.Run(scenario, func(t *testing.T) {
			e, dir, calls := deletionFixture(t, false, false)
			snapshot := filepath.Join(dir, "release-states", "deployed.compose.json")
			switch scenario {
			case "restore-copy checkpoint":
				if err := writeJSON(filepath.Join(dir, "copy-recovery.json"), map[string]string{"operation": "copy"}); err != nil {
					t.Fatal(err)
				}
			case "missing snapshot":
				if err := os.Remove(snapshot); err != nil {
					t.Fatal(err)
				}
			case "unresolved bind":
				if err := writeJSON(snapshot, map[string]any{"services": map[string]any{"app": map[string]any{"volumes": []any{map[string]any{"type": "bind", "source": "${UNRESOLVED}", "target": "/data"}}}}}); err != nil {
					t.Fatal(err)
				}
			case "inspection error":
				original := e.Run
				e.Run = func(ctx context.Context, cwd string, args, env []string, out io.Writer) error {
					if args[1] == "inspect" {
						return fmt.Errorf("fixture inspect failed")
					}
					return original(ctx, cwd, args, env, out)
				}
			}
			result, err := e.RemoveWorkload(t.Context(), "app")
			if err != nil || !result.WorkerFilesRetained || result.CleanupWarning == "" {
				t.Fatal(result, err)
			}
			if _, err := os.Lstat(filepath.Join(dir, "disposable.log")); err != nil {
				t.Fatal("uncertain tree changed", err)
			}
			if strings.Contains(strings.Join(*calls, "\n"), "docker image rm ") {
				t.Fatal("uncertain images changed")
			}
		})
	}
}

func TestDeletionRefusalsPreserveStateAndNeverForce(t *testing.T) {
	for _, scenario := range []string{"pending storage recovery", "pending hook recovery", "another operation", "tag changed", "Docker removal refused"} {
		t.Run(scenario, func(t *testing.T) {
			e, dir, calls := deletionFixture(t, false, false)
			switch scenario {
			case "pending storage recovery":
				if err := writeJSON(filepath.Join(dir, "storage-recovery.json"), map[string]bool{"restoring": true}); err != nil {
					t.Fatal(err)
				}
			case "pending hook recovery":
				if err := writeJSON(filepath.Join(dir, "pending-hook.json"), "unfinished-hook"); err != nil {
					t.Fatal(err)
				}
			case "another operation":
				e.active["other"] = true
			default:
				original := e.Run
				inspections := 0
				e.Run = func(ctx context.Context, cwd string, args, env []string, out io.Writer) error {
					command := strings.Join(args, " ")
					if strings.HasPrefix(command, "docker image inspect") {
						inspections++
						if scenario == "tag changed" && inspections > 1 {
							return json.NewEncoder(out).Encode(map[string]string{"Id": "sha256:" + strings.Repeat("b", 64)})
						}
					}
					if scenario == "Docker removal refused" && strings.HasPrefix(command, "docker image rm") {
						return fmt.Errorf("fixture container now uses this image")
					}
					return original(ctx, cwd, args, env, out)
				}
			}
			if result, err := e.RemoveWorkload(t.Context(), "app"); err == nil {
				t.Fatal("unsafe deletion succeeded", scenario, result)
			}
			if _, err := os.Lstat(filepath.Join(dir, "disposable.log")); err != nil {
				t.Fatal("refusal destroyed state", err)
			}
			if e.active[housekeepingWorkload] || e.active["app"] {
				t.Fatal("removal lock leaked")
			}
			if strings.Contains(strings.Join(*calls, "\n"), "image rm --force") || strings.Contains(strings.Join(*calls, "\n"), "volume rm") {
				t.Fatal("unsafe force/volume cleanup")
			}
		})
	}
}
