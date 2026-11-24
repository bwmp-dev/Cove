package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestListenerMergeNodeChangeLeaseAndDurableMove(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	unlock, err := e.lockNodeChange(false)
	if err != nil {
		t.Fatal(err)
	}
	if next, err := e.lockNodeChange(true); err == nil {
		next()
		t.Fatal("concurrent node mutation acquired the process lease")
	}
	unlock()
	j := moveJournal{MoveState: MoveState{ID: strings.Repeat("a", 32), Workload: "move-app", Role: "source", Phase: "freezing"}}
	if err := e.saveMove(&j); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ChangeIngressExposures(context.Background(), IngressExposureRequest{}); err == nil || !strings.Contains(err.Error(), "node move") {
		t.Fatal("listener replacement ignored durable move", err)
	}

	unlock, err = e.lockNodeChange(true)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	j.Phase = "committed"
	if err = e.saveMove(&j); err != nil {
		t.Fatal(err)
	}
	unlock, err = e.lockNodeChange(false)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestListenerMergeScheduledUpdatesRespectProcessAndRecovery(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("trusted native host helper requires Linux root; real VM acceptance runs this path")
	}
	e := &Engine{Root: t.TempDir()}
	owner, err := e.backupHelperOwner(true)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(e.Root, "host-maintenance"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.Root, "host-maintenance", "owner"), []byte(owner), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	run := func(_ context.Context, args ...string) (string, error) {
		calls++
		if len(args) != 1 || args[0] != "/usr/bin/unattended-upgrade" {
			t.Fatal(args)
		}
		return "", nil
	}
	unlock, err := e.lockNodeChange(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = runScheduledHostSecurity(context.Background(), e.Root, owner, "apt", run); err == nil || calls != 0 {
		t.Fatal("scheduled updates overlapped proxy change", err, calls)
	}
	unlock()
	if err = os.WriteFile(filepath.Join(e.Root, "proxy-traffic-change.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = runScheduledHostSecurity(context.Background(), e.Root, owner, "apt", run); err == nil || calls != 0 {
		t.Fatal("scheduled updates ignored durable recovery", err, calls)
	}
	if err = os.Remove(filepath.Join(e.Root, "proxy-traffic-change.json")); err != nil {
		t.Fatal(err)
	}
	j := moveJournal{MoveState: MoveState{ID: strings.Repeat("a", 32), Workload: "moving", Role: "source", Phase: "frozen"}}
	if err = e.saveMove(&j); err != nil {
		t.Fatal(err)
	}
	if err = runScheduledHostSecurity(context.Background(), e.Root, owner, "apt", run); err == nil || calls != 0 {
		t.Fatal("scheduled update ignored durable move", err, calls)
	}
	j.Phase = "committed"
	if err = e.saveMove(&j); err != nil {
		t.Fatal(err)
	}
	if err = runScheduledHostSecurity(context.Background(), e.Root, owner, "apt", run); err != nil || calls != 1 {
		t.Fatal("idle owned host schedule did not reach update tool", err, calls)
	}
}

func TestListenerMergeSharedProxyFirewallInventory(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	e.Run = func(_ context.Context, _ string, args []string, _ []string, out io.Writer) error {
		if args[1] == "ps" {
			if strings.Contains(strings.Join(args, " "), "cove.managed=ingress") {
				io.WriteString(out, strings.Repeat("a", 64)+"\n"+strings.Repeat("b", 64))
			}
			return nil
		}
		if args[1] != "inspect" {
			t.Fatal(args)
		}
		json.NewEncoder(out).Encode(map[string]any{"name": "/cove-ingress", "labels": map[string]string{"cove.managed": "ingress"}, "ports": map[string]any{"443/tcp": []map[string]string{{"HostPort": "9443"}}, "443/udp": []map[string]string{{"HostPort": "443"}}}})
		json.NewEncoder(out).Encode(map[string]any{"name": "/foreign-helper", "labels": map[string]string{"cove.managed": "ingress"}, "ports": map[string]any{"80/tcp": []map[string]string{{"HostPort": "1234"}}}})
		return nil
	}
	ports, err := e.firewallPorts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 || ports[0] != (FirewallPort{Protocol: "tcp", Port: 9443}) || ports[1] != (FirewallPort{Protocol: "udp", Port: 443}) {
		t.Fatal("shared HTTP3/raw publications absent or foreign owner included", ports)
	}
}

func TestListenerMergePublicHTTPClassification(t *testing.T) {
	public := Route{Domain: "*.example.test", Service: "app", Port: 80}
	private := public
	private.Policy = &RoutePolicy{Visibility: "lan", Listener: "office"}
	for _, tt := range []struct {
		route   Route
		allowed bool
	}{{public, true}, {private, false}, {Route{Protocol: "tcp", Service: "app", Port: 1234}, false}} {
		if tt.route.IsPublicHTTPRoute() != tt.allowed {
			t.Fatal(tt)
		}
	}
	if !PublicHTTPRouteDomain([]Route{public}, "app.example.test") || PublicHTTPRouteDomain([]Route{private}, "app.example.test") || PublicHTTPRouteDomain([]Route{public}, "*.example.test") {
		t.Fatal("private or literal wildcard selected for public check")
	}
}

func TestListenerMergeDrainCountsNodeTransactions(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	unlock, err := e.lockNodeChange(true)
	if err != nil {
		t.Fatal(err)
	}
	state, err := e.Maintenance(true)
	if err != nil || state.Active == 0 {
		t.Fatal("updater ignored active node transaction", state, err)
	}
	unlock()
	e.proxyUnsettled = true
	state, err = e.Maintenance(true)
	if err != nil || state.Active == 0 {
		t.Fatal("updater ignored proxy recovery", state, err)
	}
	e.proxyUnsettled = false
	state, err = e.Maintenance(false)
	if err != nil || state.Active != 0 {
		t.Fatal("idle worker retained maintenance hold", state, err)
	}
}

func TestListenerMergeLegacyScheduleMustBeResaved(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("native host policy ownership requires Linux root")
	}
	e := &Engine{Root: t.TempDir()}
	owner, err := e.backupHelperOwner(true)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.requireInterlockedHostSchedule(); err != nil {
		t.Fatal("new worker blocked", err)
	}
	if err = os.MkdirAll(filepath.Join(e.Root, "host-maintenance"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = e.requireInterlockedHostSchedule(); err == nil {
		t.Fatal("legacy direct package timer accepted")
	}
	run := func(context.Context, ...string) (string, error) { return "inactive", nil }
	if err = markInterlockedHostSchedule(t.Context(), e.Root, owner, run); err != nil {
		t.Fatal(err)
	}
	if err = e.requireInterlockedHostSchedule(); err != nil {
		t.Fatal("saved interlocked policy rejected", err)
	}
	service, _, err := hostPolicyFiles("apt", owner, e.Root)
	if err != nil || !strings.Contains(service, " host-security-run "+e.Root+" "+owner+" apt") || strings.Contains(service, "ExecStart=/usr/bin/unattended-upgrade") {
		t.Fatal("schedule bypasses trusted lease helper", err)
	}
}

func TestListenerMergeNoProxyRecoveryDoesNotBlockLegacyWorker(t *testing.T) {
	e := &Engine{Root: t.TempDir(), proxyUnsettled: true}
	if err := os.MkdirAll(filepath.Join(e.Root, "host-maintenance"), 0700); err != nil {
		t.Fatal(err)
	}
	e.runProxyRecovery(t.Context())
	if e.proxyUnsettled {
		t.Fatal("an old host timer with no proxy recovery blocked ordinary dispatch")
	}
}

func TestListenerMergeIngressDefersDuringNativeHostTransaction(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	unlock, err := e.lockNodeChange(true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		t.Fatal("proxy touched Docker during native host maintenance")
		return nil
	}
	if err = e.syncIngress(t.Context(), map[string][]Route{}, io.Discard); err == nil || !strings.Contains(err.Error(), "another proxy") {
		t.Fatal("shared ingress ignored the native host lease", err)
	}
}

func TestListenerMergeScheduleMigrationRequiresConfirmedInactiveService(t *testing.T) {
	for _, state := range []string{"active", "activating", "deactivating", "reloading", "unknown", ""} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			run := func(context.Context, ...string) (string, error) {
				if state == "" {
					return "", errors.New("systemd unavailable")
				}
				return state, nil
			}
			if err := markInterlockedHostSchedule(t.Context(), root, "owner", run); err == nil {
				t.Fatal("unconfirmed or running host service was marked safe for proxy replacement")
			}
			if _, err := os.Lstat(filepath.Join(root, "node-changes", "host-policy.json")); !os.IsNotExist(err) {
				t.Fatal("unconfirmed service left a migration marker", err)
			}
		})
	}
	for _, state := range []string{"inactive", "failed"} {
		t.Run(state, func(t *testing.T) {
			run := func(_ context.Context, args ...string) (string, error) {
				if len(args) != 5 || strings.Join(args[:4], " ") != "/usr/bin/systemctl show --property=ActiveState --value" || args[4] != hostPolicyUnit("owner")+".service" {
					t.Fatal("host status must be confirmed with systemctl show", args)
				}
				return state, nil
			}
			if err := markInterlockedHostSchedule(t.Context(), t.TempDir(), "owner", run); err != nil {
				t.Fatal(err)
			}
		})
	}
}
