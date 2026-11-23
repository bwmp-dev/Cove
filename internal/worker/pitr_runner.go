package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const pitrStatusFile = "/state/status.json"

type pitrProgress struct {
	Archive  string    `json:"archive"`
	SystemID string    `json:"system_id"`
	LSN      uint64    `json:"lsn"`
	SeenAt   time.Time `json:"seen_at"`
}

func writePITRStatus(status PITRStatus) error {
	status.CheckedAt = time.Now().UTC()
	data, err := json.Marshal(status)
	if err != nil || len(data) > 65536 {
		return errors.New("PITR status exceeds its bounded size")
	}
	return writePITRMetadata(pitrStatusFile, status)
}

func parsePITRLSN(value string) (uint64, error) {
	high, low, ok := strings.Cut(value, "/")
	if !ok || len(high) > 8 || len(low) > 8 {
		return 0, errors.New("invalid PostgreSQL WAL position")
	}
	upper, err := strconv.ParseUint(high, 16, 32)
	if err != nil {
		return 0, errors.New("invalid PostgreSQL WAL position")
	}
	lower, err := strconv.ParseUint(low, 16, 32)
	if err != nil {
		return 0, errors.New("invalid PostgreSQL WAL position")
	}
	return upper<<32 | lower, nil
}

func inspectPITRSource(ctx context.Context, config pitrRunnerConfig) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	connection, err := pitrConnection(ctx, config.Database)
	if err != nil {
		return 0, err
	}
	defer connection.Close(ctx)
	var lsn string
	var recovery bool
	var limit int64
	if err := connection.QueryRow(ctx, `SELECT CASE WHEN pg_is_in_recovery() THEN '' ELSE pg_current_wal_lsn()::text END,pg_is_in_recovery(),pg_size_bytes(current_setting('max_slot_wal_keep_size'))`).Scan(&lsn, &recovery, &limit); err != nil {
		return 0, errors.New("PostgreSQL PITR settings could not be verified")
	}
	if recovery {
		return 0, errors.New("PITR capture requires the managed writable PostgreSQL primary")
	}
	if limit <= 0 || limit > pitrSlotBytes {
		return 0, errors.New("PostgreSQL WAL retention protection changed; capture is paused")
	}
	var kind string
	var state *string
	if err := connection.QueryRow(ctx, `SELECT slot_type,wal_status FROM pg_replication_slots WHERE slot_name=$1`, config.Database.Slot).Scan(&kind, &state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errors.New("PITR replication slot is missing; create a new base backup")
		}
		return 0, errors.New("PITR replication slot could not be verified")
	}
	if kind != "physical" || state == nil || (*state != "reserved" && *state != "extended") {
		return 0, errors.New("PITR replication slot lost retained WAL; create a new base backup")
	}
	return parsePITRLSN(lsn)
}

func releasePITRSlot(ctx context.Context, config pitrRunnerConfig) error {
	connection, err := pitrConnection(ctx, config.Database)
	if err != nil {
		return err
	}
	defer connection.Close(ctx)
	var kind string
	var active bool
	err = connection.QueryRow(ctx, `SELECT slot_type,active FROM pg_replication_slots WHERE slot_name=$1`, config.Database.Slot).Scan(&kind, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil || kind != "physical" || active {
		return errors.New("PITR replication slot is still active or changed")
	}
	if _, err := connection.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, config.Database.Slot); err != nil {
		return errors.New("PITR replication slot could not be released")
	}
	if _, err := connection.Exec(ctx, "CHECKPOINT"); err != nil {
		return errors.New("PITR slot was released; PostgreSQL checkpoint is pending")
	}
	return nil
}

