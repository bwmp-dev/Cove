package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type pitrCopyRecovery struct {
	Workload string `json:"workload"`
	Owner    string `json:"owner"`
}

func (e *Engine) cleanupPITRCopy(ctx context.Context, id string) error {
	if !identifier.MatchString(id) {
		return errors.New("invalid PITR recovery operation")
	}
	journal := filepath.Join(e.Root, "pitr-recovery", id+".json")
	var recovery pitrCopyRecovery
	if err := readSnapshotMetadata(journal, &recovery); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !workloadIdentifier.MatchString(recovery.Workload) || !snapshotOwnerID.MatchString(recovery.Owner) {
		return errors.New("invalid PITR recovery ownership")
	}
	for _, name := range []string{"cove-pitr-fetch-" + id, "cove-pitr-replay-" + id} {
		container, err := e.pitrContainerID(ctx, name)
		if err != nil {
			return err
		}
		if container != "" {
			owner, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "dev.cove.pitr.owner"}}`, name)
			if err != nil {
				return err
			}
			if owner != recovery.Owner {
				return errors.New("PITR recovery helper ownership changed")
			}
			if err = e.Run(ctx, e.Root, []string{"docker", "rm", "-f", "--volumes", name}, nil, io.Discard); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"cove-pitr-wal-" + id, "cove-pitr-verify-" + id} {
		exists, err := e.pitrVolumeExists(ctx, name)
		if err != nil {
			return err
		}
		if exists {
			owner, err := e.output(ctx, "docker", "volume", "inspect", "--format", `{{index .Labels "dev.cove.restore"}}`, name)
			if err != nil {
				return err
			}
			if owner != recovery.Workload {
				return errors.New("PITR recovery stage ownership changed")
			}
			if err = e.Run(ctx, e.Root, []string{"docker", "volume", "rm", name}, nil, io.Discard); err != nil {
				return err
			}
		}
	}
	return os.Remove(journal)
}

func (e *Engine) recoverPITRCopies(ctx context.Context) error {
	entries, err := os.ReadDir(filepath.Join(e.Root, "pitr-recovery"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 32 {
		return errors.New("PITR recovery journal limit exceeded")
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return errors.New("unexpected PITR recovery artifact")
		}
		if err = e.cleanupPITRCopy(ctx, strings.TrimSuffix(entry.Name(), ".json")); err != nil {
			return err
		}
	}
	return nil
}
