package worker

import (
	"context"
	"errors"
	"time"
)

func RunPITRBase(ctx context.Context) error {
	var config pitrRunnerConfig
	if err := readSnapshotMetadata("/state/config.json", &config); err != nil {
		return err
	}
	if err := validatePITRRunner(config); err != nil {
		return err
	}
	if !config.Policy.Settings.Enabled {
		return errors.New("enable PITR capture before taking a base backup")
	}
	unlock, err := lockPITRArchive(ctx, "/state")
	if err != nil {
		return errors.New("another PITR base backup, recovery or retention pass is active")
	}
	defer unlock()
	if _, err := inspectPITRSource(ctx, config); err != nil {
		return err
	}
	connection, err := pitrConnection(ctx, config.Database)
	if err != nil {
		return err
	}
	_, err = connection.Exec(ctx, "SELECT pg_switch_wal()")
	connection.Close(ctx)
	if err != nil {
		return errors.New("PostgreSQL WAL could not advance before the base backup")
	}
	before, err := pitrBaseInventory(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if _, err := pitrWALG(ctx, "backup-push", "--wait-for-archive", "--json"); err != nil {
		return errors.New("encrypted PostgreSQL base backup failed; inspect capture status before retrying")
	}
	if err := capturePITRHeartbeat(ctx, config); err != nil {
		return errors.New("base backup was published but its recovery-window heartbeat failed; inspect capture status")
	}
	after, err := pitrBaseInventory(ctx)
	if err != nil {
		return err
	}
	previous := map[string]bool{}
	for _, base := range before {
		previous[base.Name] = true
	}
	for _, base := range after {
		if !previous[base.Name] && pitrBaseName.MatchString(base.Name) {
			return writePITRMetadata("/state/base-result.json", base)
		}
	}
	return errors.New("new PostgreSQL base backup was not published")
}