func measurePITRRunner(ctx context.Context, config pitrRunnerConfig, broker *pitrBroker, status *PITRStatus) error {
	unlock, err := lockWorker("/state")
	if err != nil {
		status.State = "base_backup"
		return nil
	}
	defer unlock()
	epoch := broker.quota.epoch()
	bases, inventory, measureErr := maintainPITRArchive(ctx, config, broker.root, broker.environment)
	if measureErr != nil && !inventory.Complete {
		return measureErr
	}
	broker.quota.reconcile(epoch, inventory)
	stamp := time.Now().UTC()
	status.MeasuredAt = &stamp
	status.Bytes, status.Objects = inventory.Bytes, inventory.Objects
	status.LastArchiveAt = nil
	if !inventory.LastWAL.IsZero() {
		stamp := inventory.LastWAL
		status.LastArchiveAt = &stamp
	}
	windowEnd, err := completedPITRHeartbeat(config, inventory)
	if err != nil {
		return err
	}
	windows, err := readPITRWindows("/state/windows.json", config)
	if err != nil {
		return err
	}
	retained := map[string]pitrHeartbeat{}
	status.Bases = []PITRBase{}
	for _, base := range bases {
		observation := PITRBase{ID: config.Policy.Archive + "_" + base.Name, CreatedAt: base.Time, Bytes: inventory.BaseBytes[base.Name], PostgresMajor: config.Database.Major}
		if previous, ok := windows.Bases[base.Name]; ok && previous.Timestamp.After(base.Time) {
			stamp := previous.Timestamp
			observation.AvailableTo = &stamp
			retained[base.Name] = previous
		}
		status.Bases = append(status.Bases, observation)
	}
	status.NeedsBase = len(bases) == 0
	if len(bases) > 0 && len(inventory.WALNames) > 0 {
		indices := []int{0}
		if len(bases) > 1 {
			indices = append(indices, 1+status.verificationCursor%(len(bases)-1))
			status.verificationCursor++
		}
		for _, index := range indices {
			base := bases[index]
			previous, hadPrevious := retained[base.Name]
			candidate := windowEnd
			if hadPrevious && (candidate == nil || previous.Timestamp.After(candidate.Timestamp)) {
				candidate = &previous
			}
			if !verifyPITRWindow(ctx, base, candidate) {
				if hadPrevious && (candidate == nil || !candidate.Timestamp.Equal(previous.Timestamp)) && verifyPITRWindow(ctx, base, &previous) {
					candidate = &previous
				} else {
					candidate = nil
				}
			}
			delete(retained, base.Name)
			status.Bases[index].AvailableTo = nil
			if candidate != nil {
				stamp := candidate.Timestamp
				status.Bases[index].AvailableTo = &stamp
				retained[base.Name] = *candidate
			}
		}
	}
	windows.Bases = retained
	if err := writePITRMetadata("/state/windows.json", windows); err != nil {
		return err
	}
	status.Gaps = false
	for _, base := range status.Bases {
		status.Gaps = status.Gaps || base.AvailableTo == nil || windowEnd != nil && base.AvailableTo.Before(windowEnd.Timestamp)
	}
	if len(status.Bases) > 0 && status.Bases[0].AvailableTo == nil {
		status.NeedsBase = true
	}
	return measureErr
}

