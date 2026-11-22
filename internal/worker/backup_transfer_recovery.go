package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func readBackupRecoveryFile(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, 32769))
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if len(data) > 32768 || json.Unmarshal(data, value) != nil {
		return errors.New("invalid backup recovery record")
	}
	return nil
}

func (e *Engine) backupHelperOwner(create bool) (string, error) {
	if e.Root == "" {
		if create {
			return "", errors.New("worker state directory required")
		}
		return "", nil
	}
	path := filepath.Join(e.Root, "backup-helper-owner.json")
	var owner struct{ ID string }
	err := readBackupRecoveryFile(path, &owner)
	if os.IsNotExist(err) {
		if !create {
			return "", nil
		}
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return "", err
		}
		owner.ID = hex.EncodeToString(nonce[:])
		if err = writeJSON(path, owner); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	decoded, err := hex.DecodeString(owner.ID)
	if err != nil || len(decoded) != 16 {
		return "", errors.New("invalid backup helper ownership record")
	}
	return owner.ID, nil
}

func (e *Engine) removeBackupHelper(ctx context.Context, owner, name, operation string) error {
	ids, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "name=^/"+name+"$", "--filter", "label=dev.cove.operation="+operation, "--filter", "label=dev.cove.backup-owner="+owner)
	if err != nil {
		return err
	}
	if owned := strings.Fields(ids); len(owned) > 0 {
		return e.Run(ctx, e.Root, append([]string{"docker", "rm", "-f"}, owned...), nil, io.Discard)
	}
	return nil
}

func (e *Engine) recoverBackupTransfer(ctx context.Context) error {
	journal := filepath.Join(e.Root, "backup-transfer.json")
	var task struct{ ID, Workload string }
	err := readBackupRecoveryFile(journal, &task)
	if os.IsNotExist(err) {
		return os.RemoveAll(filepath.Join(e.Root, "backup-transfer"))
	}
	if err != nil || !identifier.MatchString(task.ID) || !workloadIdentifier.MatchString(task.Workload) {
		return errors.New("invalid backup transfer recovery record")
	}
	owner, err := e.backupHelperOwner(false)
	if err != nil || owner == "" {
		return errors.New("backup transfer helper ownership unavailable")
	}
	for _, name := range []string{"cove-backup-" + task.ID, "cove-s3-import-" + task.ID + "-stat", "cove-s3-import-" + task.ID + "-read"} {
		if err = e.removeBackupHelper(ctx, owner, name, task.ID); err != nil {
			return err
		}
	}
	if err = e.cleanupDatabaseVerifier(ctx, Request{ID: task.ID, Workload: task.Workload}); err != nil {
		return err
	}
	if err = os.RemoveAll(filepath.Join(e.Root, "backup-transfer")); err != nil {
		return err
	}
	return os.Remove(journal)
}

func (e *Engine) startBackupTransfer(ctx context.Context, r Request) error {
	if err := e.recoverBackupTransfer(ctx); err != nil {
		return err
	}
	if _, err := e.backupHelperOwner(true); err != nil {
		return err
	}
	return writeJSON(filepath.Join(e.Root, "backup-transfer.json"), struct{ ID, Workload string }{r.ID, r.Workload})
}

func (e *Engine) recoverBackupTransfersOnStartup(ctx context.Context) error {
	owner, err := e.backupHelperOwner(false)
	if err != nil {
		return err
	}
	if owner != "" {
		ids, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "label=dev.cove.database-verify", "--filter", "label=dev.cove.backup-owner="+owner)
		if err != nil {
			return err
		}
		owned := strings.Fields(ids)
		if len(owned) > 32 {
			return errors.New("backup verifier recovery exceeds its helper cap")
		}
		if len(owned) > 0 {
			if err = e.Run(ctx, e.Root, append([]string{"docker", "rm", "--force", "--volumes"}, owned...), nil, io.Discard); err != nil {
				return err
			}
		}
	}
	if err = e.recoverBackupTransfer(ctx); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(e.Root, "workloads"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !workloadIdentifier.MatchString(entry.Name()) {
			continue
		}
		dir := filepath.Join(e.Root, "workloads", entry.Name())
		var pending databaseSnapshotJournal
		err = readBackupRecoveryFile(filepath.Join(dir, "database-snapshot.json"), &pending)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && !pending.Complete {
			if owner == "" || !identifier.MatchString(pending.Operation) {
				return errors.New("interrupted backup helper ownership unavailable")
			}
			for _, operation := range []string{pending.Operation, pending.Operation + "-data"} {
				if err = e.removeBackupHelper(ctx, owner, "cove-backup-"+operation, operation); err != nil {
					return err
				}
			}
		}
		for _, stage := range []string{"database-stream", "database-verify"} {
			if err = os.RemoveAll(filepath.Join(dir, stage)); err != nil {
				return err
			}
		}
	}
	return nil
}
