package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

func snapshotResourceName(state snapshotHostState, index int) string {
	digest := snapshotUnitName(state.Owner + ":" + state.Operation)
	return digest + "-" + strconv.Itoa(index)
}

func snapshotMounted(ctx context.Context, target string, run snapshotCommand) (bool, error) {
	raw, err := run(ctx, "findmnt", "--json", "--mountpoint", target, "--output", "TARGET")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	var report struct{ Filesystems []struct{ Target string } }
	if json.Unmarshal([]byte(raw), &report) != nil || len(report.Filesystems) != 1 || report.Filesystems[0].Target != target {
		return false, errors.New("ambiguous filesystem snapshot mount")
	}
	return true, nil
}

func snapshotLV(ctx context.Context, resource snapshotHostResource, owner string, run snapshotCommand) (bool, error) {
	raw, err := run(ctx, "lvs", "--reportformat", "json", "-o", "lv_name,origin,lv_tags,lv_attr,data_percent", "--", resource.Plan.VG)
	if err != nil {
		return false, err
	}
	var report struct {
		Report []struct {
			LV []struct {
				Name    string `json:"lv_name"`
				Origin  string `json:"origin"`
				Tags    string `json:"lv_tags"`
				Attr    string `json:"lv_attr"`
				Percent string `json:"data_percent"`
			} `json:"lv"`
		} `json:"report"`
	}
	if json.Unmarshal([]byte(raw), &report) != nil || len(report.Report) != 1 {
		return false, errors.New("invalid LVM snapshot report")
	}
	entries := report.Report[0].LV
	position := -1
	for i, entry := range entries {
		if entry.Name == resource.Name {
			if position >= 0 {
				return false, errors.New("ambiguous LVM snapshot identity")
			}
			position = i
		}
	}
	if position < 0 {
		return false, nil
	}
	lv := entries[position]
	owned := false
	for _, tag := range strings.Split(lv.Tags, ",") {
		owned = owned || strings.TrimSpace(tag) == "cove_owner_"+owner
	}
	if !owned || lv.Name != resource.Name || lv.Name == resource.Plan.LV || (lv.Origin != resource.Plan.LV && lv.Origin != "") {
		return false, errors.New("LVM snapshot ownership changed")
	}
	if lv.Origin == "" {
		return true, errors.New("LVM snapshot creation is incomplete")
	}
	if len(lv.Attr) < 5 || lv.Attr[0] != 's' || lv.Attr[4] != 'a' {
		return true, errors.New("LVM snapshot is invalid or overflowed")
	}
	percent, err := strconv.ParseFloat(strings.TrimSpace(lv.Percent), 64)
	if err != nil || percent >= 100 || percent < 0 {
		return true, errors.New("LVM snapshot capacity exhausted or unknown")
	}
	return true, nil
}

func createSnapshotResource(ctx context.Context, state snapshotHostState, index int, bytes int64, run snapshotCommand) (snapshotHostResource, error) {
	r := state.Resources[index]
	if err := os.MkdirAll(r.Mount, 0700); err != nil {
		return r, err
	}
	p := r.Plan
	if _, err := run(ctx, "sync", "-f", p.Source); err != nil {
		return r, err
	}
	switch p.Method {
	case "btrfs":
		if _, err := run(ctx, "mount", "-t", "btrfs", "-o", "subvolid=5", "UUID="+p.UUID, r.Mount); err != nil {
			return r, err
		}
		parent := path.Join(r.Mount, ".cove-snapshots-"+state.Owner)
		if info, err := os.Lstat(parent); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
				return r, errors.New("unsafe Btrfs snapshot workspace")
			}
			owner, err := os.ReadFile(path.Join(parent, "owner"))
			if err != nil || string(owner) != state.Owner {
				return r, errors.New("Btrfs snapshot workspace ownership changed")
			}
		} else if os.IsNotExist(err) {
			if err := os.Mkdir(parent, 0700); err != nil {
				return r, err
			}
			if err := os.WriteFile(path.Join(parent, "owner"), []byte(state.Owner), 0600); err != nil {
				return r, err
			}
		} else {
			return r, err
		}
		target := path.Join(parent, r.Name)
		if _, err := os.Lstat(target); err == nil || !os.IsNotExist(err) {
			return r, errors.New("filesystem snapshot target already exists")
		}
		if _, err := run(ctx, "btrfs", "subvolume", "snapshot", "-r", path.Join(r.Mount, p.Subvolume), target); err != nil {
			return r, err
		}
		r.Source = path.Join(target, p.DataPath)
	case "zfs":
		target := p.Dataset + "@" + r.Name
		if _, err := run(ctx, "zfs", "snapshot", "-o", "cove:owner="+state.Owner, "-o", "cove:operation="+state.Operation, target); err != nil {
			return r, err
		}
		r.Source = path.Join(p.DatasetMount, ".zfs", "snapshot", r.Name, p.DataPath)
	case "lvm":
		if _, err := run(ctx, "lvcreate", "--snapshot", "--size", strconv.FormatInt(bytes, 10)+"B", "--name", r.Name, "--addtag", "cove_owner_"+state.Owner, "--", p.Device); err != nil {
			return r, err
		}
		options := "ro"
		if p.Filesystem == "xfs" {
			options = "ro,nouuid"
		}
		if _, err := run(ctx, "mount", "-t", p.Filesystem, "-o", options, "/dev/"+p.VG+"/"+r.Name, r.Mount); err != nil {
			return r, err
		}
		r.Source = path.Join(r.Mount, p.DataPath)
	default:
		return r, errors.New("unsupported filesystem snapshot method")
	}
	r.Created = true
	info, err := os.Stat(r.Source)
	if err != nil || !info.IsDir() {
		return r, errors.New("filesystem snapshot data directory is unavailable")
	}
	return r, nil
}

