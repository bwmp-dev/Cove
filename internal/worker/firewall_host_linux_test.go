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
	"sync"
	"testing"
	"time"
)

func TestHostFirewallTimedRevertOnOwnedLinuxVM(t *testing.T) {
	testHostFirewallTimedRevertOnOwnedLinuxVM(t, nil, false, false)
}

func TestHostFirewallRevertSurvivesManagerChangesOnOwnedLinuxVM(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		commands []string
	}{
		{"reload", []string{"daemon-reload"}},
		{"reexec", []string{"daemon-reexec"}},
		{"both", []string{"daemon-reload", "daemon-reexec"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			testHostFirewallTimedRevertOnOwnedLinuxVM(t, fixture.commands, false, false)
		})
	}
}

func TestHostFirewallArmingRefusalOnOwnedLinuxVM(t *testing.T) {
	testHostFirewallTimedRevertOnOwnedLinuxVM(t, nil, true, false)
}

func testHostFirewallTimedRevertOnOwnedLinuxVM(t *testing.T, managerChanges []string, armingRefusals, removal bool) {
	binary := os.Getenv("COVE_TEST_FIREWALL_BINARY")
	if os.Getenv("COVE_TEST_OWNED_HOST_INSPECTION") != "1" || os.Geteuid() != 0 || binary == "" {
		t.Skip("explicit disposable root Linux VM and worker binary required")
	}
	timeout := 4 * time.Minute
	if armingRefusals {
		timeout = 10 * time.Minute
	}
	if removal {
		timeout = 12 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
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
	// Keep udev from replacing a MAC already cached by the peer.
	macSuffix := fmt.Sprintf("%s:%s:%s:%s:%s", owner[:2], owner[2:4], owner[4:6], owner[6:8], owner[8:10])
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
		if stamp, err := readFirewallDeadline(owner); err != nil {
			t.Error(err)
		} else if stamp != nil {
			identity, err := readHostFile(filepath.Join(root, "host-maintenance", "owner"))
			if err != nil || string(identity) != owner {
				t.Error("refusing deadline cleanup without fixture ownership", err)
			} else if err := os.Remove(firewallDeadlineFile(owner)); err != nil {
				t.Error(err)
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
		if createdLink {
			if _, err := snapshotHostCommand(cleanup, "/usr/sbin/ip", "link", "show", hostLink); err == nil {
				if _, err := snapshotHostCommand(cleanup, "/usr/sbin/ip", "link", "delete", hostLink); err != nil {
					t.Error(err)
				}
			}
		}
		if createdNamespace {
			if _, err := snapshotHostCommand(cleanup, "/usr/sbin/ip", "netns", "delete", namespace); err != nil {
				t.Error(err)
			}
		}
	})
	for _, args := range [][]string{
		{"netns", "add", namespace},
		{"link", "add", hostLink, "address", "02:" + macSuffix, "type", "veth", "peer", "name", peer, "address", "06:" + macSuffix},
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
	var applyProbes sync.WaitGroup
	t.Cleanup(applyProbes.Wait)
	t.Cleanup(func() {
		if t.Failed() {
			logFirewallTestDiagnostics(t, ctx, root, owner, namespace, hostLink, management)
		}
	})
	request := func(requestContext context.Context, port uint16) error {
		detail, err := firewallTestRequest(requestContext, namespace, port)
		if err != nil && port == management {
			logFirewallTestDiagnostics(t, ctx, root, owner, namespace, hostLink, management)
		}
		if err != nil {
			return fmt.Errorf("%w: %s", err, detail)
		}
		return nil
	}
	ready, cancelReady := context.WithTimeout(ctx, 15*time.Second)
	for {
		err = request(ready, application)
		if err != nil {
			t.Log("pre-policy application readiness attempt", err)
		}
		if err == nil || ready.Err() != nil {
			break
		}
		select {
		case <-ready.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
	cancelReady()
	if err != nil {
		logFirewallTestDiagnostics(t, ctx, root, owner, namespace, hostLink, management)
		t.Fatal("application never became ready before filtering", err)
	}
	applyNumber := 0
	probeApply := func() func() {
		applyNumber++
		number := applyNumber
		stopped := make(chan struct{})

		applyProbes.Add(1)
		go func() {
			defer applyProbes.Done()
			for attempt := 1; ; attempt++ {
				select {
				case <-stopped:
					return
				case <-ctx.Done():
					return
				default:
				}
				started := time.Now()
				detail, err := firewallTestRequest(ctx, namespace, management)
				t.Logf("management probe apply=%d attempt=%d started=%s duration=%s error=%v cancelled=%v detail=%q", number, attempt, started.UTC().Format(time.RFC3339Nano), time.Since(started), err, ctx.Err(), detail)
				var exit *exec.ExitError
				if err != nil && (ctx.Err() == nil || errors.As(err, &exit) && exit.ExitCode() == 28) {
					logFirewallTestDiagnostics(t, ctx, root, owner, namespace, hostLink, management)
					t.Error("management connection failed across firewall apply", err, detail)
				}
			}
		}()
		t.Logf("firewall apply=%d started=%s application=%d management=%d", number, time.Now().UTC().Format(time.RFC3339Nano), application, management)
		return func() { close(stopped) }
	}
	call := func(control FirewallControl) FirewallResult {
		t.Helper()
		control.Available = []FirewallPort{{Protocol: "tcp", Port: application}, {Protocol: "tcp", Port: management}, {Protocol: "tcp", Port: 80}, {Protocol: "tcp", Port: 443}}
		control.Reserved = []uint16{22, management, 80, 443}
		input, err := json.Marshal(firewallHostRequest{Root: root, Owner: owner, Control: control})
		if err != nil {
			t.Fatal(err)
		}
		if control.Action == "apply" {
			stop := probeApply()
			defer stop()
		}
		command := exec.CommandContext(ctx, binary, "host-firewall")
		command.Stdin = bytes.NewReader(input)
		output, err := command.Output()
		if control.Action == "apply" {
			t.Logf("firewall apply=%d returned=%s error=%v", applyNumber, time.Now().UTC().Format(time.RFC3339Nano), err)
		}
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
		for _, port := range response.State.Available {
			if port.Port == 80 || port.Port == 443 || port.Port == management {
				t.Fatal("reserved port offered as openable", port)
			}
		}
		if control.Action == "apply" {
			t.Logf("firewall apply=%d active=%v pending=%v", applyNumber, response.State.Active, response.State.Pending != nil)
		}
		return response.State
	}
	if removal {
		testFirewallRemoval(t, ctx, root, owner, binary, application, management, call, request)
		return
	}
	if armingRefusals {
		testFirewallArmingRefusals(t, ctx, root, owner, binary, application, management, namespace, call, probeApply)
		return
	}
	rules := []FirewallRule{{Protocol: "tcp", Port: application, Sources: []string{}}}
	preview := call(FirewallControl{Action: "preview", Rules: rules})
	applied := call(FirewallControl{Action: "apply", Rules: rules, Revision: preview.Revision})
	armedAt := time.Now()
	if applied.Pending == nil || request(ctx, application) == nil {
		t.Fatal("unconfirmed policy did not block the owned application")
	}
	if err = request(ctx, management); err != nil {
		t.Fatal("reserved management connection was cut", err)
	}
	readDeadline := func() string {
		t.Helper()
		raw, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "show", firewallUnit(owner)+".timer", "--property=ActiveState,NextElapseUSecRealtime,NextElapseUSecMonotonic")
		if err != nil {
			t.Fatal("read recovery timer", err)
		}
		properties := map[string]string{}
		for _, line := range strings.Split(raw, "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				properties[key] = value
			}
		}
		if properties["ActiveState"] != "active" {
			return ""
		}
		if value := properties["NextElapseUSecRealtime"]; value != "" {
			return value
		}
		return properties["NextElapseUSecMonotonic"]
	}
	if len(managerChanges) > 0 {
		original := readDeadline()
		if original == "" || original == "0" {
			t.Fatal("recovery timer was not armed")
		}
		for index, command := range managerChanges {
			delay := 67500 * time.Millisecond
			if index > 0 {
				delay = 145800 * time.Millisecond
			}
			timer := time.NewTimer(time.Until(armedAt.Add(delay)))
			select {
			case <-ctx.Done():
				timer.Stop()
				t.Fatal(ctx.Err())
			case <-timer.C:
			}
			if _, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", command); err != nil {
				t.Fatal("manager change failed", command, err)
			}
			after := readDeadline()
			t.Logf("%s: recovery deadline %q -> %q", command, original, after)
			if after != original && (after != "" && after != "0" || request(ctx, application) != nil) {
				t.Fatal("manager change postponed or lost the recovery deadline", command, original, after)
			}
		}
	}
	deadline := armedAt.Add(150 * time.Second)
	for time.Now().Before(deadline) && request(ctx, application) != nil {
		time.Sleep(2 * time.Second)
	}
	if err = request(ctx, application); err != nil {
		t.Fatal("independent systemd recovery did not restore the application", err)
	}
	waitFirewallRecoveryComplete(t, ctx, owner)
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
	if confirmed.Pending != nil || !confirmed.Active || request(ctx, application) != nil || request(ctx, management) != nil {
		t.Fatal("confirmed source allow-list or management protection failed", confirmed)
	}
	if _, err = snapshotHostCommand(ctx, "/usr/sbin/nft", "delete", "table", "inet", firewallTable(owner)); err != nil {
		t.Fatal(err)
	}
	if _, err = snapshotHostCommand(ctx, "/usr/bin/systemctl", "start", firewallBootUnit(owner)); err != nil {
		t.Fatal("boot policy failed", err)
	}
	restored := call(FirewallControl{Action: "inspect"})
	if !restored.Active || len(restored.Rules) != 1 || request(ctx, application) != nil || request(ctx, management) != nil {
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
	if len(restored.Rules) != 0 || !strings.Contains(restored.LastError, "management listeners") || request(ctx, application) != nil || request(ctx, management) != nil {
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

func firewallTestRequest(ctx context.Context, namespace string, port uint16) (string, error) {
	command := exec.CommandContext(ctx, "/usr/sbin/ip", "netns", "exec", namespace, "/usr/bin/curl", "--silent", "--show-error", "--fail", "--max-time", "2", "--write-out", "\nprobe http=%{http_code} connect=%{time_connect} starttransfer=%{time_starttransfer} total=%{time_total} local=%{local_ip}:%{local_port} remote=%{remote_ip}:%{remote_port}\n", fmt.Sprintf("http://198.19.255.1:%d", port))
	command.Dir = "/"
	output := &limitedBuffer{Limit: 64 << 10}
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	return output.data.String(), err
}

func logFirewallTestDiagnostics(t *testing.T, ctx context.Context, root, owner, namespace, hostLink string, management uint16) {
	t.Helper()
	diagnostic, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	port := fmt.Sprint(management)
	if state, err := readFirewallJournal(filepath.Join(root, "host-maintenance"), owner); err != nil {
		t.Log("firewall diagnostic journal read", err)
	} else {
		t.Logf("firewall diagnostic protected state removed=%v generation=%s pending=%+v", state.Removed, state.Generation, state.Pending)
	}
	for _, args := range [][]string{
		{"/usr/bin/journalctl", "--no-pager", "-u", firewallUnit(owner) + ".service", "-u", firewallBootUnit(owner), "-n", "80"},
		{"/usr/sbin/nft", "--handle", "list", "table", "inet", firewallTable(owner)},
		{"/usr/sbin/nft", "--json", "list", "tables"},
		{"/usr/sbin/ip", "-json", "route", "list"},
		{"/usr/sbin/ip", "netns", "exec", namespace, "/usr/sbin/ip", "-json", "route", "list"},
		{"/usr/sbin/ip", "-json", "neigh", "show", "dev", hostLink},
		{"/usr/sbin/ip", "netns", "exec", namespace, "/usr/sbin/ip", "-json", "neigh", "show"},
		{"/usr/sbin/ip", "-statistics", "link", "show", "dev", hostLink},
		{"/usr/sbin/conntrack", "-L", "--proto", "tcp", "--orig-src", "198.19.255.2", "--dport", port, "--output", "extended"},
		{"/usr/bin/ss", "-lntp", "sport = :" + port},
		{"/usr/bin/ss", "-ntop", "( sport = :" + port + " or dport = :" + port + " )"},
		{"/usr/bin/systemctl", "show", firewallUnit(owner) + ".timer", firewallUnit(owner) + ".service", firewallBootUnit(owner), "--property=Id,ActiveState,SubState,NextElapseUSecMonotonic,NextElapseUSecRealtime,LastTriggerUSec,ExecMainStatus"},
		{"/usr/bin/journalctl", "--no-pager", "--output=short-monotonic", "-n", "30", "-u", firewallUnit(owner) + ".timer", "-u", firewallUnit(owner) + ".service"},
		{"/usr/bin/cat", "/proc/loadavg", "/proc/uptime"},
		{"/usr/bin/curl", "--silent", "--show-error", "--fail", "--max-time", "2", "--write-out", " http=%{http_code} connect=%{time_connect} starttransfer=%{time_starttransfer} total=%{time_total}", "http://127.0.0.1:" + port},
		{"/usr/bin/curl", "--silent", "--show-error", "--fail", "--max-time", "2", "--write-out", " http=%{http_code} connect=%{time_connect} starttransfer=%{time_starttransfer} total=%{time_total}", "http://198.19.255.1:" + port},
	} {
		raw, err := snapshotHostCommand(diagnostic, args...)
		t.Logf("firewall diagnostic at %s: %q error=%v output=%s", time.Now().UTC().Format(time.RFC3339Nano), args, err, raw)
	}
	state, readErr := readFirewallJournal(filepath.Join(root, "host-maintenance"), owner)
	data, _ := json.Marshal(state)
	t.Logf("owned firewall journal error=%v state=%s", readErr, data)
}
