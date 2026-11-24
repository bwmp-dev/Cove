package worker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type firewallArming struct {
	Owner       string
	Attempt     string
	Previous    []byte
	BootExisted bool
	Stamp       *firewallDeadlineStamp
}

type firewallDeadlineStamp struct {
	Data     []byte
	Modified time.Time
	Mode     os.FileMode
}

func firewallDeadlineFile(owner string) string {
	return filepath.Join("/var/lib/systemd/timers", "stamp-"+firewallUnit(owner)+".timer")
}

func readFirewallDeadline(owner string) (*firewallDeadlineStamp, error) {
	filename := firewallDeadlineFile(owner)
	info, err := os.Lstat(filename)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	uid, _ := fileOwner(info)
	if !info.Mode().IsRegular() || info.Size() > 4096 || info.Mode().Perm()&0022 != 0 || uid != 0 {
		return nil, errors.New("firewall recovery deadline stamp is unsafe")
	}
	data, err := readHostFile(filename)
	if err != nil || len(data) > 4096 {
		return nil, errors.Join(err, errors.New("firewall deadline stamp could not be read"))
	}
	return &firewallDeadlineStamp{data, info.ModTime(), info.Mode().Perm()}, nil
}

func cleanupFirewallArming(ctx context.Context, scope, owner string, run snapshotCommand) error {
	filename := filepath.Join(scope, "firewall-arming.json")
	var saved firewallArming
	if err := readSnapshotMetadata(filename, &saved); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if saved.Owner != owner || !snapshotOwnerID.MatchString(saved.Attempt) {
		return errors.New("firewall arming recovery ownership is invalid")
	}
	state, err := readFirewallJournal(scope, owner)
	if err != nil || state.Pending != nil && state.Pending.ID != saved.Attempt {
		return errors.Join(err, errors.New("firewall arming recovery conflicts with another attempt"))
	}
	if state.Pending == nil {
		current, err := os.ReadFile(filepath.Join(scope, "firewall.json"))
		if !(os.IsNotExist(err) && saved.Previous == nil) && (err != nil || !bytes.Equal(current, saved.Previous)) {
			return errors.New("firewall journal changed after the refused arming attempt")
		}
	}
	for _, suffix := range []string{".timer", ".service"} {
		unit := firewallUnit(owner) + suffix
		load, err := run(ctx, "/usr/bin/systemctl", "show", unit, "--property=LoadState", "--value")
		if err != nil {
			return err
		}
		if strings.TrimSpace(load) != "not-found" {
			description, err := run(ctx, "/usr/bin/systemctl", "show", unit, "--property=Description", "--value")
			if err != nil || strings.TrimSpace(description) != "Cove firewall recovery "+owner {
				return errors.New("refusing an unowned firewall arming recovery unit")
			}
			if _, err := run(ctx, "/usr/bin/systemctl", "stop", unit); err != nil {
				return err
			}
		}
		until := time.Now().Add(5 * time.Second)
		for {
			load, err = run(ctx, "/usr/bin/systemctl", "show", unit, "--property=LoadState", "--value")
			if err != nil {
				return err
			}
			if strings.TrimSpace(load) == "not-found" {
				break
			}
			if time.Now().After(until) || ctx.Err() != nil {
				return errors.New("firewall arming recovery unit removal could not be confirmed")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	stamp := firewallDeadlineFile(owner)
	current, err := readFirewallDeadline(owner)
	if err != nil {
		return err
	}
	if saved.Stamp == nil {
		if current != nil {
			if err := os.Remove(stamp); err != nil {
				return err
			}
			if err := syncDirectory(filepath.Dir(stamp)); err != nil {
				return err
			}
		}
	} else {
		if len(saved.Stamp.Data) > 4096 || saved.Stamp.Modified.IsZero() || saved.Stamp.Mode&^0777 != 0 || saved.Stamp.Mode&0022 != 0 {
			return errors.New("saved firewall deadline stamp is invalid")
		}
		if current == nil || !bytes.Equal(current.Data, saved.Stamp.Data) || !current.Modified.Equal(saved.Stamp.Modified) || current.Mode != saved.Stamp.Mode {
			if err := writeFileAtomic(stamp, saved.Stamp.Data, saved.Stamp.Mode); err != nil {
				return err
			}
			if err := os.Chtimes(stamp, saved.Stamp.Modified, saved.Stamp.Modified); err != nil {
				return err
			}
			file, err := os.Open(stamp)
			if err != nil {
				return err
			}
			if err := errors.Join(file.Sync(), file.Close(), syncDirectory(filepath.Dir(stamp))); err != nil {
				return err
			}
		}
	}
	if !saved.BootExisted {
		unit := firewallBootUnit(owner)
		filename := filepath.Join("/etc/systemd/system", unit)
		if _, err := os.Lstat(filename); err == nil {
			if err := ownedHostPolicyFile(filename, "# Managed by Cove firewall "+owner+"\n"); err != nil {
				return err
			}
			if _, err := run(ctx, "/usr/bin/systemctl", "disable", "--now", unit); err != nil {
				return err
			}
			if err := os.Remove(filename); err != nil {
				return err
			}
			if err := syncDirectory(filepath.Dir(filename)); err != nil {
				return err
			}
			if _, err := run(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	journal := filepath.Join(scope, "firewall.json")
	if saved.Previous == nil {
		if err := os.Remove(journal); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := writeFileAtomic(journal, saved.Previous, 0600); err != nil {
		return err
	}
	if err := os.Remove(filename); err != nil {
		return err
	}
	return syncDirectory(scope)
}

func armFirewallRecovery(ctx context.Context, root, owner, scope, binary string, state firewallJournal, run snapshotCommand) error {
	journal := filepath.Join(scope, "firewall.json")
	var previous []byte
	if _, err := os.Lstat(journal); err == nil {
		previous, err = readHostFile(journal)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	_, bootErr := os.Lstat(filepath.Join("/etc/systemd/system", firewallBootUnit(owner)))
	if bootErr != nil && !os.IsNotExist(bootErr) {
		return bootErr
	}
	stamp, err := readFirewallDeadline(owner)
	if err != nil {
		return err
	}
	saved := firewallArming{Owner: owner, Attempt: state.Pending.ID, Previous: previous, BootExisted: bootErr == nil, Stamp: stamp}
	// Keep a lost arming acknowledgement recoverable before any firewall change.
	marker := filepath.Join(scope, "firewall-arming.json")
	if err := writeJSON(marker, saved); err != nil {
		return err
	}
	arm := func() error {
		if err := installFirewallBootPolicy(ctx, root, owner, scope, binary, run); err != nil {
			return err
		}
		if err := stopFirewallTimer(ctx, owner, run); err != nil {
			return err
		}
		state.Pending.Deadline = time.Now().UTC().Add(2 * time.Minute)
		if err := writeJSON(filepath.Join(scope, "firewall.json"), state); err != nil {
			return err
		}
		// OnActiveSec restarts on manager reload; the saved deadline must not move.
		_, err := run(ctx, "/usr/bin/systemd-run", "--unit="+firewallUnit(owner), "--on-calendar="+state.Pending.Deadline.Format("2006-01-02 15:04:05.000000 UTC"), "--timer-property=Persistent=true", "--timer-property=AccuracySec=1s", "--timer-property=Description=Cove firewall recovery "+owner, "--property=Description=Cove firewall recovery "+owner, "--property=Restart=on-failure", "--property=RestartSec=5s", "--property=TimeoutStartSec=30s", "--collect", binary, "firewall-revert-attempt", root, owner, state.Pending.ID)
		if err != nil {
			return err
		}
		// Startup cleanup must only discard attempts that have not touched nftables.
		if err := os.Remove(marker); err != nil {
			return err
		}
		if err := syncDirectory(scope); err != nil {
			return errors.Join(err, writeJSON(marker, saved))
		}
		return nil
	}
	if err := arm(); err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if cleanupErr := cleanupFirewallArming(cleanup, scope, owner, run); cleanupErr != nil {
			return errors.New("automatic-revert timer could not be confirmed; no firewall policy was applied. Recovery cleanup is pending; restart the worker or inspect the node before retrying")
		}
		return errors.New("automatic-revert timer could not be confirmed; no firewall policy was applied. Retry after the host service manager is available")
	}
	return nil
}