func validateSnapshotResource(ctx context.Context, state snapshotHostState, r snapshotHostResource, run snapshotCommand) error {
	switch r.Plan.Method {
	case "btrfs":
		mounted, err := snapshotMounted(ctx, r.Mount, run)
		if err != nil || !mounted {
			return errors.New("Btrfs snapshot mount is unavailable")
		}
		target := path.Join(r.Mount, ".cove-snapshots-"+state.Owner, r.Name)
		property, err := run(ctx, "btrfs", "property", "get", "-ts", target, "ro")
		if err != nil || strings.TrimSpace(property) != "ro=true" {
			return errors.New("Btrfs snapshot is no longer read-only")
		}
	case "zfs":
		for property, want := range map[string]string{"cove:owner": state.Owner, "cove:operation": state.Operation} {
			value, err := run(ctx, "zfs", "get", "-H", "-o", "value", property, r.Plan.Dataset+"@"+r.Name)
			if err != nil || strings.TrimSpace(value) != want {
				return errors.New("ZFS snapshot ownership changed")
			}
		}
	case "lvm":
		exists, err := snapshotLV(ctx, r, state.Owner, run)
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("LVM snapshot disappeared")
		}
	default:
		return errors.New("unknown filesystem snapshot method")
	}
	return nil
}

func cleanupSnapshotResource(ctx context.Context, state snapshotHostState, r snapshotHostResource, run snapshotCommand) error {
	switch r.Plan.Method {
	case "btrfs":
		mounted, err := snapshotMounted(ctx, r.Mount, run)
		if err != nil {
			return err
		}
		if !mounted {
			if err := os.MkdirAll(r.Mount, 0700); err != nil {
				return err
			}
			if _, err := run(ctx, "mount", "-t", "btrfs", "-o", "subvolid=5", "UUID="+r.Plan.UUID, r.Mount); err != nil {
				return err
			}
		}
		parent := path.Join(r.Mount, ".cove-snapshots-"+state.Owner)
		target := path.Join(parent, r.Name)
		if _, err := os.Lstat(target); err == nil {
			owner, err := os.ReadFile(path.Join(parent, "owner"))
			if err != nil || string(owner) != state.Owner {
				return errors.New("Btrfs cleanup ownership changed")
			}
			if err := validateSnapshotResource(ctx, state, r, run); err != nil {
				return err
			}
			if _, err := run(ctx, "btrfs", "subvolume", "delete", target); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		entries, err := os.ReadDir(parent)
		if err == nil && len(entries) == 1 && entries[0].Name() == "owner" {
			owner, err := os.ReadFile(path.Join(parent, "owner"))
			if err != nil || string(owner) != state.Owner {
				return errors.New("Btrfs cleanup workspace ownership changed")
			}
			if err := os.Remove(path.Join(parent, "owner")); err != nil {
				return err
			}
			if err := os.Remove(parent); err != nil {
				return err
			}
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		if _, err := run(ctx, "umount", "--", r.Mount); err != nil {
			return err
		}
	case "zfs":
		names, err := run(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-r", r.Plan.Dataset)
		if err != nil {
			return err
		}
		exists := false
		for _, name := range strings.Fields(names) {
			exists = exists || name == r.Plan.Dataset+"@"+r.Name
		}
		if exists {
			if err := validateSnapshotResource(ctx, state, r, run); err != nil {
				return err
			}
			if _, err := run(ctx, "zfs", "destroy", r.Plan.Dataset+"@"+r.Name); err != nil {
				return err
			}
		}
	case "lvm":
		exists, checkErr := snapshotLV(ctx, r, state.Owner, run)
		if !exists && checkErr != nil {
			return checkErr
		}
		mounted, err := snapshotMounted(ctx, r.Mount, run)
		if err != nil {
			return err
		}
		if mounted {
			if _, err := run(ctx, "umount", "--", r.Mount); err != nil {
				return err
			}
		}
		if exists {
			if _, err := run(ctx, "lvremove", "--force", "--", r.Plan.VG+"/"+r.Name); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported filesystem cleanup method %q", r.Plan.Method)
	}
	if err := os.Remove(r.Mount); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
