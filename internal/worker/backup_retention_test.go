package worker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupPruningPreservesCalendarSnapshots(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "workloads", "app", "backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for _, record := range []BackupRecord{
		{ID: "latest", Repository: "local", Snapshot: strings.Repeat("a", 64), Created: now},
		{ID: "same-day", Repository: "local", Snapshot: strings.Repeat("b", 64), Created: now.Add(-time.Hour)},
		{ID: "previous-day", Repository: "local", Snapshot: strings.Repeat("c", 64), Created: now.Add(-24 * time.Hour)},
		{ID: "other-repository", Repository: "s3:other", Snapshot: strings.Repeat("d", 64), Created: now.Add(-48 * time.Hour)},
	} {
		if err := writeJSON(filepath.Join(dir, record.ID+".json"), backupManifest{Record: record}); err != nil {
			t.Fatal(err)
		}
	}
	forgotten := []string{}
	pruned := 0
	e := &Engine{Root: root, Run: func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		if len(args) > 2 && args[1] == "volume" && args[2] == "inspect" {
			_, err := io.WriteString(out, "app")
			return err
		}
		if args[len(args)-1] == "prune" {
			pruned++
			return nil
		}
		if args[len(args)-2] == "forget" {
			forgotten = append(forgotten, args[len(args)-1])
			return nil
		}
		t.Fatal("unexpected backup command", args)
		return nil
	}}
	if err := e.pruneBackups(context.Background(), Request{ID: "cleanup", Workload: "app", Backup: &BackupConfig{Repository: "local", Password: "synthetic-backup-password", KeepLast: 1, KeepDaily: 2}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(forgotten) != 1 || forgotten[0] != strings.Repeat("b", 64) || pruned != 1 {
		t.Fatal("incorrect snapshots removed", forgotten, pruned)
	}
	for _, id := range []string{"latest", "previous-day", "other-repository"} {
		if _, err := os.Stat(filepath.Join(dir, id+".json")); err != nil {
			t.Fatal("retained metadata removed", id, err)
		}
	}
}

func TestBackupCalendarRetention(t *testing.T) {
	stamp := func(value string) time.Time {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	records := []BackupRecord{
		{ID: "november", Repository: "local", Created: stamp("2025-11-30T12:00:00Z")},
		{ID: "same-day", Repository: "local", Created: stamp("2026-01-05T08:00:00Z")},
		{ID: "foreign", Repository: "s3:other", Created: stamp("2026-01-06T12:00:00Z")},
		{ID: "december", Repository: "local", Created: stamp("2025-12-28T12:00:00Z")},
		{ID: "yesterday", Repository: "local", Created: stamp("2026-01-04T12:00:00Z")},
		{ID: "recent", Repository: "local", Created: stamp("2026-01-05T12:00:00Z")},
		{ID: "utc-same-day", Repository: "local", Created: stamp("2026-01-04T23:30:00-08:00")},
	}
	policy := &BackupConfig{Repository: "local", KeepLast: 2, KeepDaily: 2, KeepWeekly: 2, KeepMonthly: 2}
	kept := retainedBackups(records, policy)
	if len(kept) != 4 || !kept["recent"] || !kept["same-day"] || !kept["yesterday"] || !kept["december"] {
		t.Fatal("incorrect calendar union", kept)
	}
	if records[0].ID != "november" {
		t.Fatal("input order changed")
	}
	policy.KeepDaily = 0
	policy.KeepWeekly = 0
	policy.KeepMonthly = 0
	legacy := retainedBackups(records, policy)
	if len(legacy) != 2 || !legacy["recent"] || !legacy["same-day"] {
		t.Fatal("legacy retention changed", legacy)
	}
	policy.Password = "synthetic-backup-password"
	policy.KeepDaily = -1
	if ValidateBackup(policy) == nil {
		t.Fatal("negative retention accepted")
	}
	policy.KeepDaily = 367
	if ValidateBackup(policy) == nil {
		t.Fatal("unbounded retention accepted")
	}
}

func TestBackblazeStorageConfiguration(t *testing.T) {
	s := &StorageLocation{Protocol: "b2", Path: "bucket/backups", AccessKey: "synthetic-key-id", SecretKey: "synthetic-application-key"}
	env, err := storageEnvironment(s)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "RCLONE_CONFIG_COVE_TYPE=b2") || !strings.Contains(joined, "RCLONE_CONFIG_COVE_ACCOUNT=synthetic-key-id") || !strings.Contains(joined, "RCLONE_CONFIG_COVE_KEY=synthetic-application-key") {
		t.Fatal("incorrect native B2 credentials")
	}
	for _, bad := range []StorageLocation{{Protocol: "b2", Path: "bucket", AccessKey: "id"}, {Protocol: "b2", Path: "/bucket", AccessKey: "id", SecretKey: "key"}, {Protocol: "b2", Endpoint: "http://example.com", Path: "bucket", AccessKey: "id", SecretKey: "key"}} {
		if ValidateStorage(&bad) == nil {
			t.Fatal("invalid B2 location accepted")
		}
	}
}
