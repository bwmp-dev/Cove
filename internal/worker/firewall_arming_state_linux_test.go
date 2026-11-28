package worker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFirewallArmingCleanupRestoresJournal(t *testing.T) {
	if os.Getenv("COVE_TEST_OWNED_HOST_INSPECTION") != "1" || os.Geteuid() != 0 {
		t.Skip("explicit disposable root Linux VM required for deadline-state cleanup")
	}
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			scope := t.TempDir()
			owner, err := firewallChangeID()
			if err != nil {
				t.Fatal(err)
			}
			attempt := strings.Repeat("b", 32)
			var previous []byte
			if existing {
				previous = []byte(`{"owner":"` + owner + `","rules":[],"generation":"","digest":"","last_error":"","reserved":[]}`)
			}
			marker := filepath.Join(scope, "firewall-arming.json")
			if err := writeJSON(marker, firewallArming{Owner: owner, Attempt: attempt, Previous: previous, BootExisted: true}); err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(scope, "firewall.json")
			if err := writeJSON(journal, firewallJournal{Owner: owner, Rules: []FirewallRule{}, Pending: &firewallIntent{ID: attempt, Deadline: time.Now().Add(time.Minute)}}); err != nil {
				t.Fatal(err)
			}
			unavailable := func(context.Context, ...string) (string, error) { return "", errors.New("manager unavailable") }
			if err := cleanupFirewallArming(t.Context(), scope, owner, unavailable); err == nil {
				t.Fatal("unconfirmed cleanup succeeded")
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("cleanup checkpoint was lost", err)
			}
			absent := func(_ context.Context, args ...string) (string, error) {
				if args[0] != "/usr/bin/systemctl" || args[1] != "show" {
					t.Fatal("unexpected cleanup command", args)
				}
				return "not-found\n", nil
			}
			if err := cleanupFirewallArming(t.Context(), scope, owner, absent); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(journal)
			if existing && (err != nil || !bytes.Equal(previous, after)) || !existing && !os.IsNotExist(err) {
				t.Fatal("journal was not restored byte for byte", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("cleanup checkpoint survived", err)
			}
		})
	}
}

func TestFirewallStaleRevertCannotTouchLaterAttempt(t *testing.T) {
	for _, pending := range []bool{false, true} {
		root := t.TempDir()
		owner, attempt := strings.Repeat("a", 32), strings.Repeat("b", 32)
		scope, err := hostPolicyScope(root, owner)
		if err != nil {
			t.Fatal(err)
		}
		state := firewallJournal{Owner: owner, Rules: []FirewallRule{}}
		if pending {
			state.Pending = &firewallIntent{ID: strings.Repeat("c", 32), AttemptBound: true, Deadline: time.Now().Add(time.Minute)}
		}
		filename := filepath.Join(scope, "firewall.json")
		if err := writeJSON(filename, state); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		run := func(context.Context, ...string) (string, error) {
			t.Fatal("stale revert ran a host command")
			return "", nil
		}
		for _, stale := range []string{attempt, "legacy"} {
			if err := revertFirewall(t.Context(), root, owner, stale, run); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := runHostFirewall(t.Context(), root, owner, FirewallControl{Action: "revert", Intent: attempt}, run); err == nil {
			t.Fatal("stale rollback RPC accepted a different pending attempt")
		}
		after, err := os.ReadFile(filename)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("stale revert changed the journal", err)
		}
	}
}

func TestFirewallInventoryExcludesReservedPortsBeforeEarlyReturn(t *testing.T) {
	if os.Getenv("COVE_TEST_OWNED_HOST_INSPECTION") != "1" || os.Geteuid() != 0 {
		t.Skip("explicit disposable root Linux fixture required for protected state")
	}
	for _, action := range []string{"recover", "inspect"} {
		t.Run(action, func(t *testing.T) {
			control := FirewallControl{Action: action, Reserved: []uint16{22, 80, 443}, Available: []FirewallPort{{Protocol: "tcp", Port: 80}, {Protocol: "tcp", Port: 443}, {Protocol: "tcp", Port: 8080}}}
			run := func(_ context.Context, args ...string) (string, error) {
				if action != "inspect" || len(args) == 0 || args[0] != "/usr/sbin/nft" {
					t.Fatal("unexpected host command", args)
				}
				return "", errors.New("injected nft inspection unavailable")
			}
			result, err := runHostFirewall(t.Context(), t.TempDir(), strings.Repeat("a", 32), control, run)
			if err != nil || len(result.Available) != 1 || result.Available[0] != (FirewallPort{Protocol: "tcp", Port: 8080}) {
				t.Fatal("reserved port exposed by early response", action, result.Available, err)
			}
			if result.Supported != (action == "recover") {
				t.Fatal("injected inspection failure was not exercised", result)
			}
		})
	}
}
