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

func TestBackupTransferStartupDocker(t *testing.T) {
	fixture := os.Getenv("COVE_DATABASE_FIXTURE")
	if fixture == "" {
		t.Skip("disposable Docker fixture required")
	}
	f := newDatabaseImportFixture(t, filepath.Join(fixture, "recovery-"+filepath.Base(fixture)), "redis")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r := f.request
	r.ID = "interrupted-transfer"
	if err := f.engine.startBackupTransfer(ctx, r); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.engine.Root, "backup-transfer", "input")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(dir, "database.dump")
	if err := os.WriteFile(dump, f.dump, 0600); err != nil {
		t.Fatal(err)
	}
	cleanup, err := f.engine.verifyExtendedDatabase(ctx, r, f.container, dump, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	owner, err := f.engine.backupHelperOwner(false)
	if err != nil {
		t.Fatal(err)
	}
	foreign := &Engine{Root: filepath.Join(fixture, "foreign-root")}
	if err = os.MkdirAll(foreign.Root, 0700); err != nil {
		t.Fatal(err)
	}
	foreignOwner, err := foreign.backupHelperOwner(true)
	if err != nil || foreignOwner == owner {
		t.Fatal("worker ownership is not distinct", err)
	}
	for _, helper := range []struct{ name, owner, label string }{{"cove-backup-" + r.ID, owner, "dev.cove.operation=" + r.ID}, {"cove-foreign-" + filepath.Base(fixture), foreignOwner, "dev.cove.database-verify=" + r.ID}} {
		args := []string{"docker", "run", "-d", "--name", helper.name, "--label", helper.label, "--label", "dev.cove.backup-owner=" + helper.owner, "--log-driver", "none", "--network", "none", "--memory", "32m", "busybox:1.37", "sleep", "600"}
		if err = f.engine.Run(ctx, f.engine.Root, args, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
		name := helper.name
		t.Cleanup(func() {
			ids, err := f.engine.output(context.Background(), "docker", "ps", "-aq", "--filter", "name=^/"+name+"$")
			if err != nil {
				t.Error(err)
			} else if strings.TrimSpace(ids) != "" {
				if err = f.engine.Run(context.Background(), f.engine.Root, []string{"docker", "rm", "-f", name}, nil, io.Discard); err != nil {
					t.Error(err)
				}
			}
		})
	}
	fresh, err := NewEngine(f.engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err = fresh.recoverBackupTransfersOnStartup(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{databaseHelperName(r), "cove-backup-" + r.ID} {
		ids, err := fresh.output(ctx, "docker", "ps", "-aq", "--filter", "name=^/"+name+"$")
		if err != nil || strings.TrimSpace(ids) != "" {
			t.Fatal("interrupted helper remained", name, err)
		}
	}
	state, err := fresh.output(ctx, "docker", "inspect", "--format", "{{.State.Running}}", "cove-foreign-"+filepath.Base(fixture))
	if err != nil || state != "true" || f.value() != "before" {
		t.Fatal("startup cleanup touched foreign helper or source", err)
	}
	if _, err = os.Stat(filepath.Join(f.engine.Root, "backup-transfer")); !os.IsNotExist(err) {
		t.Fatal("restart retained plaintext dump", err)
	}
	if _, err = os.Stat(filepath.Join(f.engine.Root, "backup-transfer.json")); !os.IsNotExist(err) {
		t.Fatal("restart retained completed cleanup journal", err)
	}
}
