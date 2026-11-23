package worker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (e *Engine) verifySQLDatabaseDump(ctx context.Context, r Request, source, dump string) (result error) {
	d := r.Backup.Database
	if d == nil || d.Engine != "postgresql" && d.Engine != "mariadb" {
		return errors.New("SQL database backup policy required")
	}
	image, err := e.output(ctx, "docker", "inspect", "--format", "{{.Image}}", source)
	if err != nil || !imageDigest.MatchString(image) {
		return errors.New("database image identity unavailable")
	}
	owner, err := e.backupHelperOwner(true)
	if err != nil {
		return err
	}
	name := databaseHelperName(r)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		result = errors.Join(result, e.cleanupDatabaseVerifier(cleanup, r))
	}()
	password := rand.Text()
	env := []string{"POSTGRES_PASSWORD=" + password, "POSTGRES_USER=postgres", "POSTGRES_DB=" + d.Name, "PGPASSWORD=" + password, "MARIADB_ROOT_PASSWORD=" + password, "MARIADB_DATABASE=" + d.Name, "MYSQL_PWD=" + password}
	args := []string{"docker", "create", "--log-driver", "local", "--log-opt", "max-size=1m", "--log-opt", "max-file=2", "--name", name, "--label", "dev.cove.database-verify=" + r.ID, "--label", "dev.cove.backup-owner=" + owner, "--network", "none", "--restart", "no", "--memory", "2g", "--cpus", "2", "--pids-limit", "512"}
	for _, value := range env {
		key, _, _ := strings.Cut(value, "=")
		args = append(args, "-e", key)
	}
	args = append(args, image)
	if err = e.Run(ctx, e.Root, args, env, io.Discard); err != nil {
		return errors.New("could not create isolated import verifier")
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "start", name}, nil, io.Discard); err != nil {
		return errors.New("could not start isolated import verifier")
	}
	ready := false
	for attempt := 0; attempt < 90; attempt++ {
		probe := []string{"docker", "exec", name, "pg_isready", "--host", "127.0.0.1", "--username", "postgres", "--dbname", d.Name}
		if d.Engine == "mariadb" {
			probe = []string{"docker", "exec", name, "mariadb", "--host=127.0.0.1", "--user=root", "--database", d.Name, "--execute", "SELECT 1"}
		}
		if e.Run(ctx, e.Root, probe, nil, io.Discard) == nil {
			ready = true
			break
		}
		state, inspectErr := e.output(ctx, "docker", "inspect", "--format", "{{.State.Status}}", name)
		if inspectErr != nil || state == "exited" || state == "dead" {
			return errors.New("isolated import verifier exited before becoming ready")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if !ready {
		return errors.New("isolated database import verifier did not become ready")
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "cp", dump, name + ":/tmp/cove-import.dump"}, nil, io.Discard); err != nil {
		return errors.New("could not stage isolated import verification")
	}
	command := []string{"docker", "exec", name, "pg_restore", "--username", "postgres", "--dbname", d.Name, "--no-owner", "--no-acl", "--single-transaction", "--exit-on-error", "/tmp/cove-import.dump"}
	if d.Engine == "mariadb" {
		command = []string{"docker", "exec", name, "sh", "-ec", "exec mariadb --user=root < /tmp/cove-import.dump"}
	}
	if err = e.Run(ctx, e.Root, command, nil, io.Discard); err != nil {
		return errors.New("dump failed isolated database restore validation; source storage was not changed")
	}
	if info, err := os.Stat(dump); err != nil || !info.Mode().IsRegular() || info.Size() < 1 {
		return errors.New("dump changed during import verification")
	}
	return nil
}

func (e *Engine) createImportedDatabaseBackup(ctx context.Context, r Request, dump string) (result error) {
	if r.Backup == nil || r.Backup.Database == nil {
		return errors.New("select a logical database backup policy before importing a dump")
	}
	records, err := e.Backups(r.Workload)
	if err != nil {
		return err
	}
	if len(records) >= 2006 {
		return errors.New("backup history is at its 2006-record import cap; complete a normal backup and retention pass first")
	}
	container, err := e.databaseContainer(ctx, r)
	if err != nil {
		return err
	}
	if r.Backup.Remote != nil {
		err = e.remoteBackupHelper(ctx, io.Discard)
	} else {
		err = e.Run(ctx, e.Root, []string{"docker", "pull", "restic/restic:0.19.1"}, nil, io.Discard)
	}
	if err != nil {
		return err
	}
	if e.restic(ctx, r, nil, false, io.Discard, "snapshots", "--json") != nil {
		if err = e.restic(ctx, r, nil, false, io.Discard, "init"); err != nil {
			return errors.New("backup repository unavailable")
		}
	}
	if err = e.settleDatabaseSnapshot(ctx, r); err != nil {
		return err
	}
	journal := filepath.Join(e.Root, "workloads", r.Workload, "database-snapshot.json")
	pending := databaseSnapshotJournal{Operation: r.ID, Repository: r.Backup.Repository}
	if err = writeJSON(journal, pending); err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		result = errors.Join(result, e.settleDatabaseSnapshot(c, r))
	}()
	if extendedDatabase(r.Backup.Database.Engine) {
		cleanup, verifyErr := e.verifyExtendedDatabase(ctx, r, container, dump, false)
		if verifyErr != nil {
			return verifyErr
		}
		if err = cleanup(); err != nil {
			return err
		}
	} else if err = e.verifySQLDatabaseDump(ctx, r, container, dump); err != nil {
		return err
	}
	state, err := os.ReadFile(filepath.Join(e.Root, "workloads", r.Workload, "current.json"))
	if err != nil {
		return err
	}
	var current deployedState
	if json.Unmarshal(state, &current) != nil {
		return errors.New("deployed database configuration unavailable")
	}
	mounts := []backupMount{{Type: "bind", Source: filepath.Dir(dump), RW: true, Destination: "/database"}}
	manifest, err := e.makeBackup(ctx, r, mounts, current, io.Discard)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	manifest.Record.VerifiedAt, manifest.Record.Verification = &now, "database"
	if err = writeJSON(filepath.Join(e.Root, "workloads", r.Workload, "backups", r.ID+".json"), manifest); err != nil {
		return err
	}
	pending.Complete = true
	if err = writeJSON(journal, pending); err != nil {
		return err
	}
	return e.pruneBackupsLocked(ctx, r, io.Discard)
}
