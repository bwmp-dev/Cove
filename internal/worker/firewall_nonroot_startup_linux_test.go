package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHostFirewallNonRootStartupOnOwnedLinuxVM(t *testing.T) {
	if root := os.Getenv("COVE_TEST_FIREWALL_NONROOT_ROOT"); root != "" {
		if os.Geteuid() == 0 {
			t.Fatal("startup process must be unprivileged")
		}
		_, probe := os.Lstat(filepath.Join(root, "host-maintenance", "firewall-arming.json"))
		identity, _ := json.Marshal(struct {
			UID              int
			PermissionDenied bool
			SnapshotDenied   bool
		}{os.Geteuid(), os.IsPermission(probe), func() bool {
			_, err := os.Lstat(filepath.Join(snapshotScope(root), "index.json"))
			return os.IsPermission(err)
		}()})
		if err := os.WriteFile(filepath.Join(root, "process-identity.json"), identity, 0600); err != nil {
			t.Fatal(err)
		}
		if os.Getenv("COVE_TEST_FIREWALL_DENIED_DOCKER") == "1" {
			connection, err := net.DialTimeout("unix", "/var/run/docker.sock", time.Second)
			if err == nil {
				connection.Close()
			}
			if !errors.Is(err, os.ErrPermission) {
				t.Fatal("expected a real Docker socket permission refusal", err)
			}
		}
		e, err := NewEngine(root)
		if err != nil {
			t.Fatal(err)
		}
		e.hostHelperBinary = os.Getenv("COVE_TEST_FIREWALL_BINARY")
		if os.Getenv("COVE_TEST_FIREWALL_SNAPSHOT_HELPER") == "1" {
			if err := e.recoverFilesystemSnapshots(t.Context()); err == nil || !strings.Contains(err.Error(), "host snapshot helper failed") {
				t.Fatal("snapshot helper refusal was not reached", err)
			}
			t.Log("snapshot helper recovery refused before RPC")
		}
		if os.Getenv("COVE_TEST_FIREWALL_SNAPSHOT_PENDING") == "1" {
			pending, err := e.callSnapshotHost(t.Context(), snapshotHostRequest{Action: "pending", Workload: "orphan", Operation: "preview"})
			if err != nil || !pending.Pending {
				t.Fatal("privileged pending-snapshot inspection", pending, err)
			}
			handled, _, err := e.performFilesystemBackup(t.Context(), Request{ID: "preview", Workload: "orphan", Backup: &BackupConfig{VolumeSnapshot: &VolumeSnapshotConfig{}}}, nil, nil, deployedState{}, &bytes.Buffer{})
			if !handled || err == nil || !strings.Contains(err.Error(), "recover the pending filesystem snapshot") {
				t.Fatal("pending snapshot allowed a backup fallback", handled, err)
			}
		}
		ctx, cancel := signal.NotifyContext(t.Context(), syscall.SIGTERM, os.Interrupt)
		defer cancel()
		if err := e.Listen(ctx, filepath.Join(root, "run", "worker.sock")); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("worker startup refused: %v", err)
		}
		return
	}
	binary := os.Getenv("COVE_TEST_FIREWALL_BINARY")
	if os.Getenv("COVE_TEST_OWNED_HOST_INSPECTION") != "1" || os.Geteuid() != 0 || binary == "" {
		t.Skip("explicit disposable root Linux VM, systemd, Docker and worker binary required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	accountID, err := firewallChangeID()
	if err != nil {
		t.Fatal(err)
	}
	account := "cove-fw-" + accountID[:12]
	if _, err := snapshotHostCommand(ctx, "/usr/sbin/useradd", "--system", "--user-group", "--no-create-home", "--shell", "/usr/sbin/nologin", account); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer stop()
		if _, err := snapshotHostCommand(cleanup, "/usr/sbin/userdel", account); err != nil {
			t.Error(err)
		}
	})
	identity, err := user.Lookup(account)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.Atoi(identity.Uid)
	gid, _ := strconv.Atoi(identity.Gid)
	if uid == 0 || gid == 0 {
		t.Fatal("fixture account must not be root")
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name            string
		scope           bool
		marker          bool
		denied          bool
		snapshots       bool
		pendingSnapshot bool
	}{
		{name: "directory-absent"},
		{name: "root-only-directory", scope: true},
		{name: "root-only-marker", scope: true, marker: true},
		{name: "helper-failure-without-marker", scope: true, denied: true},
		{name: "helper-failure-with-marker", scope: true, marker: true, denied: true},
		{name: "root-only-snapshot-directory", snapshots: true},
		{name: "root-only-snapshot-index", snapshots: true, pendingSnapshot: true},
		{name: "snapshot-helper-failure", snapshots: true, denied: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root, err := os.MkdirTemp("/var/lib", "cove-firewall-startup-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(root); err != nil {
					t.Error(err)
				}
			})
			if err := os.Chown(root, uid, gid); err != nil {
				t.Fatal(err)
			}
			owner, err := firewallChangeID()
			if err != nil {
				t.Fatal(err)
			}
			ownerFile := filepath.Join(root, "backup-helper-owner.json")
			if err := writeJSON(ownerFile, struct{ ID string }{owner}); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(ownerFile, uid, gid); err != nil {
				t.Fatal(err)
			}
			scope := filepath.Join(root, "host-maintenance")
			if scenario.scope {
				if _, err := runHostFirewall(ctx, root, owner, FirewallControl{Action: "inspect"}, snapshotHostCommand); err != nil {
					t.Fatal("plain host inspection", err)
				}
				for _, filename := range []string{scope, filepath.Join(scope, "owner"), filepath.Join(scope, "worker.lock")} {
					info, err := os.Lstat(filename)
					if err != nil {
						t.Fatal(err)
					}
					owner, _ := fileOwner(info)
					if owner != 0 || info.Mode().Perm()&0077 != 0 {
						t.Fatal("helper state must really be root-only", filename, info.Mode())
					}
				}
			}
			checkpoint := filepath.Join(scope, "firewall-arming.json")
			if scenario.snapshots {
				if err := os.Mkdir(snapshotScope(root), 0700); err != nil {
					t.Fatal(err)
				}
				index := snapshotHostIndex{Owner: owner, Workloads: []string{}}
				if scenario.pendingSnapshot {
					index.Workloads = append(index.Workloads, "orphan")
				}
				if err := writeJSON(filepath.Join(snapshotScope(root), "index.json"), index); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.marker {
				data, err := os.ReadFile(binary)
				if err != nil {
					t.Fatal(err)
				}
				helper := snapshotHostBinary(root)
				if err := os.WriteFile(helper, data, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chown(helper, uid, gid); err != nil {
					t.Fatal(err)
				}
				preview, err := runHostFirewall(ctx, root, owner, FirewallControl{Action: "preview", Reserved: []uint16{22}}, snapshotHostCommand)
				if err != nil || !preview.Supported {
					t.Fatal("real firewall preview", preview.Notice, err)
				}
				armed := false
				fault := func(call context.Context, args ...string) (string, error) {
					if args[0] == "/usr/bin/systemd-run" {
						out, err := snapshotHostCommand(call, args...)
						if err != nil {
							return out, err
						}
						armed = true
						return out, errors.New("injected lost arming acknowledgement")
					}
					if armed && args[0] == "/usr/bin/systemctl" && args[1] == "stop" {
						return "", errors.New("injected cleanup refusal")
					}
					return snapshotHostCommand(call, args...)
				}
				if _, err := runHostFirewall(ctx, root, owner, FirewallControl{Action: "apply", Revision: preview.Revision, Reserved: []uint16{22}}, fault); err == nil || !armed {
					t.Fatal("real arming/lost-acknowledgement boundary was not reached", err)
				}
				t.Cleanup(func() {
					cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
					defer stop()
					if _, err := runHostFirewall(cleanup, root, owner, FirewallControl{Action: "recover"}, snapshotHostCommand); err != nil {
						t.Error("owned recovery cleanup", err)
					}
				})
				if _, err := os.Stat(checkpoint); err != nil {
					t.Fatal("arming marker missing", err)
				}
			}
			run := func(denied bool) {
				unit := "cove-fw-startup-" + owner
				if denied {
					unit += "-denied"
				} else {
					unit += "-ready"
				}
				args := []string{"/usr/bin/systemd-run", "--unit=" + unit, "--collect", "--property=User=" + account, "--property=Group=" + account, "--property=ProtectSystem=strict", "--property=NoNewPrivileges=true", "--property=ReadWritePaths=" + root, "--setenv=COVE_TEST_FIREWALL_NONROOT_ROOT=" + root, "--setenv=COVE_TEST_FIREWALL_BINARY=" + binary}
				if scenario.pendingSnapshot {
					args = append(args, "--setenv=COVE_TEST_FIREWALL_SNAPSHOT_PENDING=1")
				}
				if denied {
					args = append(args, "--setenv=COVE_TEST_FIREWALL_DENIED_DOCKER=1")
					if scenario.snapshots {
						args = append(args, "--setenv=COVE_TEST_FIREWALL_SNAPSHOT_HELPER=1")
					}
				} else {
					args = append(args, "--property=SupplementaryGroups=docker")
				}
				args = append(args, testBinary, "-test.run=^TestHostFirewallNonRootStartupOnOwnedLinuxVM$", "-test.v", "-test.timeout=90s")
				before, _ := os.ReadFile(checkpoint)
				if _, err := snapshotHostCommand(ctx, args...); err != nil {
					t.Fatal("start non-root process", err)
				}
				defer func() {
					cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
					defer stop()
					load, err := snapshotHostCommand(cleanup, "/usr/bin/systemctl", "show", unit+".service", "--property=LoadState", "--value")
					if err == nil && strings.TrimSpace(load) == "not-found" {
						return
					}
					_, stopErr := snapshotHostCommand(cleanup, "/usr/bin/systemctl", "stop", unit+".service")
					state, stateErr := snapshotHostCommand(cleanup, "/usr/bin/systemctl", "show", unit+".service", "--property=ActiveState,MainPID")
					inactive := strings.Contains(state, "ActiveState=inactive") || denied && strings.Contains(state, "ActiveState=failed")
					if stateErr != nil || !inactive || !strings.Contains(state, "MainPID=0") {
						t.Error("owned startup unit did not stop", stopErr, stateErr, state)
					} else if stopErr != nil {
						t.Log("stop command returned an error; verified the owned unit is inactive with no main process", stopErr)
					}
				}()
				until := time.Now().Add(80 * time.Second)
				socket := filepath.Join(root, "run", "worker.sock")
				for {
					connection, dialErr := net.DialTimeout("unix", socket, 50*time.Millisecond)
					if dialErr == nil {
						connection.Close()
						if denied {
							t.Fatal("RPC opened without confirming privileged recovery")
						}
						if _, err := os.Lstat(checkpoint); !os.IsNotExist(err) {
							t.Fatal("RPC opened before arming recovery completed", err)
						}
						if scenario.snapshots {
							index, err := snapshotIndex(root, owner)
							if err != nil || len(index.Workloads) != 0 {
								t.Fatal("RPC opened before privileged snapshot recovery", index, err)
							}
						}
						break
					}
					state, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "show", unit+".service", "--property=ActiveState", "--value")
					if err != nil || strings.TrimSpace(state) == "failed" || strings.TrimSpace(state) == "inactive" {
						log, _ := snapshotHostCommand(ctx, "/usr/bin/journalctl", "--no-pager", "-u", unit+".service", "-n", "40")
						expectedError := "firewall arming recovery"
						if scenario.snapshots {
							expectedError = "worker startup refused: exit status 1"
							if !strings.Contains(log, "snapshot helper recovery refused before RPC") {
								t.Fatal("snapshot helper refusal was not confirmed", log)
							}
						}
						if !denied || !strings.Contains(log, expectedError) {
							t.Fatal("unexpected non-root startup failure", err, state, log)
						}
						after, _ := os.ReadFile(checkpoint)
						if !bytes.Equal(before, after) {
							t.Fatal("failed helper lost its arming checkpoint")
						}
						break
					}
					if time.Now().After(until) || ctx.Err() != nil {
						t.Fatal("non-root startup exceeded its bound")
					}
					time.Sleep(100 * time.Millisecond)
				}
				var proof struct {
					UID              int
					PermissionDenied bool
					SnapshotDenied   bool
				}
				if err := readSnapshotMetadata(filepath.Join(root, "process-identity.json"), &proof); err != nil || proof.UID != uid || proof.PermissionDenied != scenario.scope || proof.SnapshotDenied != scenario.snapshots {
					t.Fatal("real non-root filesystem layout not reproduced", proof, err)
				}
				info, err := os.Stat(root)
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatal("worker state directory permissions changed", err)
				}
				if scenario.scope {
					info, err := os.Stat(scope)
					if err != nil || info.Mode().Perm() != 0700 {
						t.Fatal("helper directory permissions weakened", err)
					}
					owner, _ := fileOwner(info)
					if owner != 0 {
						t.Fatal("helper directory ownership weakened")
					}
				}
				t.Logf("unit=%s.service uid=%d ProtectSystem=strict helper_denied=%v RPC_ready=%v", unit, proof.UID, denied, !denied)
			}
			if scenario.denied {
				run(true)
			}
			run(false)
			if scenario.marker {
				for _, suffix := range []string{".timer", ".service"} {
					state, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "show", firewallUnit(owner)+suffix, "--property=LoadState", "--value")
					if err != nil || strings.TrimSpace(state) != "not-found" {
						t.Fatal("startup left an orphan recovery unit", suffix, state, err)
					}
				}
				if _, err := os.Lstat(firewallDeadlineFile(owner)); !os.IsNotExist(err) {
					t.Fatal("startup left the recovery deadline", err)
				}
			}
		})
	}
}
