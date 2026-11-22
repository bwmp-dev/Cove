package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRemoteBackupMetadataRecovery(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh worker", true: "foreign snapshot"}[foreign], func(t *testing.T) {
			root := t.TempDir()
			pulled := false
			request := Request{ID: "restore", Workload: "resource", BackupID: "backup", Backup: &BackupConfig{Repository: "s3:https://example.com/backups", Password: "fixture-password-long", KeepLast: 3}}
			manifest := backupManifest{Record: BackupRecord{ID: "backup", Workload: "resource", Repository: request.Backup.Repository}}
			if foreign {
				manifest.Record.Workload = "another-resource"
			}
			engine := &Engine{Root: root, Run: func(_ context.Context, _ string, args, env []string, out io.Writer) error {
				joined := strings.Join(args, " ")
				if strings.Contains(joined, "image inspect") {
					return errors.New("image absent")
				}
				if strings.Contains(joined, "docker pull") {
					pulled = true
					_, err := io.WriteString(out, "image download progress")
					return err
				}
				if !pulled {
					t.Fatal("machine-readable output requested before preparing image")
				}
				if strings.Contains(joined, "snapshots --json") {
					return json.NewEncoder(out).Encode([]map[string]string{{"id": strings.Repeat("a", 64)}})
				}
				if strings.Contains(joined, "dump ") {
					return json.NewEncoder(out).Encode(manifest)
				}
				t.Fatalf("unexpected command %s", joined)
				return nil
			}}
			got, err := engine.loadBackupManifest(context.Background(), request, io.Discard)
			if foreign {
				if err == nil {
					t.Fatal("accepted foreign metadata")
				}
				if _, err = os.Stat(filepath.Join(root, "workloads", "resource", "backups", "backup.json")); !os.IsNotExist(err) {
					t.Fatal("persisted foreign metadata")
				}
			} else if err != nil || got.Record.Snapshot != strings.Repeat("a", 64) {
				t.Fatal("metadata recovery failed", err)
			}
		})
	}
}

func TestPortableBackupVolumeLayout(t *testing.T) {
	a := []backupMount{{Type: "volume", Name: "app-data", Source: "/var/lib/docker/volumes/app-data/_data", RW: true}}
	b := []backupMount{{Type: "volume", Name: "app-data", Source: "/mnt/docker/volumes/app-data/_data", RW: true}}
	if !reflect.DeepEqual(portableBackupMounts(a), portableBackupMounts(b)) {
		t.Fatal("Docker data root must not prevent same-volume recovery")
	}
	b[0].Name = "other-data"
	if reflect.DeepEqual(portableBackupMounts(a), portableBackupMounts(b)) {
		t.Fatal("different volume accepted")
	}
	a[0].Type = "bind"
	b[0].Type = "bind"
	b[0].Name = a[0].Name
	if reflect.DeepEqual(portableBackupMounts(a), portableBackupMounts(b)) {
		t.Fatal("different bind source accepted")
	}
}
