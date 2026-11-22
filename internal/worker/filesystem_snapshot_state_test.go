package worker

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFilesystemSnapshotRecoveryBoundsAndIdentity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("recovery metadata requires Linux permission semantics")
	}
	root := t.TempDir()
	owner := strings.Repeat("a", 32)
	workload := "resource"
	if err := os.MkdirAll(filepath.Join(root, "workloads", workload), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(snapshotScope(root), 0700); err != nil {
		t.Fatal(err)
	}
	state := snapshotHostState{Version: 1, Root: root, Owner: owner, Workload: workload, Operation: "snapshot-operation", CreatedAt: time.Now().UTC(), Containers: []string{strings.Repeat("1", 12)}}
	state.ExpiresAt = state.CreatedAt.Add(snapshotLease)
	name := snapshotResourceName(state, 0)
	state.Resources = []snapshotHostResource{{Name: name, Mount: path.Join(snapshotScope(root), workload, name), Plan: snapshotMountPlan{Method: "lvm", Available: true, DataPath: "data", VG: "fixture", LV: "data", LVUUID: "abc-def"}}}
	if err := writeJSON(snapshotStateFile(root, workload), state); err != nil {
		t.Fatal(err)
	}
	if _, err := readSnapshotState(root, owner, workload); err != nil {
		t.Fatal("valid journal", err)
	}
	state.Resources[0].Mount = "/unrelated"
	if err := writeJSON(snapshotStateFile(root, workload), state); err != nil {
		t.Fatal(err)
	}
	if _, err := readSnapshotState(root, owner, workload); err == nil {
		t.Fatal("unrelated mount accepted")
	}
	index := snapshotHostIndex{Owner: owner, Workloads: []string{workload, workload}}
	if err := writeJSON(path.Join(snapshotScope(root), "index.json"), index); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotIndex(root, owner); err == nil {
		t.Fatal("duplicate recovery session accepted")
	}
	index.Workloads = nil
	for i := 0; i <= snapshotSessionLimit; i++ {
		index.Workloads = append(index.Workloads, strings.Repeat("a", i+1))
	}
	if err := writeJSON(path.Join(snapshotScope(root), "index.json"), index); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotIndex(root, owner); err == nil {
		t.Fatal("unbounded recovery sessions accepted")
	}
}

func TestFilesystemSnapshotLVOverflowNeverValidatesAndForeignVolumesAreKept(t *testing.T) {
	resource := snapshotHostResource{Name: "cove-snapshot-fixture", Plan: snapshotMountPlan{VG: "fixture", LV: "data", Method: "lvm"}}
	owner := strings.Repeat("a", 32)
	for _, kind := range []string{"healthy", "overflow", "foreign", "missing", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			run := func(_ context.Context, args ...string) (string, error) {
				if args[0] != "lvs" {
					t.Fatal("unexpected mutation", args)
				}
				if kind == "unavailable" {
					return "", errors.New("LVM unavailable")
				}
				if kind == "missing" {
					return `{"report":[{"lv":[]}]}`, nil
				}
				tag, percent, attr := "cove_owner_"+owner, "25.0", "swi-a-s---"
				if kind == "overflow" {
					percent = "100.0"
					attr = "swi-I-s---"
				}
				if kind == "foreign" {
					tag = "unrelated"
				}
				return `{"report":[{"lv":[{"lv_name":"cove-snapshot-fixture","origin":"data","lv_tags":"` + tag + `","lv_attr":"` + attr + `","data_percent":"` + percent + `"}]}]}`, nil
			}
			exists, err := snapshotLV(context.Background(), resource, owner, run)
			if (kind == "healthy" || kind == "overflow") != exists {
				t.Fatal("snapshot ownership", exists, err)
			}
			if (kind == "foreign" || kind == "overflow" || kind == "unavailable") != (err != nil) {
				t.Fatal("unsafe snapshot accepted", err)
			}
		})
	}
}
