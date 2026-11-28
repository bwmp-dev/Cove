package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostFirewallFirstAttemptCleanupOnOwnedLinuxVM(t *testing.T) {
	binary := os.Getenv("COVE_TEST_FIREWALL_BINARY")
	if os.Getenv("COVE_TEST_OWNED_HOST_INSPECTION") != "1" || os.Geteuid() != 0 || binary == "" {
		t.Skip("explicit disposable root Linux VM, systemd and worker binary required")
	}
	for _, mode := range []string{"boot-unit", "explicit-revert"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			t.Cleanup(cancel)
			root := t.TempDir()
			owner, err := firewallChangeID()
			if err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(filepath.Join(root, "backup-helper-owner.json"), struct{ ID string }{owner}); err != nil {
				t.Fatal(err)
			}
			scope := filepath.Join(root, "host-maintenance")
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer stop()
				if t.Failed() {
					state, _ := readFirewallJournal(scope, owner)
					t.Logf("failed first-attempt cleanup mode=%s state=%+v", mode, state)
					output, _ := snapshotHostCommand(cleanup, "/usr/bin/journalctl", "--no-pager", "-n", "80", "-u", firewallBootUnit(owner), "-u", firewallUnit(owner)+".service")
					t.Log(output)
				}
				if _, err := runHostFirewall(cleanup, root, owner, FirewallControl{Action: "recover"}, snapshotHostCommand); err != nil {
					t.Error("owned checkpoint cleanup", err)
				}
				if err := revertFirewall(cleanup, root, owner, "", snapshotHostCommand); err != nil {
					t.Error("owned first-attempt cleanup", err)
				}
			})
			call := func(control FirewallControl) FirewallResult {
				t.Helper()
				control.Reserved = []uint16{22, 80, 443}
				input, err := json.Marshal(firewallHostRequest{Root: root, Owner: owner, Control: control})
				if err != nil {
					t.Fatal(err)
				}
				command := exec.CommandContext(ctx, binary, "host-firewall")
				command.Stdin = bytes.NewReader(input)
				output, err := command.Output()
				if err != nil {
					t.Fatal(err)
				}
				var response struct {
					State FirewallResult
					Error string
				}
				if json.Unmarshal(output, &response) != nil || response.Error != "" || !response.State.Supported {
					t.Fatal("first-attempt helper refused", response.Error, response.State.Notice)
				}
				return response.State
			}
			before, err := snapshotHostCommand(ctx, "/usr/sbin/nft", "--stateless", "list", "ruleset")
			if err != nil {
				t.Fatal(err)
			}
			preview := call(FirewallControl{Action: "preview", Rules: []FirewallRule{}})
			other := filepath.Join(scope, "other-host-feature.fixture")
			if err := os.WriteFile(other, []byte("keep unrelated host state"), 0600); err != nil {
				t.Fatal(err)
			}
			applied := call(FirewallControl{Action: "apply", Rules: []FirewallRule{}, Revision: preview.Revision})
			if applied.Pending == nil || !applied.Active || !applied.Managed {
				t.Fatal("first policy was not armed", applied)
			}
			if mode == "boot-unit" {
				if _, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "start", firewallBootUnit(owner)); err != nil {
					t.Fatal("real boot restoration unit did not complete its own cleanup", err)
				}
			} else {
				call(FirewallControl{Action: "revert", Intent: applied.Pending.ID})
			}
			state := call(FirewallControl{Action: "inspect"})
			if state.Active || state.Managed || state.Pending != nil {
				t.Fatal("first policy survived its revert", state)
			}
			for _, name := range []string{"firewall.json", "firewall.nft", "firewall-reverter", "firewall-arming.json"} {
				if _, err := os.Lstat(filepath.Join(scope, name)); !os.IsNotExist(err) {
					t.Fatal("first attempt left worker artifacts", name, err)
				}
			}
			for _, name := range []string{firewallDeadlineFile(owner), filepath.Join("/etc/systemd/system", firewallBootUnit(owner))} {
				if _, err := os.Lstat(name); !os.IsNotExist(err) {
					t.Fatal("first attempt left system artifacts", name, err)
				}
			}
			until := time.Now().Add(10 * time.Second)
			for _, unit := range []string{firewallUnit(owner) + ".timer", firewallUnit(owner) + ".service", firewallBootUnit(owner)} {
				for {
					loaded, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "show", unit, "--property=LoadState", "--value")
					if err != nil {
						t.Fatal(err)
					}
					if strings.TrimSpace(loaded) == "not-found" {
						break
					}
					if time.Now().After(until) || ctx.Err() != nil {
						t.Fatal("first-attempt recovery unit did not disappear", unit, loaded)
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
			if data, err := os.ReadFile(other); err != nil || string(data) != "keep unrelated host state" {
				t.Fatal("another host feature's state was touched", err)
			}
			after, err := snapshotHostCommand(ctx, "/usr/sbin/nft", "--stateless", "list", "ruleset")
			if err != nil || before != after {
				t.Fatal("first attempt changed another firewall table", err)
			}
			t.Logf("first policy cleanup through %s completed without another worker start; owned files/units/stamp absent", mode)
		})
	}
}
