package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFilesystemSnapshotRealHost(t *testing.T) {
	if os.Getenv("COVE_TEST_FILESYSTEM_SNAPSHOTS") != "1" {
		t.Skip("COVE_TEST_FILESYSTEM_SNAPSHOTS=1 required; provisions owned loopback filesystems on a disposable Linux host")
	}
	if os.Geteuid() != 0 {
		t.Fatal("requires an isolated root test host")
	}
	binary := os.Getenv("COVE_TEST_WORKER_BINARY")
	if binary == "" {
		t.Fatal("COVE_TEST_WORKER_BINARY required")
	}
	for _, kind := range []string{"btrfs", "lvm", "zfs"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
			defer cancel()
			root := t.TempDir()
			mount := filepath.Join(root, "mount")
			if err := os.Mkdir(mount, 0700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(root, "disk.img")
			disk, err := os.Create(file)
			if err != nil {
				t.Fatal(err)
			}
			if err = disk.Truncate(2 << 30); err != nil {
				t.Fatal(err)
			}
			disk.Close()
			run := func(args ...string) string {
				t.Helper()
				out, err := snapshotHostCommand(ctx, args...)
				if err != nil {
					t.Fatal(strings.Join(args, " "), err)
				}
				return strings.TrimSpace(out)
			}
			digest := sha256.Sum256([]byte(root))
			name := fmt.Sprintf("covefstest%x", digest[:8])
			loop := ""
			mounted := false
			pool := false
			vg := false
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				command := func(args ...string) {
					if _, err := snapshotHostCommand(cleanup, args...); err != nil {
						t.Error("fixture cleanup", args, err)
					}
				}
				if pool {
					command("zpool", "destroy", name)
				}
				if mounted {
					command("umount", mount)
				}
				if vg {
					command("vgremove", "--force", name)
					command("pvremove", "--force", loop)
				}
				if loop != "" {
					backing, err := snapshotHostCommand(cleanup, "losetup", "--noheadings", "--output", "BACK-FILE", loop)
					if err != nil || strings.TrimSpace(backing) != file {
						t.Error("owned loop backing changed; not detaching")
					} else {
						command("losetup", "--detach", loop)
					}
				}
			}()
			switch kind {
			case "btrfs":
				run("modprobe", "btrfs")
				run("mkfs.btrfs", "-f", file)
				loop = run("losetup", "--find", "--show", file)
				run("mount", "-t", "btrfs", loop, mount)
				mounted = true
				run("btrfs", "subvolume", "create", filepath.Join(mount, "data"))
			case "lvm":
				run("modprobe", "dm_snapshot")
				loop = run("losetup", "--find", "--show", file)
				run("pvcreate", loop)
				run("vgcreate", name, loop)
				vg = true
				run("lvcreate", "--size", "512M", "--name", "data", name)
				run("mkfs.ext4", "-F", "/dev/"+name+"/data")
				run("mount", "/dev/"+name+"/data", mount)
				mounted = true
				if err := os.Mkdir(filepath.Join(mount, "data"), 0700); err != nil {
					t.Fatal(err)
				}
			case "zfs":
				run("modprobe", "zfs")
				run("zpool", "create", "-f", "-m", mount, name, file)
				pool = true
				if err := os.Mkdir(filepath.Join(mount, "data"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			data := filepath.Join(mount, "data")
			if err := os.WriteFile(filepath.Join(data, "marker"), []byte("before"), 0600); err != nil {
				t.Fatal(err)
			}
			e, err := NewEngine(filepath.Join(root, "worker"))
			if err != nil {
				t.Fatal(err)
			}
			e.hostHelperBinary = binary
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				owner, err := e.output(cleanup, "docker", "volume", "inspect", "--format", `{{index .Labels "cove.backup"}}`, "cove-backups-"+name)
				if err != nil {
					return
				}
				if owner != name {
					t.Error("repository fixture ownership changed; preserving volume")
					return
				}
				if err := e.Run(cleanup, e.Root, []string{"docker", "volume", "rm", "cove-backups-" + name}, nil, io.Discard); err != nil {
					t.Error("owned repository cleanup", err)
				}
			}()
			workload := name
			container := name
			if _, err = e.output(ctx, "docker", "run", "-d", "--name", container, "--label", "com.docker.compose.project=cove-"+workload, "--mount", "type=bind,source="+data+",target=/data", "busybox:1.37", "sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 1; done"); err != nil {
				t.Fatal(err)
			}
			defer func() {
				c, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if err := e.Run(c, e.Root, []string{"docker", "rm", "-f", container}, nil, io.Discard); err != nil {
					t.Error(err)
				}
			}()
			dir := filepath.Join(e.Root, "workloads", workload)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			current := deployedState{}
			if err := writeJSON(filepath.Join(dir, "current.json"), current); err != nil {
				t.Fatal(err)
			}
			r := Request{ID: "backup", Workload: workload, Action: "backup", Backup: &BackupConfig{Repository: "local", Password: "synthetic-filesystem-backup-password", KeepLast: 2, VolumeSnapshot: &VolumeSnapshotConfig{MaxBytes: 256 << 20}}}
			preview, err := e.SnapshotPreview(ctx, workload, 256<<20)
			if err != nil || !preview.Available {
				t.Fatal("snapshot preview", preview, err)
			}
			started := run("docker", "inspect", "--format", "{{.State.StartedAt}}", container)
			if err := e.performBackup(ctx, r, os.Stdout); err != nil {
				t.Fatal("backup", err)
			}
			if after := run("docker", "inspect", "--format", "{{.State.StartedAt}}", container); after != started {
				t.Fatal("backup restarted source")
			}
			var manifest backupManifest
			if err := readSnapshotMetadata(filepath.Join(dir, "backups", "backup.json"), &manifest); err != nil || manifest.Record.VolumeMethod != kind {
				t.Fatal("snapshot publication", manifest.Record, err)
			}
			if err := os.WriteFile(filepath.Join(data, "marker"), []byte("after"), 0600); err != nil {
				t.Fatal(err)
			}
			r.ID, r.Action, r.BackupID = "restore", "restore", "backup"
			if err := e.performBackup(ctx, r, os.Stdout); err != nil {
				t.Fatal("restore", err)
			}
			value, err := os.ReadFile(filepath.Join(data, "marker"))
			if err != nil || string(value) != "before" {
				t.Fatal("restore marker", string(value), err)
			}
			mounts, running, err := e.backupMounts(ctx, workload)
			if err != nil {
				t.Fatal(err)
			}
			r.ID = "interrupted"
			request := snapshotRequest(r, mounts, running)
			request.Action = "create"
			if _, err := e.callSnapshotHost(ctx, request); err != nil {
				t.Fatal("interrupted snapshot", err)
			}
			owner, err := e.backupHelperOwner(false)
			if err != nil {
				t.Fatal(err)
			}
			interrupted, err := readSnapshotState(e.Root, owner, workload)
			if err != nil {
				t.Fatal(err)
			}
			run("docker", "pause", container)
			interrupted.Paused = true
			if err := writeJSON(snapshotStateFile(e.Root, workload), interrupted); err != nil {
				t.Fatal(err)
			}
			fresh := &Engine{Root: e.Root, Run: RunCommand, hostHelperBinary: binary}
			if err := fresh.recoverFilesystemSnapshots(ctx); err != nil {
				t.Fatal("restart recovery", err)
			}
			if _, err := os.Stat(snapshotStateFile(e.Root, workload)); !os.IsNotExist(err) {
				t.Fatal("restart retained snapshot", err)
			}
			request.Operation = "expiry"
			if _, err := e.callSnapshotHost(ctx, request); err != nil {
				t.Fatal(err)
			}
			state, err := readSnapshotState(e.Root, owner, workload)
			if err != nil {
				t.Fatal(err)
			}
			state.CreatedAt = time.Now().Add(-time.Hour)
			state.ExpiresAt = state.CreatedAt.Add(snapshotLease)
			if err := writeJSON(snapshotStateFile(e.Root, workload), state); err != nil {
				t.Fatal(err)
			}
			run("systemctl", "start", snapshotUnitName(owner)+".service")
			if _, err := os.Stat(snapshotStateFile(e.Root, workload)); !os.IsNotExist(err) {
				t.Fatal("host timer did not recover snapshot", err)
			}
			if paused := run("docker", "inspect", "--format", "{{.State.Paused}}", container); paused != "false" {
				t.Fatal("source remained paused")
			}
			if kind == "lvm" {
				r.ID, r.Action, r.BackupID = "overflow", "backup", ""
				r.Backup.VolumeSnapshot.MaxBytes = 64 << 20
				before := run("docker", "inspect", "--format", "{{.State.StartedAt}}", container)
				e.Run = func(c context.Context, dir string, args, environment []string, out io.Writer) error {
					err := RunCommand(c, dir, args, environment, out)
					if err == nil && strings.Contains(strings.Join(args, " "), "cove-operation:overflow") && strings.Contains(strings.Join(args, " "), " backup ") {
						_, err = snapshotHostCommand(c, "dd", "if=/dev/zero", "of="+filepath.Join(data, "overflow-write"), "bs=1M", "count=96", "conv=fsync")
					}
					return err
				}
				err := e.performBackup(ctx, r, os.Stdout)
				e.Run = RunCommand
				if err == nil {
					t.Fatal("overflowed LVM snapshot was published")
				}
				if _, err := os.Stat(filepath.Join(dir, "backups", "overflow.json")); !os.IsNotExist(err) {
					t.Fatal("overflow backup record retained", err)
				}
				if after := run("docker", "inspect", "--format", "{{.State.StartedAt}}", container); after != before {
					t.Fatal("failed online backup restarted source")
				}
				index, err := snapshotIndex(e.Root, owner)
				if err != nil || len(index.Workloads) != 0 {
					t.Fatal("overflow left kernel recovery state", index, err)
				}
				t.Log("Actual LVM COW overflow rejected publication and retained the running source")
			}
			fmt.Println("Verified", kind, "online encrypted backup, exact restore, startup recovery and independent host recovery")
			encoded, _ := json.Marshal(manifest.Record)
			t.Log(string(encoded))
		})
	}
}
