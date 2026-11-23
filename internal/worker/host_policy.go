package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func hostPolicyUnit(owner string) string {
	digest := sha256.Sum256([]byte("cove host security schedule v1:" + owner))
	return "cove-security-" + hex.EncodeToString(digest[:16])
}

func hostPolicyMarker(owner string) string { return "# Managed by Cove host security " + owner + "\n" }

func ownedHostPolicyFile(filename, marker string) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return errors.New("host schedule path is unsafe")
	}
	data, err := readHostFile(filename)
	if err != nil || !bytes.HasPrefix(data, []byte(marker)) {
		return errors.New("host schedule is not owned by this worker")
	}
	return nil
}

func hostPolicyFiles(manager, owner string) (string, string, error) {
	command := ""
	switch manager {
	case "apt":
		command = "/usr/bin/unattended-upgrade"
	case "dnf":
		command = "/usr/bin/dnf --assumeyes --security upgrade"
	default:
		return "", "", errors.New("unsupported host package manager")
	}
	marker := hostPolicyMarker(owner)
	service := marker + "[Unit]\nDescription=Cove scheduled host security updates\nWants=network-online.target\nAfter=network-online.target\n\n[Service]\nType=oneshot\nExecStart=" + command + "\nNice=10\nIOSchedulingClass=best-effort\nIOSchedulingPriority=7\nCPUQuota=50%\nStandardOutput=null\nStandardError=journal\nSyslogIdentifier=" + hostPolicyUnit(owner) + "\nLogRateLimitIntervalSec=30s\nLogRateLimitBurst=20\n"
	timer := marker + "[Unit]\nDescription=Cove daily host security update schedule\n\n[Timer]\nOnCalendar=*-*-* 04:00:00 UTC\nRandomizedDelaySec=1h\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n"
	return service, timer, nil
}

func hostPolicyScope(root, owner string) (string, error) {
	if !snapshotOwnerID.MatchString(owner) {
		return "", errors.New("invalid host maintenance owner")
	}
	directory := filepath.Join(root, "host-maintenance")
	if err := os.Mkdir(directory, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("host maintenance directory is unsafe")
	}
	unlock, err := lockWorker(directory)
	if err != nil {
		return "", err
	}
	defer unlock()
	filename := filepath.Join(directory, "owner")
	if info, err := os.Lstat(filename); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != 32) {
		return "", errors.New("host maintenance ownership is unsafe")
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	data, err := readHostFile(filename)
	if err == nil {
		if string(data) != owner {
			return "", errors.New("host maintenance directory belongs to another owner")
		}
	} else {
		if _, statErr := os.Lstat(filename); !os.IsNotExist(statErr) {
			return "", errors.New("host maintenance ownership is unavailable")
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			return "", err
		}
		if len(entries) != 1 || entries[0].Name() != "worker.lock" {
			return "", errors.New("unowned host maintenance files require inspection")
		}
		file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return "", err
		}
		_, writeErr := file.WriteString(owner)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return "", errors.Join(writeErr, closeErr)
		}
		if err := syncDirectory(directory); err != nil {
			return "", err
		}
	}
	return directory, nil
}

func writeHostPolicyFile(filename, stageDirectory, marker, data string) error {
	if len(data) > 16<<10 || !strings.HasPrefix(data, marker) {
		return errors.New("host schedule unit exceeds its bounds")
	}
	if info, err := os.Lstat(filename); err == nil {
		if !info.Mode().IsRegular() || info.Size() > 16<<10 {
			return errors.New("host schedule path is unsafe")
		}
		existing, err := readHostFile(filename)
		if err != nil || !bytes.HasPrefix(existing, []byte(marker)) {
			return errors.New("existing host schedule is not owned by this worker")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	stage := filename + ".cove-next"
	intent := filepath.Join(stageDirectory, filepath.Base(filename)+".intent.json")
	var claim struct {
		Path   string `json:"path"`
		Marker string `json:"marker"`
	}
	claimErr := readSnapshotMetadata(intent, &claim)
	if claimErr == nil && (claim.Path != stage || claim.Marker != marker) {
		return errors.New("host schedule staging claim differs from this worker's intent")
	}
	if claimErr != nil && !os.IsNotExist(claimErr) {
		return claimErr
	}
	if info, err := os.Lstat(stage); err == nil {
		if claimErr != nil || !info.Mode().IsRegular() || info.Size() > 16<<10 || info.Mode().Perm() != 0600 && info.Mode().Perm() != 0644 {
			return errors.New("host schedule staging file is unsafe")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if claimErr != nil {
		claim.Path, claim.Marker = stage, marker
		if err := writeJSON(intent, claim); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(stage, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = file.Chmod(0600)
	if err == nil {
		_, err = file.WriteString(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return err
	}
	if err = os.Chmod(stage, 0644); err != nil {
		return err
	}
	if err = os.Rename(stage, filename); err != nil {
		return err
	}
	if err = syncDirectory(filepath.Dir(filename)); err != nil {
		return err
	}
	if err = os.Remove(intent); err != nil {
		return err
	}
	return syncDirectory(stageDirectory)
}

func configureHostPatchPolicy(ctx context.Context, hostRoot, workerRoot, owner string, enabled bool, run snapshotCommand) error {
	scope, err := hostPolicyScope(workerRoot, owner)
	if err != nil {
		return err
	}
	unlock, err := lockWorker(scope)
	if err != nil {
		return err
	}
	defer unlock()
	unit := hostPolicyUnit(owner)
	marker := hostPolicyMarker(owner)
	unitDirectory := filepath.Join(hostRoot, "etc", "systemd", "system")
	if enabled {
		status, err := inspectHostPatches(ctx, hostRoot, owner, run)
		if err != nil {
			return err
		}
		if !status.PolicySupported {
			return errors.New("install the supported native update tool and run systemd before enabling this schedule")
		}
		service, timer, err := hostPolicyFiles(status.Manager, owner)
		if err != nil {
			return err
		}
		for _, suffix := range []string{".service", ".timer"} {
			if err = ownedHostPolicyFile(filepath.Join(unitDirectory, unit+suffix), marker); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		for _, item := range []struct{ suffix, content string }{{".service", service}, {".timer", timer}} {
			if err = writeHostPolicyFile(filepath.Join(unitDirectory, unit+item.suffix), scope, marker, item.content); err != nil {
				return err
			}
		}
		if _, err = run(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
			return errors.New("host schedule reload failed; retry after inspecting systemd")
		}
		if _, err = run(ctx, "/usr/bin/systemctl", "enable", "--now", unit+".timer"); err != nil {
			return errors.New("host schedule activation could not be confirmed; refresh its status before retrying")
		}
		return nil
	}
	for _, suffix := range []string{".service", ".timer"} {
		filename := filepath.Join(unitDirectory, unit+suffix)
		if _, err := os.Lstat(filename); os.IsNotExist(err) {
			continue
		}
		if err := ownedHostPolicyFile(filename, marker); err != nil {
			return err
		}
	}
	if _, err = run(ctx, "/usr/bin/systemctl", "disable", "--now", unit+".timer"); err != nil {
		return fmt.Errorf("host schedule disable could not be confirmed: %w", err)
	}
	return nil
}
