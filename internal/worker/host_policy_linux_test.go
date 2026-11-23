package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostMaintenanceUsesHelperOwnerAndRejectsForeignRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("native root helper validation required")
	}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	build, err := e.buildOwner()
	if err != nil {
		t.Fatal(err)
	}
	helper, err := e.backupHelperOwner(true)
	if err != nil || helper == build {
		t.Fatal("distinct fixture owners unavailable", err)
	}
	request := HostMaintenanceRequest{Action: "inspect", Root: e.Root, Owner: helper}
	if err = validateHostMaintenance(request); err != nil {
		t.Fatal("correct native helper owner refused", err)
	}
	request.Owner = build
	if validateHostMaintenance(request) == nil {
		t.Fatal("unrelated build owner authenticated a host helper")
	}
	request.Owner = helper
	if err = os.Chmod(e.Root, 0777); err != nil {
		t.Fatal(err)
	}
	if validateHostMaintenance(request) == nil {
		t.Fatal("writable host root accepted")
	}
}

func TestHostSecurityScheduleOwnershipAndRecovery(t *testing.T) {
	root := t.TempDir()
	owner := strings.Repeat("b3", 16)
	scope, err := hostPolicyScope(root, owner)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, "security.service")
	marker := hostPolicyMarker(owner)
	if err = os.WriteFile(filename, []byte("operator-owned service"), 0644); err != nil {
		t.Fatal(err)
	}
	if err = writeHostPolicyFile(filename, scope, marker, marker+"[Unit]\n"); err == nil {
		t.Fatal("overwrote operator service")
	}
	if err = os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	if err = writeHostPolicyFile(filename, scope, marker, marker+"[Unit]\n"); err != nil {
		t.Fatal(err)
	}
	if err = writeHostPolicyFile(filename, scope, marker, marker+"[Unit]\nDescription=Changed\n"); err != nil {
		t.Fatal("owned schedule could not be updated", err)
	}
	if _, err = hostPolicyScope(root, strings.Repeat("c4", 16)); err == nil {
		t.Fatal("accepted a foreign worker scope")
	}
	alias := filepath.Join(root, "alias.service")
	if err = os.Symlink(filename, alias); err != nil {
		t.Fatal(err)
	}
	if err = ownedHostPolicyFile(alias, marker); err == nil {
		t.Fatal("accepted aliased schedule")
	}
	if err = writeHostPolicyFile(alias, scope, marker, marker+"changed"); err == nil {
		t.Fatal("replaced symlink schedule")
	}
}

func TestHostPatchScheduleOnOwnedLinuxVM(t *testing.T) {
	if os.Getenv("COVE_TEST_OWNED_HOST_INSPECTION") != "1" || os.Geteuid() != 0 {
		t.Skip("explicit disposable root Linux VM required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	owner := strings.Repeat("ab", 16)
	unit := hostPolicyUnit(owner)
	for _, path := range []string{"/etc/systemd/system/", "/run/systemd/system/"} {
		for _, suffix := range []string{".service", ".timer"} {
			if _, err := os.Lstat(path + unit + suffix); !os.IsNotExist(err) {
				t.Fatal("refusing an existing host fixture unit", path, unit, err)
			}
		}
	}
	// Masking the test service proves timer installation without applying real packages.
	if _, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "mask", "--runtime", unit+".service"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, args := range [][]string{{"disable", "--now", unit + ".timer"}, {"unmask", "--runtime", unit + ".service"}} {
			if _, err := snapshotHostCommand(cleanup, append([]string{"/usr/bin/systemctl"}, args...)...); err != nil {
				t.Error(err)
			}
		}
		for _, suffix := range []string{".service", ".timer"} {
			filename := "/etc/systemd/system/" + unit + suffix
			if err := ownedHostPolicyFile(filename, hostPolicyMarker(owner)); err == nil {
				if err = os.Remove(filename); err != nil {
					t.Error(err)
				}
			} else if !os.IsNotExist(err) {
				t.Error(err)
			}
		}
		if _, err := snapshotHostCommand(cleanup, "/usr/bin/systemctl", "daemon-reload"); err != nil {
			t.Error(err)
		}
	})
	if err := configureHostPatchPolicy(ctx, "/", root, owner, true, snapshotHostCommand); err != nil {
		t.Fatal(err)
	}
	status, err := inspectHostPatches(ctx, "/", owner, snapshotHostCommand)
	if err != nil || !status.PolicyEnabled || status.LastResult != "" {
		t.Fatal("real security timer was not enabled or claimed an update", status, err)
	}
	if err = configureHostPatchPolicy(ctx, "/", root, owner, false, snapshotHostCommand); err != nil {
		t.Fatal(err)
	}
	status, err = inspectHostPatches(ctx, "/", owner, snapshotHostCommand)
	if err != nil || status.PolicyEnabled {
		t.Fatal("real security timer was not disabled", status, err)
	}
	t.Log("Native systemd timer enabled and disabled; masked service applied no package changes.")
}
