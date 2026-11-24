package worker

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostFirewallStartupRecoveryOnOwnedLinuxVM(t *testing.T) {
	binary := os.Getenv("COVE_TEST_FIREWALL_BINARY")
	if os.Getenv("COVE_TEST_OWNED_HOST_INSPECTION") != "1" || os.Geteuid() != 0 || binary == "" {
		t.Skip("explicit disposable root Linux VM, Docker and worker binary required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	t.Cleanup(cancel)
	root := t.TempDir()
	owner, err := firewallChangeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(root, "backup-helper-owner.json"), struct{ ID string }{owner}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
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
		if stamp, err := readFirewallDeadline(owner); err != nil {
			t.Error(err)
		} else if stamp != nil {
			if err := os.Remove(firewallDeadlineFile(owner)); err != nil {
				t.Error(err)
			}
		}
		present, comment, _, err := inspectFirewallTable(cleanup, owner, snapshotHostCommand)
		if err != nil {
			t.Error(err)
		} else if present {
			if !strings.HasPrefix(comment, "Cove owner "+owner+" generation ") {
				t.Error("refusing unexpected firewall fixture ownership")
			} else if _, err := snapshotHostCommand(cleanup, "/usr/sbin/nft", "delete", "table", "inet", firewallTable(owner)); err != nil {
				t.Error(err)
			}
		}
	})
	control := FirewallControl{Action: "preview", Rules: []FirewallRule{{Protocol: "tcp", Port: port}}, Available: []FirewallPort{{Protocol: "tcp", Port: port}}, Reserved: []uint16{22}}
	preview, err := runHostFirewall(ctx, root, owner, control, snapshotHostCommand)
	if err != nil || !preview.Supported {
		t.Fatal("native preview unavailable", err, preview.Notice)
	}
	control.Action, control.Revision = "apply", preview.Revision
	armed, cleanupRefused := false, false
	fault := func(commandContext context.Context, args ...string) (string, error) {
		if args[0] == "/usr/bin/systemd-run" {
			for index, argument := range args {
				if argument == "--collect" {
					args[index+1] = binary
				}
			}
			output, err := snapshotHostCommand(commandContext, args...)
			if err != nil {
				return output, err
			}
			armed = true
			return output, errors.New("injected lost arming acknowledgement")
		}
		if armed && !cleanupRefused && args[0] == "/usr/bin/systemctl" && args[1] == "stop" && args[2] == firewallUnit(owner)+".timer" {
			cleanupRefused = true
			return "", errors.New("injected cleanup unavailable")
		}
		return snapshotHostCommand(commandContext, args...)
	}
	if _, err := runHostFirewall(ctx, root, owner, control, fault); err == nil || !armed || !cleanupRefused || !strings.Contains(err.Error(), "cleanup is pending") {
		t.Fatal("refused arming did not retain a genuine recovery checkpoint", err)
	}
	checkpoint := filepath.Join(root, "host-maintenance", "firewall-arming.json")
	if _, err := os.Stat(checkpoint); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	heldScope, err := lockWorker(filepath.Join(root, "host-maintenance"))
	if err != nil {
		t.Fatal(err)
	}
	held := true
	t.Cleanup(func() {
		if held {
			heldScope()
		}
	})
	blockedEngine, err := NewEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	blockedEngine.hostHelperBinary = binary
	blockedContext, stopBlocked := context.WithCancel(ctx)
	t.Cleanup(stopBlocked)
	blockedDone := make(chan error, 1)
	blockedFinished := false
	t.Cleanup(func() {
		stopBlocked()
		if !blockedFinished {
			select {
			case <-blockedDone:
			case <-time.After(30 * time.Second):
				t.Error("blocked startup did not stop")
			}
		}
	})
	socket := filepath.Join(root, "worker.sock")
	go func() { blockedDone <- blockedEngine.Listen(blockedContext, socket) }()
	blockedUntil := time.Now().Add(90 * time.Second)
blockedStartup:
	for {
		if connection, err := net.DialTimeout("unix", socket, 100*time.Millisecond); err == nil {
			connection.Close()
			t.Fatal("RPC accepted a connection while recovery cleanup was locked")
		}
		select {
		case err := <-blockedDone:
			blockedFinished = true
			if err == nil || !strings.Contains(err.Error(), "firewall arming recovery") {
				t.Fatal("unexpected blocked startup result", err)
			}
			break blockedStartup
		default:
		}
		if time.Now().After(blockedUntil) || ctx.Err() != nil {
			t.Fatal("blocked startup did not refuse before opening RPC")
		}
		time.Sleep(100 * time.Millisecond)
	}
	stopBlocked()
	after, err := os.ReadFile(checkpoint)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed startup lost the cleanup checkpoint", err)
	}
	heldScope()
	held = false
	t.Log("Real startup refused while cleanup was locked and never opened RPC; the checkpoint survived")
	e, err := NewEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	e.hostHelperBinary = binary
	startup, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.Listen(startup, socket) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error("worker shutdown", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("worker did not stop")
		}
	})
	blocked := 0
	until := time.Now().Add(90 * time.Second)
	for {
		connection, dialErr := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if dialErr == nil {
			connection.Close()
			if _, err := os.Stat(checkpoint); !os.IsNotExist(err) {
				t.Fatal("RPC accepted a connection before arming cleanup completed", err)
			}
			break
		}
		blocked++
		select {
		case err := <-done:
			done <- err
			t.Fatal("worker startup failed", err)
		default:
		}
		if time.Now().After(until) || ctx.Err() != nil {
			t.Fatal("worker did not complete recovery before opening RPC", dialErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if blocked == 0 {
		t.Fatal("startup recovery was not observed blocking RPC")
	}
	for _, filename := range []string{checkpoint, filepath.Join(root, "host-maintenance", "firewall.json"), firewallDeadlineFile(owner), filepath.Join("/etc/systemd/system", firewallBootUnit(owner))} {
		if _, err := os.Lstat(filename); !os.IsNotExist(err) {
			t.Fatal("startup left refused arming state", filename, err)
		}
	}
	for _, suffix := range []string{".timer", ".service"} {
		load, err := snapshotHostCommand(ctx, "/usr/bin/systemctl", "show", firewallUnit(owner)+suffix, "--property=LoadState", "--value")
		if err != nil || strings.TrimSpace(load) != "not-found" {
			t.Fatal("startup left a recovery unit", suffix, load, err)
		}
	}
	if present, _, _, err := inspectFirewallTable(ctx, owner, snapshotHostCommand); err != nil || present {
		t.Fatal("startup changed the previously absent Cove firewall table", err)
	}
	t.Logf("Real worker startup cleaned the failed-ack checkpoint through its Docker/native helper before RPC accepted connections; %d blocked socket probes", blocked)
}
