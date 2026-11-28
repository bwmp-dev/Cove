package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func commitFirewallRemoval(ctx context.Context, scope, owner string, run snapshotCommand) error {
	present, _, _, err := inspectFirewallTable(ctx, owner, run)
	if err != nil {
		return err
	}
	if present {
		return errors.New("firewall removal is not confirmed; the owned table is still present")
	}
	// Boot/startup must finish cleanup rather than restore a confirmed removal.
	if err := writeJSON(filepath.Join(scope, "firewall.json"), firewallJournal{Owner: owner, Removed: true, Rules: []FirewallRule{}}); err != nil {
		return err
	}
	return cleanupFirewallManagement(ctx, scope, owner, run)
}

func cleanupFirewallManagement(ctx context.Context, scope, owner string, run snapshotCommand) error {
	state, err := readFirewallJournal(scope, owner)
	if err != nil {
		return err
	}
	if !state.Removed {
		return errors.New("firewall management cleanup was not committed")
	}
	present, _, _, err := inspectFirewallTable(ctx, owner, run)
	if err != nil {
		return err
	}
	if present {
		return errors.New("firewall cleanup refused an unexpected table; inspect the node")
	}
	files := []string{"firewall.nft", "firewall-reverter", "firewall-reverter.next", "firewall.json"}
	for _, name := range files {
		info, err := os.Lstat(filepath.Join(scope, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		uid, _ := fileOwner(info)
		if !info.Mode().IsRegular() || info.Size() > 64<<20 || uid != 0 || info.Mode().Perm()&0077 != 0 {
			return errors.New("firewall cleanup refused an unsafe state file")
		}
	}
	if _, err := readFirewallDeadline(owner); err != nil {
		return err
	}
	boot := filepath.Join("/etc/systemd/system", firewallBootUnit(owner))
	if err := ownedHostPolicyFile(boot, "# Managed by Cove firewall "+owner+"\n"); err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, suffix := range []string{".timer", ".service"} {
		unit := firewallUnit(owner) + suffix
		load, err := run(ctx, "/usr/bin/systemctl", "show", unit, "--property=LoadState", "--value")
		if err != nil {
			return err
		}
		if strings.TrimSpace(load) == "not-found" {
			continue
		}
		description, err := run(ctx, "/usr/bin/systemctl", "show", unit, "--property=Description", "--value")
		if err != nil || strings.TrimSpace(description) != "Cove firewall recovery "+owner {
			return errors.New("firewall cleanup refused an unowned recovery unit")
		}
		if err := stopFirewallCleanupUnit(ctx, unit, suffix == ".service", run); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(boot); err == nil {
		if _, err := run(ctx, "/usr/bin/systemctl", "disable", firewallBootUnit(owner)); err != nil {
			return err
		}
		if err := stopFirewallCleanupUnit(ctx, firewallBootUnit(owner), true, run); err != nil {
			return err
		}
		if err := os.Remove(boot); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Dir(boot)); err != nil {
			return err
		}
		if _, err := run(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(firewallDeadlineFile(owner)); err == nil {
		if err := syncDirectory(filepath.Dir(firewallDeadlineFile(owner))); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// The journal is last, so an interrupted cleanup can be resumed safely.
	for _, name := range files {
		if err := os.Remove(filepath.Join(scope, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := syncDirectory(scope); err != nil {
			return err
		}
	}
	return nil
}

func stopFirewallCleanupUnit(ctx context.Context, unit string, service bool, run snapshotCommand) error {
	if service {
		value, err := run(ctx, "/usr/bin/systemctl", "show", unit, "--property=MainPID", "--value")
		if err != nil {
			return err
		}
		pid, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
		if err != nil {
			return errors.New("firewall cleanup could not identify the recovery process")
		}
		// A first-attempt timer or boot recovery cleans itself before exiting.
		if pid == uint64(os.Getpid()) {
			return nil
		}
	}
	_, err := run(ctx, "/usr/bin/systemctl", "stop", unit)
	return err
}
