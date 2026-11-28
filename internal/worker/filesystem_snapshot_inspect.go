package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const snapshotMountLimit = 16
const snapshotDefaultBytes int64 = 1 << 30
const snapshotReserveBytes int64 = 256 << 20

type VolumeSnapshotConfig struct {
	MaxBytes int64 `json:"max_bytes"`
}

type SnapshotCapability struct {
	Mount      string `json:"mount"`
	Filesystem string `json:"filesystem"`
	Method     string `json:"method"`
	Available  bool   `json:"available"`
	Reason     string `json:"reason,omitempty"`
}

type SnapshotPreview struct {
	Available bool                 `json:"available"`
	Mounts    []SnapshotCapability `json:"mounts"`
	Reason    string               `json:"reason,omitempty"`
}

type snapshotSource struct {
	Source string `json:"source"`
	Label  string `json:"label"`
}

type snapshotMountPlan struct {
	snapshotSource
	Filesystem   string `json:"filesystem"`
	Method       string `json:"method"`
	Device       string `json:"device"`
	UUID         string `json:"uuid,omitempty"`
	DataPath     string `json:"data_path"`
	Subvolume    string `json:"subvolume,omitempty"`
	SubvolumeID  uint64 `json:"subvolume_id,omitempty"`
	Dataset      string `json:"dataset,omitempty"`
	DatasetMount string `json:"dataset_mount,omitempty"`
	VG           string `json:"vg,omitempty"`
	LV           string `json:"lv,omitempty"`
	LVUUID       string `json:"lv_uuid,omitempty"`
	Available    bool   `json:"available"`
	Reason       string `json:"reason,omitempty"`
}

type snapshotCommand func(context.Context, ...string) (string, error)

