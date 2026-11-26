package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHostFirewallRemovalOnOwnedLinuxVM(t *testing.T) {
	testHostFirewallTimedRevertOnOwnedLinuxVM(t, nil, false, true)
}

func waitFirewallRecoveryComplete(t *testing.T, ctx context.Context, owner string) {
	t.Helper()
	until := time.Now().Add(30 * time.Second)
	for {
		state, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "show", firewallUnit(owner)+".service", "--property=ActiveState,MainPID")
		if err != nil {
			t.Fatal("firewall recovery service status unavailable", err)
		}
		if strings.Contains(state, "ActiveState=inactive\n") && strings.Contains(state, "MainPID=0\n") {
			return
		}
		if time.Now().After(until) || ctx.Err() != nil {
			t.Fatal("firewall recovery did not finish its state cleanup", state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func testFirewallRemoval(t *testing.T, ctx context.Context, root, owner, binary string, application, management uint16, call func(FirewallControl) FirewallResult, request func(context.Context, uint16) error) {
	t.Helper()
	scope := filepath.Join(root, "host-maintenance")
	assertClean := func() {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for _, unit := range []string{firewallUnit(owner) + ".timer", firewallUnit(owner) + ".service", firewallBootUnit(owner)} {
			for {
				value, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "show", unit, "--property=LoadState", "--value")
				if err != nil {
					t.Fatal(err)
				}
				if strings.TrimSpace(value) == "not-found" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("firewall unit remains loaded", unit, value)
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		for _, name := range []string{"firewall.json", "firewall.nft", "firewall-reverter", "firewall-reverter.next", "firewall-arming.json"} {
			if _, err := os.Lstat(filepath.Join(scope, name)); !os.IsNotExist(err) {
				t.Fatal("firewall state residue", name, err)
			}
		}
		for _, name := range []string{firewallDeadlineFile(owner), filepath.Join("/etc/systemd/system", firewallBootUnit(owner))} {
			if _, err := os.Lstat(name); !os.IsNotExist(err) {
				t.Fatal("firewall unit/stamp residue", name, err)
			}
		}
		state := call(FirewallControl{Action: "inspect"})
		if state.Active || state.Managed || state.Pending != nil || len(state.Rules) != 0 {
			t.Fatal("firewall still managed", state)
		}
	}
	apply := func(rules []FirewallRule, remove bool) FirewallResult {
		t.Helper()
		var before []FirewallRule
		if remove {
			before = call(FirewallControl{Action: "inspect"}).Rules
		}
		preview := call(FirewallControl{Action: "preview", Rules: rules, Remove: remove})
		if remove && !reflect.DeepEqual(preview.Rules, before) {
			t.Fatal("removal preview omitted the current policy being dropped", preview.Rules, before)
		}
		result := call(FirewallControl{Action: "apply", Rules: rules, Remove: remove, Revision: preview.Revision})
		if result.Pending == nil || result.Pending.Remove != remove || !result.Managed {
			t.Fatal("missing removal recovery intent", result)
		}
		return result
	}
	confirm := func(result FirewallResult) FirewallResult {
		t.Helper()
		return call(FirewallControl{Action: "confirm", Intent: result.Pending.ID, Remove: result.Pending.Remove})
	}
	baseline, err := snapshotHostCommand(ctx, "/usr/sbin/nft", "--stateless", "list", "ruleset")
	if err != nil {
		t.Fatal(err)
	}

	// This is the residue left by earlier builds: confirmed empty table and full recovery assets.
	empty := confirm(apply([]FirewallRule{}, false))
	if !empty.Managed || !empty.Active || len(empty.Rules) != 0 {
		t.Fatal("empty confirmed residue was not constructed", empty)
	}
	for _, name := range []string{filepath.Join(scope, "firewall.json"), filepath.Join(scope, "firewall.nft"), filepath.Join(scope, "firewall-reverter"), firewallDeadlineFile(owner), filepath.Join("/etc/systemd/system", firewallBootUnit(owner))} {
		if _, err := os.Stat(name); err != nil {
			t.Fatal("missing earlier-build residue", name, err)
		}
	}
	removed := confirm(apply(nil, true))
	if removed.Active || removed.Managed || removed.Pending != nil {
		t.Fatal("confirmed residue removal still managed", removed)
	}
	assertClean()
	after, err := snapshotHostCommand(ctx, "/usr/sbin/nft", "--stateless", "list", "ruleset")
	if err != nil || baseline != after {
		t.Fatal("removal changed another host firewall table", err)
	}

	rules := []FirewallRule{{Protocol: "tcp", Port: application, Sources: []string{}}}
	confirmed := confirm(apply(rules, false))
	if !confirmed.Active || request(ctx, application) == nil || request(ctx, management) != nil {
		t.Fatal("protected policy precondition failed")
	}
	unconfirmed := apply(nil, true)
	t.Logf("unconfirmed removal deadline=%s", unconfirmed.Pending.Deadline.UTC().Format(time.RFC3339Nano))
	if unconfirmed.Active || request(ctx, application) != nil || request(ctx, management) != nil {
		t.Fatal("removal did not lift only the owned restrictions")
	}
	originalTimer, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "show", firewallUnit(owner)+".timer", "--property=NextElapseUSecRealtime", "--value")
	if err != nil || strings.TrimSpace(originalTimer) == "" || strings.TrimSpace(originalTimer) == "0" {
		t.Fatal("removal deadline was not armed", err, originalTimer)
	}
	if _, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "daemon-reexec"); err != nil {
		t.Fatal(err)
	}
	afterManager, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "show", firewallUnit(owner)+".timer", "--property=NextElapseUSecRealtime", "--value")
	if err != nil || originalTimer != afterManager {
		t.Fatal("manager change moved the original removal deadline", err, originalTimer, afterManager)
	}
	t.Logf("removal timer before/after reload+reexec: %q -> %q", originalTimer, afterManager)
	deadline := unconfirmed.Pending.Deadline.Add(30 * time.Second)
	for time.Now().Before(deadline) && request(ctx, application) == nil {
		if request(ctx, management) != nil {
			t.Fatal("management unavailable while awaiting removal recovery")
		}
		time.Sleep(500 * time.Millisecond)
	}
	waitFirewallRecoveryComplete(t, ctx, owner)
	restored := call(FirewallControl{Action: "inspect"})
	if restored.Pending != nil || !restored.Active || !restored.Managed || len(restored.Rules) != 1 || request(ctx, application) == nil || request(ctx, management) != nil {
		t.Fatal("unconfirmed removal did not restore the original policy by its absolute deadline", restored)
	}
	confirm(apply(nil, true))
	assertClean()

	// A first unconfirmed apply must restore the pre-Cove host, including its files and units.
	first := apply(rules, false)
	t.Logf("first policy deadline=%s", first.Pending.Deadline.UTC().Format(time.RFC3339Nano))
	deadline = first.Pending.Deadline.Add(30 * time.Second)
	for time.Now().Before(deadline) && request(ctx, application) != nil {
		if request(ctx, management) != nil {
			t.Fatal("management unavailable while awaiting first-attempt recovery")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if request(ctx, application) != nil {
		t.Fatal("first unconfirmed policy did not revert on time")
	}
	t.Logf("first policy traffic restored=%s before cleanup completion check", time.Now().UTC().Format(time.RFC3339Nano))
	assertClean()
	t.Logf("first policy cleanup complete=%s without boot or worker restart", time.Now().UTC().Format(time.RFC3339Nano))

	// A collected old attempt must not restore a later confirmed policy after removal/re-enable.
	current := confirm(apply([]FirewallRule{}, false))
	command := exec.CommandContext(ctx, binary, "firewall-revert-attempt", root, owner, first.Pending.ID)
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "stale attempt ignored") {
		t.Fatal("old removed attempt was not rejected", err, string(output))
	}
	state := call(FirewallControl{Action: "inspect"})
	if !current.Active || !state.Active || state.Pending != nil || len(state.Rules) != 0 || request(ctx, management) != nil {
		t.Fatal("old removed attempt changed a later confirmed policy", state)
	}
	prior, err := readFirewallJournal(scope, owner)
	if err != nil {
		t.Fatal(err)
	}
	// A lost table does not make an existing confirmed boot policy a first attempt.
	if _, err := snapshotHostCommand(ctx, "/usr/sbin/nft", "delete", "table", "inet", firewallTable(owner)); err != nil {
		t.Fatal(err)
	}
	missingTable := apply(nil, true)
	reverted := call(FirewallControl{Action: "revert", Intent: missingTable.Pending.ID, Remove: true})
	kept, err := readFirewallJournal(scope, owner)
	if err != nil || reverted.Active || !reverted.Managed || reverted.Pending != nil || kept.Generation != prior.Generation || kept.Removed {
		t.Fatal("unconfirmed removal discarded a confirmed policy whose table was absent", kept, reverted, err)
	}
	for _, name := range []string{filepath.Join(scope, "firewall-reverter"), filepath.Join("/etc/systemd/system", firewallBootUnit(owner))} {
		if _, err := os.Stat(name); err != nil {
			t.Fatal("prior boot recovery was not preserved", name, err)
		}
	}
	confirm(apply(nil, true))
	assertClean()
}