func RunPITRRunner(ctx context.Context) (result error) {
	var config pitrRunnerConfig
	if err := readSnapshotMetadata("/state/config.json", &config); err != nil {
		return err
	}
	if err := validatePITRRunner(config); err != nil {
		return err
	}
	var progress pitrProgress
	if err := readSnapshotMetadata("/state/progress.json", &progress); err != nil && !os.IsNotExist(err) {
		return err
	}
	if progress.Archive == config.Policy.Archive {
		if progress.SystemID != config.Database.SystemID || progress.SeenAt.After(time.Now().Add(time.Minute)) {
			return errors.New("PITR archive identity or observation stamp changed")
		}
	} else {
		progress = pitrProgress{Archive: config.Policy.Archive, SystemID: config.Database.SystemID}
	}
	status := PITRStatus{Enabled: config.Policy.Settings.Enabled, State: "starting", NeedsBase: true, Bases: []PITRBase{}}
	if err := writePITRStatus(status); err != nil {
		return err
	}
	broker, err := startPITRBroker(ctx, config)
	if err != nil {
		status.State, status.LastError = "error", err.Error()
		return errors.Join(err, writePITRStatus(status))
	}
	defer func() { result = errors.Join(result, broker.close()) }()
	var receiver *pitrChild
	defer func() { receiver.stop() }()
	lastLSN := progress.LSN
	nextMeasure, nextSwitch, nextSource, nextWrite, nextRelease := time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}
	retryAt := time.Time{}
	blocked := false
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if config.Policy.Settings.Enabled && !blocked && time.Now().After(retryAt) && time.Now().After(nextSource) {
			nextSource = time.Now().Add(5 * time.Second)
			lsn, err := inspectPITRSource(ctx, config)
			if err != nil {
				receiver.stop()
				receiver = nil
				status.Receiver = false
				status.State, status.LastError = "paused", err.Error()
				status.NeedsBase = true
				retryAt = time.Now().Add(30 * time.Second)
			} else if lastLSN != 0 && lsn < lastLSN {
				receiver.stop()
				receiver = nil
				status.Receiver = false
				blocked = true
				status.State, status.LastError = "paused", "PostgreSQL WAL moved backwards; begin a new PITR archive"
			} else {
				lastLSN = lsn
				progress.LSN, progress.SeenAt = lsn, time.Now().UTC()
				if err := writePITRMetadata("/state/progress.json", progress); err != nil {
					return err
				}
				if receiver == nil {
					receiver, err = startPITRChild(ctx, []string{"wal-g", "--config", pitrWALGFile, "wal-receive"}, nil)
					if err != nil {
						status.State, status.LastError = "error", "PITR WAL receiver could not start"
						retryAt = time.Now().Add(30 * time.Second)
					} else {
						status.Receiver = true
						status.State, status.LastError = "capturing", ""
					}
				}
			}
		}
		if !config.Policy.Settings.Enabled {
			status.State, status.Receiver = "disabled", false
		}
		if time.Now().After(nextMeasure) {
			if err := measurePITRRunner(ctx, config, broker, &status); err != nil {
				receiver.stop()
				receiver = nil
				status.Receiver = false
				status.NeedsBase = true
				blocked = true
				status.State, status.LastError = "paused", err.Error()
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				if err := releasePITRSlot(cleanup, config); err != nil {
					status.LastError += "; slot cleanup will retry when PostgreSQL is reachable"
				}
				cancel()
			}
			nextMeasure = time.Now().Add(10 * time.Second)
		}
		if blocked && config.Policy.Settings.Enabled && time.Now().After(nextRelease) {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			if err := releasePITRSlot(cleanup, config); err != nil {
				nextRelease = time.Now().Add(30 * time.Second)
			} else {
				nextRelease = time.Now().Add(time.Hour)
			}
			cancel()
		}
		if !blocked && receiver != nil {
			if status.NeedsBase {
				status.State = "waiting_for_base"
			} else if status.State == "base_backup" || status.State == "waiting_for_base" {
				status.State = "capturing"
			}
		}
		if receiver != nil && time.Now().After(nextSwitch) {
			err := capturePITRHeartbeat(ctx, config)
			const delayed = "PITR WAL switching is delayed; the latest partial segment is not a recovery point"
			if err != nil {
				status.LastError = delayed
			} else if status.LastError == delayed {
				status.LastError = ""
			}
			nextSwitch = time.Now().Add(time.Minute)
		}
		if time.Now().After(nextWrite) {
			if err := writePITRStatus(status); err != nil {
				return err
			}
			nextWrite = time.Now().Add(5 * time.Second)
		}
		var receiverDone <-chan struct{}
		if receiver != nil {
			receiverDone = receiver.done
		}
		select {
		case <-ctx.Done():
			return nil
		case <-broker.child.done:
			status.State, status.LastError = "error", "PITR storage bridge stopped unexpectedly"
			return errors.Join(errors.New(status.LastError), writePITRStatus(status))
		case <-broker.done:
			status.State, status.LastError = "error", "PITR archive admission server stopped unexpectedly"
			return errors.Join(errors.New(status.LastError), writePITRStatus(status))
		case <-receiverDone:
			if receiver.err != nil {
				status.LastError = "PITR WAL receiver failed; capture will retry"
			} else {
				status.LastError = "PITR WAL receiver exited; capture will retry"
			}
			receiver.stop()
			receiver = nil
			status.Receiver = false
			status.State = "paused"
			retryAt = time.Now().Add(30 * time.Second)
		case <-broker.quota.reached:
			receiver.stop()
			receiver = nil
			status.Receiver = false
			blocked = true
			status.NeedsBase = true
			status.State, status.LastError = "paused", "PITR archive admission limit reached; increase storage limits or begin a new archive"
		case <-ticker.C:
		}
	}
}
