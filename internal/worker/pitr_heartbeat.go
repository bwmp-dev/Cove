package worker

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"
)

type pitrHeartbeat struct {
	Archive   string    `json:"archive"`
	Timestamp time.Time `json:"timestamp"`
	WAL       string    `json:"wal"`
	LSN       string    `json:"lsn"`
}

func capturePITRHeartbeat(ctx context.Context, config pitrRunnerConfig) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := os.MkdirAll("/tmp/pitr/heartbeat", 0700); err != nil {
		return err
	}
	unlock, err := lockWorker("/tmp/pitr/heartbeat")
	if err != nil {
		return errors.New("another PITR heartbeat is active")
	}
	defer unlock()
	connection, err := pitrConnection(ctx, config.Database)
	if err != nil {
		return err
	}
	defer connection.Close(ctx)
	tx, err := connection.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var stamp time.Time
	var xid string
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp(),pg_current_xact_id()::text").Scan(&stamp, &xid); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	var wal, lsn string
	if err = connection.QueryRow(ctx, "WITH position AS MATERIALIZED (SELECT pg_current_wal_insert_lsn() AS lsn) SELECT pg_walfile_name(lsn),lsn::text FROM position").Scan(&wal, &lsn); err != nil {
		return err
	}
	if !pitrWALName.MatchString(wal) {
		return errors.New("invalid PITR heartbeat WAL segment")
	}
	if _, err = connection.Exec(ctx, "SELECT pg_switch_wal()"); err != nil {
		return err
	}
	var heartbeats []pitrHeartbeat
	if err = readSnapshotMetadata("/state/heartbeats.json", &heartbeats); err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(heartbeats) > 32 {
		return errors.New("PITR heartbeat history exceeds its limit")
	}
	if len(heartbeats) == 32 {
		heartbeats = heartbeats[1:]
	}
	heartbeats = append(heartbeats, pitrHeartbeat{Archive: config.Policy.Archive, Timestamp: stamp.UTC(), WAL: wal, LSN: lsn})
	return writePITRMetadata("/state/heartbeats.json", heartbeats)
}

func validPITRHeartbeat(config pitrRunnerConfig, heartbeat pitrHeartbeat) bool {
	position, err := parsePITRLSN(heartbeat.LSN)
	if heartbeat.Archive != config.Policy.Archive || !pitrWALName.MatchString(heartbeat.WAL) || heartbeat.Timestamp.IsZero() || heartbeat.Timestamp.After(time.Now().Add(time.Minute)) || err != nil || position == 0 {
		return false
	}
	timeline, err := strconv.ParseUint(heartbeat.WAL[:8], 16, 32)
	return err == nil && timeline > 0
}

func completedPITRHeartbeat(config pitrRunnerConfig, inventory pitrInventory) (*pitrHeartbeat, error) {
	var heartbeats []pitrHeartbeat
	if err := readSnapshotMetadata("/state/heartbeats.json", &heartbeats); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(heartbeats) > 32 {
		return nil, errors.New("PITR heartbeat history exceeds its limit")
	}
	wals := map[string]bool{}
	for _, wal := range inventory.WALNames {
		wals[wal] = true
	}
	var latest *pitrHeartbeat
	for _, heartbeat := range heartbeats {
		if !validPITRHeartbeat(config, heartbeat) {
			return nil, errors.New("invalid PITR heartbeat identity or timestamp")
		}
		if wals[heartbeat.WAL] && (latest == nil || heartbeat.Timestamp.After(latest.Timestamp)) {
			copy := heartbeat
			latest = &copy
		}
	}
	return latest, nil
}
