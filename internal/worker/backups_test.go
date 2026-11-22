package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailedRestoreRetryKeepsContainersStopped(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", "app")
	if err = os.MkdirAll(filepath.Join(dir, "backups"), 0700); err != nil {
		t.Fatal(err)
	}
	mounts := []backupMount{{Type: "volume", Name: "app-data", Source: "/var/lib/docker/volumes/app-data/_data", RW: true}}
	for path, value := range map[string]any{
		"current.json":          deployedState{},
		"storage-recovery.json": storageRecovery{Containers: []string{"container"}, Restoring: true, Helper: "old-helper"},
		"backups/snapshot.json": backupManifest{Record: BackupRecord{ID: "snapshot", Workload: "app", Repository: "local", Snapshot: strings.Repeat("a", 64)}, Mounts: mounts},
	} {
		if err = writeJSON(filepath.Join(dir, path), value); err != nil {
			t.Fatal(err)
		}
	}
	started := false
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		switch {
		case len(args) > 2 && args[1] == "ps" && args[2] == "-q":
			return nil
		case len(args) > 1 && args[1] == "ps":
			_, err = io.WriteString(out, `{"ID":"container","State":"exited"}`)
		case len(args) > 2 && args[1] == "inspect":
			err = json.NewEncoder(out).Encode(mounts)
		case len(args) > 1 && args[1] == "start":
			started = true
		case strings.Contains(strings.Join(args, " "), " backup "):
			return errors.New("repository unavailable during safety backup")
		case len(args) > 2 && args[1] == "volume":
			_, err = io.WriteString(out, "app")
		}
		return err
	}
	err = e.performBackup(context.Background(), Request{ID: "retry", Workload: "app", Action: "restore", BackupID: "snapshot", Backup: &BackupConfig{Repository: "local", Password: "sixteen-character-password", KeepLast: 2}}, io.Discard)
	if err == nil || started {
		t.Fatalf("err=%v restarted=%v", err, started)
	}
	data, err := os.ReadFile(filepath.Join(dir, "storage-recovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recovery storageRecovery
	if err = json.Unmarshal(data, &recovery); err != nil || !recovery.Restoring {
		t.Fatalf("lost restore lock: %s %v", data, err)
	}
}

func TestBackupValidation(t *testing.T) {
	for _, repo := range []string{"http://host/bucket", "s3:http://host/bucket", "s3:https://user:password@host/bucket", "s3:https://host/bucket?secret=abc"} {
		if ValidateBackup(&BackupConfig{Repository: repo, Password: "sixteen-character-password", KeepLast: 7}) == nil {
			t.Fatal("accepted", repo)
		}
	}
}
