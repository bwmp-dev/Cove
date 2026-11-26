package worker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testFirewallArmingRefusals(t *testing.T, ctx context.Context, root, owner, binary string, application, management uint16, namespace string, call func(FirewallControl) FirewallResult, probe func() func()) {
	t.Helper()
	scope := filepath.Join(root, "host-maintenance")
	journal := filepath.Join(scope, "firewall.json")
	ownedPolicy := func() string {
		t.Helper()
		present, _, _, err := inspectFirewallTable(ctx, owner, snapshotHostCommand)
		if err != nil {
			t.Fatal(err)
		}
		if !present {
			return ""
		}
		raw, err := snapshotHostCommand(ctx, "/usr/sbin/nft", "--stateless", "list", "table", "inet", firewallTable(owner))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, mode := range []string{"initial-lost-ack", "reexec", "lost-ack", "cleanup-restart"} {
		t.Run(mode, func(t *testing.T) {
			before, err := os.ReadFile(journal)
			if err != nil && !(mode == "initial-lost-ack" && os.IsNotExist(err)) {
				t.Fatal(err)
			}
			policy, err := snapshotHostCommand(ctx, "/usr/sbin/nft", "--stateless", "list", "ruleset")
			if err != nil {
				t.Fatal(err)
			}
			ownBefore := ownedPolicy()
			stampBefore, err := readFirewallDeadline(owner)
			if err != nil {
				t.Fatal(err)
			}
			bootFile := filepath.Join("/etc/systemd/system", firewallBootUnit(owner))
			bootBefore, err := os.ReadFile(bootFile)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			rules := []FirewallRule{{Protocol: "tcp", Port: application, Sources: []string{}}}
			preview := call(FirewallControl{Action: "preview", Rules: rules})
			previewPolicy, err := snapshotHostCommand(ctx, "/usr/sbin/nft", "--stateless", "list", "ruleset")
			if err != nil {
				t.Fatal(err)
			}
			control := FirewallControl{Action: "apply", Rules: rules, Revision: preview.Revision,
				Available: preview.Available, Reserved: preview.Reserved}
			stop := probe()
			defer stop()
			var refused firewallIntent
			armed, cleanupFailed := false, false
			fault := func(commandContext context.Context, args ...string) (string, error) {
				if args[0] == "/usr/bin/systemd-run" {
					state, err := readFirewallJournal(scope, owner)
					if err != nil || state.Pending == nil {
						return "", errors.Join(err, errors.New("missing arming intent"))
					}
					refused = *state.Pending
					// The test executable is not the installed revert helper.
					for index, argument := range args {
						if argument == "--collect" {
							args[index+1] = binary
						}
					}
					var output string
					var armErr error
					if mode == "reexec" {
						reexec := exec.CommandContext(commandContext, "/usr/bin/systemctl", "daemon-reexec")
						if err := reexec.Start(); err != nil {
							return "", err
						}
						output, armErr = snapshotHostCommand(commandContext, args...)
						reexecErr := reexec.Wait()
						t.Logf("real daemon-reexec overlap: arm_error=%v manager_error=%v output=%q", armErr, reexecErr, output)
						if reexecErr != nil {
							t.Error("manager did not recover", reexecErr)
						}
					} else {
						output, armErr = snapshotHostCommand(commandContext, args...)
					}
					armed = armErr == nil
					if mode == "reexec" {
						return output, armErr
					}
					if armErr != nil {
						t.Error("injected lost-ack boundary requires a successfully created real timer", armErr)
					}
					return output, errors.New("injected lost systemd-run acknowledgement")
				}
				if mode == "cleanup-restart" && !cleanupFailed && args[0] == "/usr/bin/systemctl" && args[1] == "stop" && args[2] == firewallUnit(owner)+".timer" && armed {
					cleanupFailed = true
					return "", errors.New("injected unavailable cleanup acknowledgement")
				}
				return snapshotHostCommand(commandContext, args...)
			}
			_, err = runHostFirewall(ctx, root, owner, control, fault)
			if mode == "reexec" && err == nil {
				call(FirewallControl{Action: "revert"})
				t.Fatal("fixture did not reproduce an unconfirmed arming during real reexec")
			}
			if err == nil || !strings.Contains(err.Error(), "no firewall policy was applied") {
				t.Fatal("apply was not refused safely", err)
			}
			t.Log("refused apply:", err)
			if mode == "cleanup-restart" {
				if !cleanupFailed || !strings.Contains(err.Error(), "cleanup is pending") {
					t.Fatal("cleanup failure was not retained", err)
				}
				if _, err := os.Stat(filepath.Join(scope, "firewall-arming.json")); err != nil {
					t.Fatal("missing durable cleanup checkpoint", err)
				}
				call(FirewallControl{Action: "recover"})
			}
			after, err := os.ReadFile(journal)
			if before == nil && !os.IsNotExist(err) || before != nil && (err != nil || !bytes.Equal(before, after)) {
				t.Fatal("refused arming changed the protected journal", err)
			}
			for _, suffix := range []string{".timer", ".service"} {
				load, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "show", firewallUnit(owner)+suffix, "--property=LoadState", "--value")
				if err != nil || strings.TrimSpace(load) != "not-found" {
					t.Fatal("orphaned arming unit", suffix, load, err)
				}
			}
			for _, filename := range []string{filepath.Join(scope, "firewall-arming.json")} {
				if _, err := os.Lstat(filename); !os.IsNotExist(err) {
					t.Fatal("orphaned arming state", filename, err)
				}
			}
			if mode == "initial-lost-ack" {
				unit := firewallBootUnit(owner)
				load, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "show", unit, "--property=LoadState", "--value")
				if err != nil || strings.TrimSpace(load) != "not-found" {
					t.Fatal("orphaned initial boot recovery unit", load, err)
				}
				for _, filename := range []string{filepath.Join("/etc/systemd/system", unit), filepath.Join("/etc/systemd/system/multi-user.target.wants", unit)} {
					if _, err := os.Lstat(filename); !os.IsNotExist(err) {
						t.Fatal("orphaned initial boot recovery file", filename, err)
					}
				}
			}
			if ownAfter := ownedPolicy(); ownBefore != ownAfter {
				t.Fatalf("refused arming changed Cove's inet table: before=%q after=%q", ownBefore, ownAfter)
			}
			stampAfter, err := readFirewallDeadline(owner)
			if err != nil || (stampBefore == nil) != (stampAfter == nil) || stampBefore != nil && (!bytes.Equal(stampBefore.Data, stampAfter.Data) || !stampBefore.Modified.Equal(stampAfter.Modified) || stampBefore.Mode != stampAfter.Mode) {
				t.Fatal("refused arming changed the previous deadline state", err)
			}
			bootAfter, err := os.ReadFile(bootFile)
			if bootBefore == nil && !os.IsNotExist(err) || bootBefore != nil && (err != nil || !bytes.Equal(bootBefore, bootAfter)) {
				t.Fatal("refused arming changed the boot recovery unit", err)
			}
			t.Log("Cove-only inet table, journal, checkpoint, deadline state and recovery units are absent or unchanged after refusal")
			afterPolicy, err := snapshotHostCommand(ctx, "/usr/sbin/nft", "--stateless", "list", "ruleset")
			if err != nil || policy != afterPolicy {
				t.Fatalf("refused arming changed the firewall ruleset: %v\nbefore preview=%q\nafter preview=%q\nafter refusal=%q", err, policy, previewPolicy, afterPolicy)
			}
			retry := call(control)
			if retry.Pending == nil || retry.Pending.ID == refused.ID || !retry.Pending.Deadline.After(refused.Deadline) {
				t.Fatal("retry did not arm a fresh attempt and deadline", retry.Pending, refused)
			}
			call(FirewallControl{Action: "confirm", Intent: retry.Pending.ID})
			confirmed, err := os.ReadFile(journal)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "lost-ack" {
				// A delayed old invocation must be harmless even after another policy is confirmed.
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(time.Until(refused.Deadline.Add(2 * time.Second))):
				}
			}
			output, err := exec.CommandContext(ctx, binary, "firewall-revert-attempt", root, owner, refused.ID).CombinedOutput()
			if err != nil || !strings.Contains(string(output), "stale attempt ignored") {
				t.Fatal("stale invocation did not report its no-op", err, string(output))
			}
			after, err = os.ReadFile(journal)
			if err != nil || !bytes.Equal(confirmed, after) {
				t.Fatal("stale attempt changed the confirmed journal", err)
			}
			if _, err := firewallTestRequest(ctx, namespace, application); err == nil {
				t.Fatal("old deadline or invocation reverted the confirmed policy")
			}
			if detail, err := firewallTestRequest(ctx, namespace, management); err != nil {
				t.Fatal("management unavailable after refused apply and retry", err, detail)
			}
			t.Logf("refused attempt %s deadline %s preserved later confirmed attempt %s", refused.ID, refused.Deadline.Format(time.RFC3339Nano), retry.Pending.ID)
			// Restore the initial allowed application policy for the next independent boundary.
			preview = call(FirewallControl{Action: "preview", Rules: []FirewallRule{{Protocol: "tcp", Port: application, Sources: []string{"198.19.255.2/32"}}}})
			applied := call(FirewallControl{Action: "apply", Rules: preview.Rules, Revision: preview.Revision})
			call(FirewallControl{Action: "confirm", Intent: applied.Pending.ID})
		})
		if t.Failed() {
			return
		}
	}
	t.Logf("All arming refusals preserved management on port %d", management)
}
