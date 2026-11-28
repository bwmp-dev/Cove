package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var snapshotIdentifier = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (e *Engine) loadBackupManifest(ctx context.Context, r Request, out io.Writer) (backupManifest, error) {
	path := filepath.Join(e.Root, "workloads", r.Workload, "backups", r.BackupID+".json")
	var manifest backupManifest
	raw, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(raw, &manifest)
		if err == nil && manifest.Record.DataSnapshot != "" && !snapshotIdentifier.MatchString(manifest.Record.DataSnapshot) {
			err = errors.New("invalid database data snapshot")
		}
		return manifest, err
	}
	if !os.IsNotExist(err) {
		return manifest, err
	}
	if r.Backup.Repository == "local" {
		return manifest, errors.New("backup metadata is missing; local backups cannot recover a lost node")
	}
	if r.Backup.Remote != nil {
		if err = e.remoteBackupHelper(ctx, out); err != nil {
			return manifest, err
		}
	} else if _, err = e.output(ctx, "docker", "image", "inspect", "restic/restic:0.19.1"); err != nil {
		if err = e.Run(ctx, e.Root, []string{"docker", "pull", "restic/restic:0.19.1"}, nil, out); err != nil {
			return manifest, err
		}
	}
	snapshots := &limitedBuffer{Limit: 2 << 20}
	if err = e.restic(ctx, r, nil, false, snapshots, "snapshots", "--json", "--tag", "cove-workload:"+r.Workload+",cove-operation:"+r.BackupID); err != nil {
		return manifest, err
	}
	var records []struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(snapshots.data.Bytes(), &records); err != nil {
		return manifest, err
	}
	if len(records) != 1 || !snapshotIdentifier.MatchString(records[0].ID) {
		return manifest, errors.New("remote backup snapshot is missing or ambiguous")
	}
	data := &limitedBuffer{Limit: 2 << 20}
	if err = e.restic(ctx, r, nil, false, data, "dump", records[0].ID, "/cove-manifest.json"); err != nil {
		return manifest, err
	}
	if err = json.Unmarshal(data.data.Bytes(), &manifest); err != nil {
		return manifest, err
	}
	if manifest.Record.Workload != r.Workload || manifest.Record.ID != r.BackupID || manifest.Record.Repository != r.Backup.Repository {
		return manifest, errors.New("remote backup metadata does not match this resource and repository")
	}
	if manifest.Record.DataSnapshot != "" && !snapshotIdentifier.MatchString(manifest.Record.DataSnapshot) {
		return manifest, errors.New("invalid database data snapshot")
	}
	manifest.Record.Snapshot = records[0].ID
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return manifest, err
	}
	return manifest, writeJSON(path, manifest)
}
