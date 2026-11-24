package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type nodeLeaseContextKey struct{}

func nodeChangeLease(root string, create bool) (func(), error) {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("node maintenance requires an absolute worker root")
	}
	directory := filepath.Join(root, "node-changes")
	if create {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("node maintenance lock directory is unavailable or unsafe")
	}
	name := filepath.Join(directory, "worker.lock")
	if info, err := os.Lstat(name); err == nil {
		if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("node maintenance lock is unsafe")
		}
	} else if !os.IsNotExist(err) || !create {
		return nil, errors.New("node maintenance lock is unavailable")
	}
	unlock, err := lockNodeChanges(directory)
	if err != nil {
		return nil, errors.New("another proxy, move or host maintenance change is active; retry after it finishes")
	}
	return unlock, nil
}

func (e *Engine) pendingNodeMoves() error {
	entries, err := os.ReadDir(filepath.Join(e.Root, "moves"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || len(entries) > 4096 {
		return errors.New("node move inventory is unavailable")
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if e.moving(strings.TrimSuffix(entry.Name(), ".json")) {
			return errors.New("finish or recover the node move before changing proxy or host settings")
		}
	}
	return nil
}

func (e *Engine) lockNodeChange(allowMoves bool) (func(), error) {
	unlock, err := nodeChangeLease(e.Root, true)
	if err != nil {
		return nil, err
	}
	if !allowMoves {
		if err := e.pendingNodeMoves(); err != nil {
			unlock()
			return nil, err
		}
	}
	return unlock, nil
}

func RunScheduledHostSecurity(ctx context.Context, root, owner, manager string) error {
	return runScheduledHostSecurity(ctx, root, owner, manager, snapshotHostCommand)
}

func runScheduledHostSecurity(ctx context.Context, root, owner, manager string, run snapshotCommand) error {
	if err := validateHostMaintenance(HostMaintenanceRequest{Action: "inspect", Root: root, Owner: owner}); err != nil {
		return err
	}
	if manager != "apt" && manager != "dnf" {
		return errors.New("unsupported host security manager")
	}
	identity, err := readHostFile(filepath.Join(root, "host-maintenance", "owner"))
	if err != nil || string(identity) != owner {
		return errors.New("scheduled host update ownership is unavailable")
	}
	unlock, err := nodeChangeLease(root, false)
	if err != nil {
		return err
	}
	defer unlock()
	e := &Engine{Root: root}
	if _, err := os.Lstat(filepath.Join(root, "draining.json")); !os.IsNotExist(err) {
		return errors.New("worker is draining; scheduled host updates are deferred")
	}
	if err := e.pendingNodeMoves(); err != nil {
		return err
	}
	if err := e.ingressExposureReady(); err != nil {
		return err
	}
	if e.trafficRecoveryPending() {
		return errors.New("finish proxy traffic recovery before host updates")
	}
	custom, err := e.loadCustomIngress()
	if err != nil || unsettledCustom(custom.Change) {
		return errors.New("finish custom proxy recovery before host updates")
	}
	pending, err := e.certificateReloadPending()
	if err != nil || pending {
		return errors.New("finish certificate reload recovery before host updates")
	}
	if manager == "apt" {
		_, err = run(ctx, "/usr/bin/unattended-upgrade")
	} else {
		_, err = run(ctx, "/usr/bin/dnf", "--assumeyes", "--security", "upgrade")
	}
	return err
}

func (e *Engine) requireInterlockedHostSchedule() error {
	if _, err := os.Lstat(filepath.Join(e.Root, "host-maintenance")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	var saved struct {
		Version int
		Owner   string
	}
	owner, err := e.backupHelperOwner(false)
	if err != nil {
		return errors.New("host schedule ownership is unavailable; inspect and save its policy before node changes")
	}
	if err := readSnapshotMetadata(filepath.Join(e.Root, "node-changes", "host-policy.json"), &saved); err != nil || saved.Version != 1 || saved.Owner != owner {
		return errors.New("save the Host security updates policy once with this worker before changing listeners, traffic or node placement; its old timer does not share the node maintenance lock")
	}
	return nil
}

func markInterlockedHostSchedule(ctx context.Context, root, owner string, run snapshotCommand) error {
	state, err := run(ctx, "/usr/bin/systemctl", "show", "--property=ActiveState", "--value", hostPolicyUnit(owner)+".service")
	if err != nil {
		return errors.New("host security service status could not be confirmed; inspect and save the policy again before node changes")
	}
	switch strings.TrimSpace(state) {
	case "inactive", "failed":
	case "active", "activating", "deactivating", "reloading":
		return errors.New("a host security update is already running; wait for it to finish, then save the policy again before node changes")
	default:
		return errors.New("host security service status is unknown; inspect and save the policy again before node changes")
	}
	directory := filepath.Join(root, "node-changes")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	filename := filepath.Join(directory, "host-policy.json")
	if err := writeJSON(filename, struct {
		Version int
		Owner   string
	}{1, owner}); err != nil {
		return err
	}
	if err := os.Chmod(filename, 0644); err != nil {
		return err
	}
	return syncDirectory(directory)
}
