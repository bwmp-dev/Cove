package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFilesystemSnapshotInspectionMapsBindMountsAndRejectsIncompleteLayouts(t *testing.T) {
	for _, kind := range []string{"btrfs", "zfs", "nested-zfs", "lvm", "nested-btrfs", "whole-btrfs", "thin-lvm", "low-space", "unsupported"} {
		t.Run(kind, func(t *testing.T) {
			source := snapshotSource{Source: "/var/lib/docker/volumes/app/_data", Label: "app"}
			run := func(_ context.Context, args ...string) (string, error) {
				switch args[0] {
				case "findmnt":
					fs, device, root := "btrfs", "/dev/loop0", "/@/data"
					if kind == "zfs" || kind == "nested-zfs" {
						fs, device, root = "zfs", "pool/data", "/records"
					}
					if strings.Contains(kind, "lvm") || kind == "low-space" {
						fs, device, root = "ext4", "/dev/vg/app", "/records"
					}
					if kind == "whole-btrfs" {
						root = "/"
					}
					if kind == "unsupported" {
						fs = "overlay"
					}
					return `{"filesystems":[{"source":"` + device + `","fstype":"` + fs + `","target":"` + source.Source + `","fsroot":"` + root + `","uuid":"12345678-1234-1234-1234-123456789abc"}]}`, nil
				case "btrfs":
					if args[1] == "inspect-internal" && args[2] == "rootid" {
						if kind == "whole-btrfs" {
							return "5\n", nil
						}
						return "256\n", nil
					}
					return "@\n", nil
				case "find":
					if kind == "nested-btrfs" {
						return "/var/lib/docker/volumes/app/_data/child\n", nil
					}
					return "", nil
				case "zfs":
					if args[1] == "list" {
						if kind == "nested-zfs" {
							return "pool/data\npool/data/child\n", nil
						}
						return "pool/data\n", nil
					}
					return "/pool/data\n", nil
				case "lvs":
					attr := "-wi-a-----"
					if kind == "thin-lvm" {
						attr = "Vwi-a-tz--"
					}
					return `{"report":[{"lv":[{"vg_name":"vg","lv_name":"app","lv_uuid":"abc-def","lv_attr":"` + attr + `","lv_path":"/dev/vg/app","origin":""}]}]}`, nil
				case "vgs":
					if kind == "low-space" {
						return `{"report":[{"vg":[{"vg_free":"100"}]}]}`, nil
					}
					return `{"report":[{"vg":[{"vg_free":"1342177280.00"}]}]}`, nil
				default:
					return "", errors.New("unexpected command")
				}
			}
			plan, err := inspectSnapshotMount(context.Background(), source, 1<<30, run)
			if err != nil {
				t.Fatal(err)
			}
			available := kind == "btrfs" || kind == "zfs" || kind == "lvm"
			if plan.Available != available {
				t.Fatalf("available=%v: %s", plan.Available, plan.Reason)
			}
			if !available && plan.Reason == "" {
				t.Fatal("unsupported layout has no reason")
			}
			if kind == "btrfs" && (plan.DataPath != "data" || plan.Subvolume != "@" || plan.SubvolumeID != 256) {
				t.Fatal("Btrfs bind mapping changed", plan)
			}
			if kind == "zfs" && (plan.Dataset != "pool/data" || plan.DatasetMount != "/pool/data" || plan.DataPath != "records") {
				t.Fatal("ZFS bind mapping changed", plan)
			}
			if kind == "lvm" && (plan.VG != "vg" || plan.LV != "app" || plan.DataPath != "records") {
				t.Fatal("LVM bind mapping changed", plan)
			}
		})
	}
}

func TestFilesystemSnapshotBoundsAndPaths(t *testing.T) {
	for _, value := range []int64{-1, 1, 64<<20 - 1, MaxTransferBytes + 1} {
		if validateVolumeSnapshot(&VolumeSnapshotConfig{MaxBytes: value}) == nil {
			t.Fatal("snapshot limit accepted", value)
		}
	}
	for _, path := range []string{"/etc", "../data", "data/../secret", "data\\secret", "data\x00"} {
		if snapshotRelative(path) {
			t.Fatal("unsafe snapshot mapping accepted", path)
		}
	}
	for _, path := range []string{".", "", "data", "data/records"} {
		if !snapshotRelative(path) {
			t.Fatal("safe snapshot mapping rejected", path)
		}
	}
	called := false
	_, err := inspectSnapshotMount(context.Background(), snapshotSource{Source: "relative/data"}, 0, func(context.Context, ...string) (string, error) { called = true; return "", nil })
	if err == nil || called {
		t.Fatal("invalid source reached host commands")
	}
}
