package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStreamedDatabaseSnapshotRecovery(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "incomplete", true: "published"}[complete], func(t *testing.T) {
			root := t.TempDir()
			r := Request{ID: "next", Workload: "db", Backup: &BackupConfig{Repository: "local"}}
			dir := filepath.Join(root, "workloads", "db")
			if err := os.MkdirAll(filepath.Join(dir, "backups"), 0700); err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(dir, "database-snapshot.json")
			if err := writeJSON(journal, databaseSnapshotJournal{Operation: "pending", Repository: "local", Complete: complete}); err != nil {
				t.Fatal(err)
			}
			metadata := filepath.Join(dir, "backups", "pending.json")
			if err := os.WriteFile(metadata, []byte("private metadata"), 0600); err != nil {
				t.Fatal(err)
			}
			forgotten, pruned := 0, false
			e := &Engine{Root: root, Run: func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
				command := strings.Join(args, " ")
				switch {
				case strings.Contains(command, "volume inspect"):
					_, err := io.WriteString(out, "db")
					return err
				case strings.Contains(command, "snapshots --json"):
					return json.NewEncoder(out).Encode([]map[string]string{{"id": strings.Repeat("a", 64)}})
				case strings.Contains(command, "forget "):
					forgotten++
				case strings.HasSuffix(command, " prune"):
					pruned = true
				}
				return nil
			}}
			if err := e.settleDatabaseSnapshot(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(journal); !os.IsNotExist(err) {
				t.Fatal("reconciled journal retained", err)
			}
			_, metadataErr := os.Stat(metadata)
			if complete && (forgotten != 0 || pruned || metadataErr != nil) {
				t.Fatal("published backup was pruned")
			}
			if !complete && (forgotten != 2 || !pruned || !os.IsNotExist(metadataErr)) {
				t.Fatal("incomplete data and metadata were not reclaimed")
			}
		})
	}
}

func TestStreamedDatabasePruneCanResumeAfterOneSnapshotWasForgotten(t *testing.T) {
	r := Request{ID: "prune", Workload: "db", Backup: &BackupConfig{Repository: "local"}}
	record := BackupRecord{ID: "backup", Snapshot: strings.Repeat("a", 64), DataSnapshot: strings.Repeat("b", 64)}
	forgotten := ""
	e := &Engine{Root: t.TempDir(), Run: func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		command := strings.Join(args, " ")
		switch {
		case strings.Contains(command, "volume inspect"):
			_, err := io.WriteString(out, "db")
			return err
		case strings.Contains(command, "snapshots --json"):
			if strings.HasSuffix(command, "cove-operation:backup-data") {
				return json.NewEncoder(out).Encode([]map[string]string{{"id": record.DataSnapshot}})
			}
			_, err := io.WriteString(out, "[]")
			return err
		case strings.Contains(command, "forget "):
			forgotten = args[len(args)-1]
		}
		return nil
	}}
	if err := e.forgetDatabaseSnapshots(context.Background(), r, record); err != nil || forgotten != record.DataSnapshot {
		t.Fatal("paired prune could not resume", err, forgotten)
	}
}

func TestDatabaseStreamBoundRejectsOverflowWithoutWriting(t *testing.T) {
	var out bytes.Buffer
	w := &sizeBoundWriter{Writer: &out, remaining: 3}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("d")); err == nil || out.String() != "abc" {
		t.Fatal("stream exceeded its limit")
	}
}

func TestDatabaseSnapshotRecoveryRetainsJournalWhenRepositoryUnavailable(t *testing.T) {
	e := &Engine{Root: t.TempDir(), Run: func(_ context.Context, _ string, args, _ []string, _ io.Writer) error {
		if strings.Contains(strings.Join(args, " "), "ps -aq") {
			return nil
		}
		return errors.New("unavailable")
	}}
	path := filepath.Join(e.Root, "workloads", "db", "database-snapshot.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, databaseSnapshotJournal{Operation: "pending", Repository: "remote"}); err != nil {
		t.Fatal(err)
	}
	if err := e.settleDatabaseSnapshot(context.Background(), Request{Workload: "db", Backup: &BackupConfig{Repository: "remote"}}); err == nil {
		t.Fatal("unavailable repository accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("recovery identity lost", err)
	}
}

func TestExtendedDatabaseDoesNotEnterSQLPreviewRefresh(t *testing.T) {
	e := &Engine{Run: func(context.Context, string, []string, []string, io.Writer) error {
		t.Fatal("unsupported engine reached SQL commands")
		return nil
	}}
	for _, engine := range []string{"mongodb", "redis", "valkey"} {
		d := &DatabaseBackup{Engine: engine, Service: "db", Name: "all"}
		if err := validateDatabaseBackup(d); err != nil {
			t.Fatal(err)
		}
		inventory := e.inspectSourceDatabase(context.Background(), "db", d)
		if inventory.Supported || inventory.Reason == "" {
			t.Fatal("unsupported preview refresh accepted", engine)
		}
	}
}