var filesystemName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:/+-]{0,255}$`)
var filesystemUUID = regexp.MustCompile(`^[a-fA-F0-9-]{16,64}$`)
var logicalVolumeName = regexp.MustCompile(`^[a-zA-Z0-9_+][a-zA-Z0-9_.+-]{0,126}$`)
var logicalVolumeUUID = regexp.MustCompile(`^[a-zA-Z0-9-]{1,128}$`)

func validateVolumeSnapshot(config *VolumeSnapshotConfig) error {
	if config != nil && config.MaxBytes != 0 && (config.MaxBytes < 64<<20 || config.MaxBytes > MaxTransferBytes) {
		return errors.New("LVM snapshot space must be 64 MiB–64 GiB")
	}
	return nil
}

func snapshotBytes(config *VolumeSnapshotConfig) int64 {
	if config == nil || config.MaxBytes == 0 {
		return snapshotDefaultBytes
	}
	return config.MaxBytes
}

func snapshotRelative(value string) bool {
	if len(value) > 8192 || strings.ContainsAny(value, "\x00\r\n\\") || path.IsAbs(value) {
		return false
	}
	if value == "." || value == "" {
		return true
	}
	return path.Clean(value) == value && value != ".." && !strings.HasPrefix(value, "../")
}

func snapshotPathRelative(root, value string) (string, error) {
	if !path.IsAbs(root) || !path.IsAbs(value) || path.Clean(root) != root || path.Clean(value) != value {
		return "", errors.New("invalid filesystem path")
	}
	if root == value {
		return ".", nil
	}
	prefix := strings.TrimSuffix(root, "/") + "/"
	if !strings.HasPrefix(value, prefix) {
		return "", errors.New("filesystem path is outside its root")
	}
	return strings.TrimPrefix(value, prefix), nil
}

func inspectSnapshotMount(ctx context.Context, source snapshotSource, bytes int64, run snapshotCommand) (snapshotMountPlan, error) {
	plan := snapshotMountPlan{snapshotSource: source}
	if bytes == 0 {
		bytes = snapshotDefaultBytes
	}
	if err := validateVolumeSnapshot(&VolumeSnapshotConfig{MaxBytes: bytes}); err != nil {
		return plan, err
	}
	if !path.IsAbs(source.Source) || path.Clean(source.Source) != source.Source || len(source.Source) > 8192 || strings.ContainsAny(source.Source, "\x00\r\n") {
		return plan, errors.New("invalid snapshot source path")
	}
	raw, err := run(ctx, "findmnt", "--json", "--output", "SOURCE,FSTYPE,TARGET,FSROOT,UUID", "--target", source.Source)
	if err != nil {
		plan.Reason = "Host findmnt is unavailable or could not inspect this filesystem"
		return plan, nil
	}
	var mounts struct {
		Filesystems []struct {
			Source string `json:"source"`
			FSType string `json:"fstype"`
			Target string `json:"target"`
			Root   string `json:"fsroot"`
			UUID   string `json:"uuid"`
		} `json:"filesystems"`
	}
	if json.Unmarshal([]byte(raw), &mounts) != nil || len(mounts.Filesystems) != 1 {
		return plan, errors.New("invalid host filesystem inspection")
	}
	mount := mounts.Filesystems[0]
	if mount.FSType == "" || mount.Source == "" {
		return plan, errors.New("missing host filesystem identity")
	}
	plan.Filesystem, plan.Device, plan.UUID = mount.FSType, mount.Source, mount.UUID
	if !path.IsAbs(mount.Target) || !path.IsAbs(mount.Root) || strings.ContainsAny(mount.Target+mount.Root, "\x00\r\n") {
		return plan, errors.New("invalid filesystem mount root")
	}
	relative, err := snapshotPathRelative(mount.Target, source.Source)
	if err != nil || !snapshotRelative(relative) {
		return plan, errors.New("snapshot source is outside its reported mount")
	}
	within := strings.TrimPrefix(path.Join(mount.Root, relative), "/")
	if !snapshotRelative(within) {
		return plan, errors.New("invalid filesystem data path")
	}
	plan.DataPath = within
	switch mount.FSType {
	case "btrfs":
		if !filesystemUUID.MatchString(plan.UUID) {
			plan.Reason = "Btrfs filesystem identity is unavailable"
			return plan, nil
		}
		id, err := run(ctx, "btrfs", "inspect-internal", "rootid", source.Source)
		if err != nil {
			plan.Reason = "Host Btrfs tools are unavailable"
			return plan, nil
		}
		plan.SubvolumeID, err = strconv.ParseUint(strings.TrimSpace(id), 10, 64)
		if err != nil || plan.SubvolumeID < 5 {
			return plan, errors.New("invalid Btrfs subvolume identity")
		}
		if plan.SubvolumeID != 5 {
			subvolumePath, err := run(ctx, "btrfs", "inspect-internal", "subvolid-resolve", strconv.FormatUint(plan.SubvolumeID, 10), source.Source)
			if err != nil {
				plan.Reason = "Btrfs subvolume path could not be resolved"
				return plan, nil
			}
			plan.Subvolume = strings.TrimSuffix(subvolumePath, "\n")
			if !snapshotRelative(plan.Subvolume) || plan.Subvolume == "" || plan.Subvolume == "." {
				return plan, errors.New("invalid Btrfs subvolume path")
			}
		}
		data, err := snapshotPathRelative("/"+plan.Subvolume, "/"+plan.DataPath)
		if err != nil || !snapshotRelative(data) {
			return plan, errors.New("Btrfs data is outside its subvolume")
		}
		if plan.SubvolumeID == 5 {
			plan.Reason = "An entire Btrfs top-level volume requires a dedicated data subvolume"
			return plan, nil
		}
		plan.DataPath = data
		nested, err := run(ctx, "find", source.Source, "-mindepth", "1", "-type", "d", "-inum", "256", "-print", "-quit")
		if err != nil || strings.TrimSpace(nested) != "" {
			plan.Reason = "Nested Btrfs subvolumes require a stopped backup to include all data"
			return plan, nil
		}
		plan.Method = "btrfs"
	case "zfs":
		if !filesystemName.MatchString(mount.Source) {
			return plan, errors.New("invalid ZFS dataset identity")
		}
		datasetPath, err := run(ctx, "zfs", "get", "-H", "-o", "value", "mountpoint", mount.Source)
		if err != nil {
			plan.Reason = "Host ZFS tools are unavailable"
			return plan, nil
		}
		plan.DatasetMount = strings.TrimSuffix(datasetPath, "\n")
		if !path.IsAbs(plan.DatasetMount) || strings.ContainsAny(plan.DatasetMount, "\x00\r\n") {
			plan.Reason = "ZFS legacy or unmounted datasets require a stopped backup"
			return plan, nil
		}
		plan.Dataset, plan.Method = mount.Source, "zfs"
		datasets, err := run(ctx, "zfs", "list", "-H", "-o", "name", "-r", mount.Source)
		if err != nil || strings.TrimSpace(datasets) != mount.Source {
			plan.Reason = "Nested or unavailable ZFS datasets require a stopped backup to include all data"
			return plan, nil
		}
	case "ext4", "xfs":
		device, _, _ := strings.Cut(mount.Source, "[")
		if !strings.HasPrefix(device, "/dev/") || strings.ContainsAny(device, "\x00\r\n") {
			plan.Reason = "Filesystem is not on an inspectable LVM device"
			return plan, nil
		}
		lv, err := run(ctx, "lvs", "--readonly", "--reportformat", "json", "--units", "b", "--nosuffix", "-o", "vg_name,lv_name,lv_uuid,lv_attr,lv_path,origin", "--", device)
		if err != nil {
			plan.Reason = "Host LVM tools or a supported logical volume are unavailable"
			return plan, nil
		}
		var report struct {
			Report []struct {
				LV []struct {
					VG     string `json:"vg_name"`
					Name   string `json:"lv_name"`
					UUID   string `json:"lv_uuid"`
					Attr   string `json:"lv_attr"`
					Path   string `json:"lv_path"`
					Origin string `json:"origin"`
				} `json:"lv"`
			} `json:"report"`
		}
		if json.Unmarshal([]byte(lv), &report) != nil || len(report.Report) != 1 || len(report.Report[0].LV) != 1 {
			plan.Reason = "Filesystem is not backed by a logical volume"
			return plan, nil
		}
		entry := report.Report[0].LV[0]
		if entry.Origin != "" || len(entry.Attr) < 1 || !strings.ContainsAny(entry.Attr[:1], "-omMrR") {
			plan.Reason = "Thin volumes and existing LVM snapshots require a stopped backup"
			return plan, nil
		}
		if !logicalVolumeName.MatchString(entry.VG) || !logicalVolumeName.MatchString(entry.Name) || !logicalVolumeUUID.MatchString(entry.UUID) || !strings.HasPrefix(entry.Path, "/dev/") || path.Clean(entry.Path) != entry.Path || strings.ContainsAny(entry.Path, "\x00\r\n") {
			return plan, errors.New("invalid logical volume identity")
		}
		free, err := run(ctx, "vgs", "--readonly", "--reportformat", "json", "--units", "b", "--nosuffix", "-o", "vg_free", "--", entry.VG)
		if err != nil {
			plan.Reason = "LVM free space could not be read"
			return plan, nil
		}
		var vg struct {
			Report []struct {
				VG []struct {
					Free string `json:"vg_free"`
				} `json:"vg"`
			} `json:"report"`
		}
		if json.Unmarshal([]byte(free), &vg) != nil || len(vg.Report) != 1 || len(vg.Report[0].VG) != 1 {
			return plan, errors.New("invalid LVM space report")
		}
		available, err := strconv.ParseFloat(strings.TrimSpace(vg.Report[0].VG[0].Free), 64)
		if err != nil || math.IsNaN(available) || math.IsInf(available, 0) || available < float64(bytes+snapshotReserveBytes) {
			plan.Reason = "LVM needs the configured snapshot space plus a 256 MiB reserve"
			return plan, nil
		}
		plan.VG, plan.LV, plan.LVUUID, plan.Device, plan.Method = entry.VG, entry.Name, entry.UUID, entry.Path, "lvm"
	default:
		plan.Reason = fmt.Sprintf("%s does not provide a supported filesystem snapshot method", mount.FSType)
		return plan, nil
	}
	plan.Available = true
	return plan, nil
}
