package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

type databaseSnapshotJournal struct {
	Operation, Repository string
	Complete              bool
}

func (e *Engine) settleDatabaseSnapshot(ctx context.Context, r Request) error {
	path := filepath.Join(e.Root, "workloads", r.Workload, "database-snapshot.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var pending databaseSnapshotJournal
	if len(data) > 16384 || json.Unmarshal(data, &pending) != nil || !identifier.MatchString(pending.Operation) {
		return errors.New("invalid database snapshot recovery record")
	}
	if !pending.Complete {
		if err = e.cleanupDatabaseVerifier(ctx, Request{ID: pending.Operation, Workload: r.Workload}); err != nil {
			return err
		}
		if pending.Repository != r.Backup.Repository {
			return errors.New("restore the original repository policy to finish interrupted database backup cleanup")
		}
		for _, operation := range []string{pending.Operation, pending.Operation + "-data"} {
			out := &limitedBuffer{Limit: 2 << 20}
			if err = e.restic(ctx, r, nil, false, out, "snapshots", "--json", "--tag", "cove-workload:"+r.Workload+",cove-operation:"+operation); err != nil {
				return errors.New("interrupted database backup cleanup requires a reachable repository")
			}
			var snapshots []struct{ ID string }
			if json.Unmarshal(out.data.Bytes(), &snapshots) != nil || len(snapshots) > 1 {
				return errors.New("ambiguous interrupted database snapshot")
			}
			for _, snapshot := range snapshots {
				if !snapshotIdentifier.MatchString(snapshot.ID) {
					return errors.New("invalid interrupted snapshot identity")
				}
				if err = e.restic(ctx, r, nil, false, io.Discard, "forget", snapshot.ID); err != nil {
					return err
				}
			}
		}
		for _, suffix := range []string{".json", ".source.json"} {
			if err = os.Remove(filepath.Join(e.Root, "workloads", r.Workload, "backups", pending.Operation+suffix)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err = e.restic(ctx, r, nil, false, io.Discard, "prune"); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func (e *Engine) createStreamedDatabaseBackup(ctx context.Context, r Request, container string, mounts []backupMount, current deployedState, out io.Writer) (manifest backupManifest, result error) {
	if err := e.settleDatabaseSnapshot(ctx, r); err != nil {
		return manifest, err
	}
	path := filepath.Join(e.Root, "workloads", r.Workload, "database-snapshot.json")
	pending := databaseSnapshotJournal{Operation: r.ID, Repository: r.Backup.Repository}
	if err := writeJSON(path, pending); err != nil {
		return manifest, err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		result = errors.Join(result, e.settleDatabaseSnapshot(c, r))
	}()
	snapshot, err := e.streamDatabaseSnapshot(ctx, r, container, out)
	if err != nil {
		return manifest, err
	}
	manifest, err = e.makeBackupWithData(ctx, r, mounts, current, out, snapshot)
	if err != nil {
		return manifest, err
	}
	stage, err := e.databaseStage(r.Workload, "database-verify")
	if err != nil {
		return manifest, err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(stage)) }()
	dump := filepath.Join(stage, "database.dump")
	if err = e.readDatabaseSnapshot(ctx, r, manifest, dump); err != nil {
		return manifest, err
	}
	cleanup, err := e.verifyExtendedDatabase(ctx, r, container, dump, false)
	if err != nil {
		return manifest, err
	}
	if err = cleanup(); err != nil {
		return manifest, err
	}
	now := time.Now().UTC()
	manifest.Record.VerifiedAt = &now
	manifest.Record.Verification = "database"
	if err = writeJSON(filepath.Join(e.Root, "workloads", r.Workload, "backups", r.ID+".json"), manifest); err != nil {
		return manifest, err
	}
	pending.Complete = true
	return manifest, writeJSON(path, pending)
}

func (e *Engine) forgetDatabaseSnapshots(ctx context.Context, r Request, record BackupRecord) error {
	for _, item := range []struct{ operation, snapshot string }{{record.ID, record.Snapshot}, {record.ID + "-data", record.DataSnapshot}} {
		if !identifier.MatchString(item.operation) || !snapshotIdentifier.MatchString(item.snapshot) {
			return errors.New("invalid database snapshot identity")
		}
		out := &limitedBuffer{Limit: 2 << 20}
		if err := e.restic(ctx, r, nil, false, out, "snapshots", "--json", "--tag", "cove-workload:"+r.Workload+",cove-operation:"+item.operation); err != nil {
			return err
		}
		var found []struct{ ID string }
		if json.Unmarshal(out.data.Bytes(), &found) != nil || len(found) > 1 {
			return errors.New("ambiguous database snapshot identity")
		}
		if len(found) == 1 {
			if found[0].ID != item.snapshot {
				return errors.New("database snapshot identity changed")
			}
			if err := e.restic(ctx, r, nil, false, io.Discard, "forget", item.snapshot); err != nil {
				return err
			}
		}
	}
	return nil
}
