package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostFirewallTimedRevertOnOwnedLinuxVM(t *testing.T) {
	binary := os.Getenv("COVE_TEST_FIREWALL_BINARY")
	if os.Getenv("COVE_TEST_OWNED_HOST_INSPECTION") != "1" || os.Geteuid() != 0 || binary == "" {
		t.Skip("explicit disposable root Linux VM and worker binary required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	root := t.TempDir()
	owner, err := firewallChangeID()
	if err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(root, "backup-helper-owner.json"), struct{ ID string }{ID: owner}); err != nil {
		t.Fatal(err)
	}
	if receipt := os.Getenv("COVE_TEST_FIREWALL_RECEIPT"); receipt != "" {
		if err = writeJSON(receipt, map[string]string{"root": root, "owner": owner}); err != nil {
			t.Fatal(err)
		}
	}
	namespace, hostLink, peer := "cove-fw-"+owner[:8], "cvfh"+owner[:8], "cvfp"+owner[:8]
	createdNamespace, createdLink := false, false
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, suffix := range []string{".timer", ".service"} {
			unit := firewallUnit(owner) + suffix
			description, _ := snapshotHostCommand(cleanup, "/usr/bin/systemctl", "show", unit, "--property=Description", "--value")
			if strings.TrimSpace(description) == "Cove firewall recovery "+owner {
				if _, err := snapshotHostCommand(cleanup, "/usr/bin/systemctl", "stop", unit); err != nil {
					t.Error(err)
				}
			}
		}
		if _, err := os.Lstat(filepath.Join("/etc/systemd/system", firewallBootUnit(owner))); err == nil {
			if err := removeFirewallBootPolicy(cleanup, owner, snapshotHostCommand); err != nil {
				t.Error(err)
			}
		}
		present, comment, _, err := inspectFirewallTable(cleanup, owner, snapshotHostCommand)
		if err != nil {
			t.Error(err)
		} else if present {
			if !strings.HasPrefix(comment, "Cove owner "+owner+" generation ") {
				t.Error("refusing unexpected test firewall ownership")
			} else if _, err := snapshotHostCommand(cleanup, "/usr/sbin/nft", "delete", "table", "inet", firewallTable(owner)); err != nil {
				t.Error(err)
			}
		}
		if createdNamespace {
			if _, err := snapshotHostCommand(cleanup, "/usr/sbin/ip", "netns", "delete", namespace); err != nil {
				t.Error(err)
			}
		}
		if createdLink {
			if _, err := snapshotHostCommand(cleanup, "/usr/sbin/ip", "link", "show", hostLink); err == nil {
				if _, err := snapshotHostCommand(cleanup, "/usr/sbin/ip", "link", "delete", hostLink); err != nil {
					t.Error(err)
				}
			}
		}
	})
	for _, args := range [][]string{
		{"netns", "add", namespace},
		{"link", "add", hostLink, "type", "veth", "peer", "name", peer},
		{"link", "set", peer, "netns", namespace},
		{"addr", "add", "198.19.255.1/30", "dev", hostLink},
		{"link", "set", hostLink, "up"},
		{"netns", "exec", namespace, "/usr/sbin/ip", "addr", "add", "198.19.255.2/30", "dev", peer},
		{"netns", "exec", namespace, "/usr/sbin/ip", "link", "set", peer, "up"},
		{"netns", "exec", namespace, "/usr/sbin/ip", "link", "set", "lo", "up"},
	} {
		if _, err := snapshotHostCommand(ctx, append([]string{"/usr/sbin/ip"}, args...)...); err != nil {
			t.Fatal(err)
		}
		if args[0] == "netns" && args[1] == "add" {
			createdNamespace = true
		}
		if args[0] == "link" && args[1] == "add" {
			createdLink = true
		}
	}
	serve := func() (*httptest.Server, uint16) {
		listener, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "owned-firewall-test") }))
		server.Listener = listener
		server.Start()
		t.Cleanup(server.Close)
		return server, uint16(listener.Addr().(*net.TCPAddr).Port)
	}
	_, application := serve()
	_, management := serve()
	request := func(port uint16) error {
		_, err := snapshotHostCommand(ctx, "/usr/sbin/ip", "netns", "exec", namespace, "/usr/bin/curl", "--silent", "--fail", "--max-time", "2", fmt.Sprintf("http://198.19.255.1:%d", port))
		return err
	}
	if err = request(application); err != nil {
		t.Fatal("test application was not reachable before filtering", err)
	}
	call := func(control FirewallControl) FirewallResult {
		t.Helper()
		control.Available = []FirewallPort{{Protocol: "tcp", Port: application}, {Protocol: "tcp", Port: management}}
		control.Reserved = []uint16{22, management}
		input, err := json.Marshal(firewallHostRequest{Root: root, Owner: owner, Control: control})
		if err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(ctx, binary, "host-firewall")
		command.Stdin = bytes.NewReader(input)
		output, err := command.Output()
		if err != nil {
			var failure *exec.ExitError
			if errors.As(err, &failure) {
				t.Log(string(failure.Stderr[:min(4096, len(failure.Stderr))]))
			}
			t.Fatal("native firewall helper failed", err)
		}
		var response struct {
			State FirewallResult
			Error string
		}
		if json.Unmarshal(output, &response) != nil || response.Error != "" || !response.State.Supported {
			t.Fatal("native firewall change refused", response.Error, response.State.Notice)
		}
		return response.State
	}
	rules := []FirewallRule{{Protocol: "tcp", Port: application, Sources: []string{}}}
	preview := call(FirewallControl{Action: "preview", Rules: rules})
	applied := call(FirewallControl{Action: "apply", Rules: rules, Revision: preview.Revision})
	if applied.Pending == nil || request(application) == nil {
		t.Fatal("unconfirmed policy did not block the owned application")
	}
	if err = request(management); err != nil {
		t.Fatal("reserved management connection was cut", err)
	}
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) && request(application) != nil {
		time.Sleep(2 * time.Second)
	}
	if err = request(application); err != nil {
		t.Fatal("independent systemd recovery did not restore the application", err)
	}
	inspected := call(FirewallControl{Action: "inspect"})
	if inspected.Pending != nil || inspected.Active {
		t.Fatal("timed recovery left its temporary policy pending", inspected)
	}
	rules[0].Sources = []string{"198.19.255.2/32"}
	preview = call(FirewallControl{Action: "preview", Rules: rules})
	applied = call(FirewallControl{Action: "apply", Rules: rules, Revision: preview.Revision})
	if applied.Pending == nil {
		t.Fatal("second policy lacks recovery intent")
	}
	confirmed := call(FirewallControl{Action: "confirm", Intent: applied.Pending.ID})
	if confirmed.Pending != nil || !confirmed.Active || request(application) != nil || request(management) != nil {
		t.Fatal("confirmed source allow-list or management protection failed", confirmed)
	}
	if _, err = snapshotHostCommand(ctx, "/usr/sbin/nft", "delete", "table", "inet", firewallTable(owner)); err != nil {
		t.Fatal(err)
	}
	if _, err = snapshotHostCommand(ctx, "/usr/bin/systemctl", "start", firewallBootUnit(owner)); err != nil {
		t.Fatal("boot policy failed", err)
	}
	restored := call(FirewallControl{Action: "inspect"})
	if !restored.Active || len(restored.Rules) != 1 || request(application) != nil || request(management) != nil {
		t.Fatal("boot restoration failed", restored)
	}
	scope, err := hostPolicyScope(root, owner)
	if err != nil {
		t.Fatal(err)
	}
	state, err := readFirewallJournal(scope, owner)
	if err != nil {
		t.Fatal(err)
	}
	state.Reserved = append(state.Reserved, application)
	if err = writeJSON(filepath.Join(scope, "firewall.json"), state); err != nil {
		t.Fatal(err)
	}
	if _, err = snapshotHostCommand(ctx, "/usr/bin/systemctl", "start", firewallBootUnit(owner)); err != nil {
		t.Fatal("management-safe recovery failed", err)
	}
	restored = call(FirewallControl{Action: "inspect"})
	if len(restored.Rules) != 0 || !strings.Contains(restored.LastError, "management listeners") || request(application) != nil || request(management) != nil {
		t.Fatal("recovery retained a management-port rule", restored)
	}
	t.Log("Real ingress was filtered; independent two-minute revert restored it without worker confirmation; confirmed policy and boot restoration preserved management; changed management ports were excluded.")
}

func removeFirewallBootPolicy(ctx context.Context, owner string, run snapshotCommand) error {
	unit := firewallBootUnit(owner)
	filename := filepath.Join("/etc/systemd/system", unit)
	if err := ownedHostPolicyFile(filename, "# Managed by Cove firewall "+owner+"\n"); err != nil {
		return err
	}
	if _, err := run(ctx, "/usr/bin/systemctl", "disable", "--now", unit); err != nil {
		return err
	}
	if err := os.Remove(filename); err != nil {
		return err
	}
	_, err := run(ctx, "/usr/bin/systemctl", "daemon-reload")
	return err
}
