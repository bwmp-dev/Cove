package worker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupTransferStartupRecoveryPreservesSourceAndRepositoryReferences(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	owner, err := e.backupHelperOwner(true)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", "app")
	for _, path := range []string{filepath.Join(e.Root, "backup-transfer", "input"), filepath.Join(dir, "database-stream"), filepath.Join(dir, "database-verify"), filepath.Join(dir, "release-states")} {
		if err = os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(path, "marker"), []byte("private fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = writeJSON(filepath.Join(e.Root, "backup-transfer.json"), struct{ ID, Workload string }{"transfer", "app"}); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(dir, "database-snapshot.json"), databaseSnapshotJournal{Operation: "pending", Repository: "local"}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "current.json"), []byte("source state"), 0600); err != nil {
		t.Fatal(err)
	}
	removed := []string{}
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		command := strings.Join(args, " ")
		if strings.Contains(command, "forget") || strings.Contains(command, "prune") {
			t.Fatal("startup deleted encrypted repository references")
		}
		if len(args) > 1 && args[1] == "ps" {
			if !strings.Contains(command, "label=dev.cove.backup-owner="+owner) {
				t.Fatal("helper discovery was not scoped to this worker")
			}
			if !strings.Contains(command, "name=") {
				_, err := io.WriteString(out, strings.Repeat("a", 12))
				return err
			}
			if strings.Contains(command, "name=^/cove-backup-transfer$") {
				_, err := io.WriteString(out, strings.Repeat("b", 12))
				return err
			}
		}
		if len(args) > 1 && args[1] == "rm" {
			removed = append(removed, args[len(args)-1])
		}
		return nil
	}
	if err = e.recoverBackupTransfersOnStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || removed[0] != strings.Repeat("a", 12) || removed[1] != strings.Repeat("b", 12) {
		t.Fatal("unexpected containers removed", removed)
	}
	for _, path := range []string{filepath.Join(e.Root, "backup-transfer"), filepath.Join(e.Root, "backup-transfer.json"), filepath.Join(dir, "database-stream"), filepath.Join(dir, "database-verify")} {
		if _, err = os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("plaintext transfer state retained", path, err)
		}
	}
	for _, path := range []string{filepath.Join(dir, "current.json"), filepath.Join(dir, "release-states", "marker"), filepath.Join(dir, "database-snapshot.json")} {
		if _, err = os.Stat(path); err != nil {
			t.Fatal("source or repository recovery metadata removed", err)
		}
	}
}

func TestBackupTransferRecoveryRejectsUnsafeJournalBeforeDockerAccess(t *testing.T) {
	e := &Engine{Root: t.TempDir(), Run: func(context.Context, string, []string, []string, io.Writer) error {
		t.Fatal("unsafe recovery record reached Docker")
		return nil
	}}
	path := filepath.Join(e.Root, "backup-transfer.json")
	if err := writeJSON(path, struct{ ID, Workload string }{"../foreign", "app"}); err != nil {
		t.Fatal(err)
	}
	if err := e.recoverBackupTransfer(context.Background()); err == nil {
		t.Fatal("unsafe transfer journal accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("invalid recovery evidence discarded", err)
	}
}
