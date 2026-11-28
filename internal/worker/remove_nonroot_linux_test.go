package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestDeletionPermissionsOnOwnedLinux(t *testing.T) {
	if root := os.Getenv("COVE_TEST_DELETION_NONROOT_ROOT"); root != "" {
		if os.Geteuid() == 0 {
			t.Fatal("deletion must run unprivileged")
		}
		e, dir, calls := deletionFixtureRoot(t, root, false, false)
		blocked := filepath.Join(dir, "root-created")
		scenario := os.Getenv("COVE_TEST_DELETION_NONROOT_CASE")
		if scenario == "root-only-backups" {
			blocked = filepath.Join(dir, "backups")
		}
		if scenario == "known-bind" {
			if err := writeJSON(filepath.Join(dir, "release-states", "deployed.compose.json"), map[string]any{"services": map[string]any{"app": map[string]any{"volumes": []any{map[string]any{"type": "bind", "source": blocked, "target": "/data"}}}}}); err != nil {
				t.Fatal(err)
			}
		}
		if scenario == "late-denial" {
			normal := e.Run
			e.Run = func(ctx context.Context, cwd string, args, env []string, out io.Writer) error {
				if strings.HasPrefix(strings.Join(args, " "), "docker network ls") {
					if err := os.Chmod(blocked, 0500); err != nil {
						return err
					}
				}
				return normal(ctx, cwd, args, env, out)
			}
		}
		if scenario == "root-only" || scenario == "known-bind" || scenario == "root-only-backups" {
			if _, err := os.ReadDir(blocked); !os.IsPermission(err) {
				t.Fatal("fixture did not establish a real permission denial", err)
			}
		}
		result, err := e.RemoveWorkload(t.Context(), "app")
		if err != nil || result == nil || scenario != "root-owned-file" && (!result.WorkerFilesRetained || scenario != "known-bind" && result.CleanupWarning == "") {
			t.Fatal("permission denial failed deletion instead of retaining files", result, err)
		}
		if scenario != "known-bind" && scenario != "late-denial" && scenario != "root-owned-file" && strings.Contains(strings.Join(*calls, "\n"), "docker image rm ") {
			t.Fatal("images removed despite known cleanup denial")
		}
		if err := writeJSON(filepath.Join(root, "nonroot-result.json"), map[string]any{"uid": os.Geteuid(), "result": result}); err != nil {
			t.Fatal(err)
		}
		return
	}
	if os.Getenv("COVE_TEST_DELETION_NONROOT") != "1" || os.Geteuid() != 0 {
		t.Skip("set COVE_TEST_DELETION_NONROOT=1 in an owned root Linux fixture to launch an unprivileged deletion process")
	}
	for _, scenario := range []string{"root-only", "root-writable-only", "known-bind", "late-denial", "root-owned-parent", "root-only-backups", "root-owned-file"} {
		t.Run(scenario, func(t *testing.T) {
			root, err := os.MkdirTemp("", "cove-removal-nonroot-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(root); err != nil {
					t.Error(err)
				}
			})
			_, dir, _ := deletionFixtureRoot(t, root, false, false)
			blocked := filepath.Join(dir, "root-created")
			if scenario == "root-only-backups" {
				blocked = filepath.Join(dir, "backups")
			}
			if err := os.Mkdir(blocked, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(blocked, "value"), []byte("container-owned data"), 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "known-bind" {
				if err := writeJSON(filepath.Join(dir, "release-states", "deployed.compose.json"), map[string]any{"services": map[string]any{"app": map[string]any{"volumes": []any{map[string]any{"type": "bind", "source": blocked, "target": "/data"}}}}}); err != nil {
					t.Fatal(err)
				}
			}
			before := map[string]string{}
			if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if info.Mode().IsRegular() && strings.HasPrefix(path, dir+string(filepath.Separator)) {
					raw, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					before[path] = string(raw)
				}
				return os.Chown(path, 65534, 65534)
			}); err != nil {
				t.Fatal(err)
			}
			if scenario == "root-owned-file" {
				if err := os.RemoveAll(blocked); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(blocked, []byte("root-owned scratch file"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "root-owned-parent" {
				if err := os.Chown(filepath.Dir(dir), 0, 0); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Dir(dir), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "late-denial" {
				if err := os.Chown(blocked, 0, 0); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "root-writable-only" {
				if err := os.Chmod(blocked, 0755); err != nil {
					t.Fatal(err)
				}
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), binary, "-test.v", "-test.run=^TestDeletionPermissionsOnOwnedLinux$", "-test.timeout=30s")
			command.Env = append(os.Environ(), "COVE_TEST_DELETION_NONROOT_ROOT="+root, "COVE_TEST_DELETION_NONROOT_CASE="+scenario)
			command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("real unprivileged process failed: %v\n%s", err, output)
			}
			var result struct {
				UID int `json:"uid"`
			}
			raw, err := os.ReadFile(filepath.Join(root, "nonroot-result.json"))
			if err != nil || json.Unmarshal(raw, &result) != nil || result.UID != 65534 {
				t.Fatal("unprivileged deletion receipt missing", err)
			}
			if scenario == "root-owned-file" {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatal("deletable root-owned file was unnecessarily retained", err)
				}
				return
			}
			for path, value := range before {
				if scenario == "known-bind" && !strings.HasPrefix(path, blocked+string(filepath.Separator)) {
					continue
				}
				if raw, err := os.ReadFile(path); err != nil || string(raw) != value {
					t.Fatal("permission preflight left partially removed state or changed user data", path, err)
				}
			}
			t.Log("deletion completed as UID 65534; root-created data retained")
		})
	}
}
