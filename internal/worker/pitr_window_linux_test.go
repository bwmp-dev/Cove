package worker

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPITRWindowReceiptsBindArchiveClusterAndBoundHistory(t *testing.T) {
	config := pitrRunnerConfig{Policy: PITRPolicy{Archive: strings.Repeat("a1", 16)}, Database: pitrDatabase{SystemID: "123456"}}
	heartbeat := pitrHeartbeat{Archive: config.Policy.Archive, Timestamp: time.Now().UTC(), WAL: "000000010000000000000002", LSN: "0/2000100"}
	filename := filepath.Join(t.TempDir(), "windows.json")
	valid := pitrWindows{Archive: config.Policy.Archive, SystemID: config.Database.SystemID, Bases: map[string]pitrHeartbeat{"base_000000010000000000000001": heartbeat}}
	if err := writeJSON(filename, valid); err != nil {
		t.Fatal(err)
	}
	loaded, err := readPITRWindows(filename, config)
	if err != nil || len(loaded.Bases) != 1 {
		t.Fatal("retained window receipt unreadable", loaded, err)
	}
	for _, change := range []func(*pitrWindows){func(state *pitrWindows) { state.SystemID = "another" }, func(state *pitrWindows) { state.Archive = strings.Repeat("b2", 16) }, func(state *pitrWindows) {
		bad := heartbeat
		bad.LSN = ""
		state.Bases["base_000000010000000000000001"] = bad
	}, func(state *pitrWindows) {
		for i := 1; i <= 33; i++ {
			state.Bases[fmt.Sprintf("base_0000000100000000%08X", i)] = heartbeat
		}
	}} {
		state := valid
		state.Bases = map[string]pitrHeartbeat{"base_000000010000000000000001": heartbeat}
		change(&state)
		if err := writeJSON(filename, state); err != nil {
			t.Fatal(err)
		}
		if _, err := readPITRWindows(filename, config); err == nil {
			t.Fatal("invalid window identity or unbounded history accepted")
		}
	}
}
