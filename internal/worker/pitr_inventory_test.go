package worker

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPITRInventoryAccountsObjectsAndRejectsUnsafeOrUnboundedInput(t *testing.T) {
	stamp := time.Now().UTC().Truncate(time.Second)
	objects := []pitrObject{{Path: "basebackups_005/base_000000010000000000000002/sentinel.json", Size: 10, ModTime: stamp}, {Path: "wal_005/000000010000000000000002.lz4", Size: 30, ModTime: stamp}}
	data, _ := json.Marshal(objects)
	inventory, err := readPITRInventory(strings.NewReader(string(data)))
	if err != nil || inventory.Bytes != 40 || inventory.Objects != 2 || !inventory.LastWAL.Equal(stamp) || len(inventory.WALNames) != 1 {
		t.Fatal(inventory, err)
	}
	partial := []pitrObject{{Path: "wal_005/000000010000000000000003.partial.lz4", Size: 30, ModTime: stamp}}
	data, _ = json.Marshal(partial)
	incomplete, err := readPITRInventory(strings.NewReader(string(data)))
	if err != nil || !incomplete.LastWAL.IsZero() || len(incomplete.WALNames) != 0 || incomplete.Bytes != 30 {
		t.Fatal("partial WAL advanced recovery window", incomplete, err)
	}
	for _, input := range []string{`[{"Path":"../outside","Size":1}]`, `[{"Path":"..","Size":1}]`, `[{"Path":"/outside","Size":1}]`, `[{"Path":"a/../outside","Size":1}]`, `[{"Path":"a","Size":-1}]`, `[{"Path":"a","Size":1},{"Path":"a","Size":1}]`, `[] true`, `[{"Path":"a","Size":1}`} {
		if _, err := readPITRInventory(strings.NewReader(input)); err == nil {
			t.Fatal("unsafe inventory accepted", input)
		}
	}
	objects = nil
	for i := 0; i <= pitrObjectLimit+pitrInventoryBatch; i++ {
		objects = append(objects, pitrObject{Path: fmt.Sprintf("backup_reservations/%08d", i)})
	}
	data, _ = json.Marshal(objects)
	if _, err := readPITRInventory(strings.NewReader(string(data))); err == nil {
		t.Fatal("unbounded archive inventory accepted")
	}
}

func TestPITRRetentionKeepsOneBaselineForTheWindowWithinCountLimit(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	settings := PITRSettings{Service: "db", IntervalHours: 24, KeepLast: 4, RetentionDays: 2, MaxBytes: 1 << 30}
	var bases []pitrStoredBase
	for i := 0; i < 6; i++ {
		wal := fmt.Sprintf("0000000100000000000000%02X", i)
		bases = append(bases, pitrStoredBase{Name: "base_" + wal, WAL: wal, Storage: "default", Time: now.Add(-time.Duration(5-i) * 24 * time.Hour)})
	}
	kept, err := retainedPITRBases(bases, settings, now)
	if err != nil || len(kept) != 4 || kept[0].Time != now || kept[3].Time != now.Add(-72*time.Hour) {
		t.Fatal("window baseline", kept, err)
	}
	settings.KeepLast = 2
	kept, err = retainedPITRBases(bases, settings, now)
	if err != nil || len(kept) != 2 {
		t.Fatal("count cap", kept, err)
	}
	kept, err = retainedPITRBases(bases, settings, now.Add(30*24*time.Hour))
	if err != nil || len(kept) != 1 {
		t.Fatal("newest stale base was removed", kept, err)
	}
	bases = append(bases, bases[0])
	if _, err := retainedPITRBases(bases, settings, now); err == nil {
		t.Fatal("ambiguous base identity accepted")
	}
}
