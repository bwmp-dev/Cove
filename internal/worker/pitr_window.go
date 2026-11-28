package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"time"
)

type pitrWindows struct {
	Archive  string                   `json:"archive"`
	SystemID string                   `json:"system_id"`
	Bases    map[string]pitrHeartbeat `json:"bases"`
}

func readPITRWindows(filename string, config pitrRunnerConfig) (pitrWindows, error) {
	state := pitrWindows{Archive: config.Policy.Archive, SystemID: config.Database.SystemID, Bases: map[string]pitrHeartbeat{}}
	if err := readSnapshotMetadata(filename, &state); os.IsNotExist(err) {
		return state, nil
	} else if err != nil {
		return state, err
	}
	if state.Archive != config.Policy.Archive || state.SystemID != config.Database.SystemID || len(state.Bases) > 32 {
		return state, errors.New("PITR window receipt identity or count is invalid")
	}
	for base, heartbeat := range state.Bases {
		if !pitrBaseName.MatchString(base) || !validPITRHeartbeat(config, heartbeat) {
			return state, errors.New("invalid retained PITR window receipt")
		}
	}
	if state.Bases == nil {
		state.Bases = map[string]pitrHeartbeat{}
	}
	return state, nil
}

func verifyPITRWindow(ctx context.Context, base pitrStoredBase, heartbeat *pitrHeartbeat) bool {
	if heartbeat == nil || !heartbeat.Timestamp.After(base.Time) {
		return false
	}
	timeline, err := strconv.ParseUint(heartbeat.WAL[:8], 16, 32)
	if err != nil {
		return false
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// A retained window must remain verifiable after its source socket disappears or later WAL has gaps.
	data, err := pitrWALG(check, "wal-verify", "integrity", "--backup-name", base.Name, "--timeline", strconv.FormatUint(timeline, 10), "--lsn", heartbeat.LSN, "--json")
	var report struct {
		Integrity struct {
			Status string `json:"status"`
		} `json:"integrity"`
	}
	return err == nil && json.Unmarshal(data, &report) == nil && report.Integrity.Status == "OK"
}
