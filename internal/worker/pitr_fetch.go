package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func RunPITRFetch(ctx context.Context, input io.Reader) (result error) {
	var request PITRRestore
	data, err := io.ReadAll(io.LimitReader(input, 4097))
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &request) != nil {
		return errors.New("invalid PITR recovery request")
	}
	var config pitrRunnerConfig
	if err = readSnapshotMetadata("/state/config.json", &config); err != nil {
		return err
	}
	if err = validatePITRRunner(config); err != nil {
		return err
	}
	baseName := strings.TrimPrefix(request.Base, config.Policy.Archive+"_")
	if !pitrBaseName.MatchString(baseName) || request.Timestamp.IsZero() {
		return errors.New("invalid PITR base or timestamp")
	}
	broker, err := startPITRBroker(ctx, config)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, broker.close()) }()
	bases, err := pitrBaseInventory(ctx)
	if err != nil {
		return err
	}
	var selected *pitrStoredBase
	for i := range bases {
		if bases[i].Name == baseName {
			selected = &bases[i]
			break
		}
	}
	if selected == nil || request.Timestamp.Before(selected.Time) || request.Timestamp.After(time.Now().UTC()) {
		return errors.New("PITR base was pruned or the timestamp is outside its recovery window")
	}
	inventory, err := pitrArchiveInventory(ctx, broker.root, config, broker.environment)
	if err != nil {
		return err
	}
	names := []string{}
	for _, name := range inventory.WALNames {
		if name >= selected.WAL {
			names = append(names, name)
		}
	}
	if len(names) == 0 || int64(len(names)) > config.Policy.Settings.MaxBytes/(16<<20) {
		return errors.New("PITR WAL recovery stage exceeds its configured byte limit or is empty; select a newer base")
	}
	for _, directory := range []string{"/restore", "/wals"} {
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 0 {
			return errors.New("PITR restore requires fresh empty storage")
		}
	}
	budget, err := transferDiskBudget("/restore", 2*config.Policy.Settings.MaxBytes)
	if err != nil || budget < 2*config.Policy.Settings.MaxBytes {
		return errors.New("PITR recovery needs twice its configured byte limit plus the 256 MiB disk reserve")
	}
	ctx, finish, err := guardPITRStage(ctx, 2*config.Policy.Settings.MaxBytes)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, finish()) }()
	if _, err = pitrWALG(ctx, "backup-fetch", "/restore", baseName); err != nil {
		return errors.New("PITR encrypted base backup verification or download failed")
	}
	for _, name := range names {
		destination := filepath.Join("/wals", name)
		if _, err = pitrWALG(ctx, "wal-fetch", name, destination); err != nil {
			return errors.New("PITR encrypted WAL is missing or failed verification")
		}
		info, err := os.Lstat(destination)
		if err != nil || !info.Mode().IsRegular() || info.Size() != 16<<20 {
			return errors.New("PITR WAL segment has an unexpected type or size")
		}
		if err = os.Chmod(destination, 0444); err != nil {
			return err
		}
		if free, err := transferDiskBudget("/wals", 1); err != nil || free != 1 {
			return errors.New("PITR recovery disk reserve reached")
		}
	}
	return nil
}
