package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	oldRoute = Route{Domain: "old.example.com", Service: "web", Port: 80}
	newRoute = Route{Domain: "new.example.com", Service: "web", Port: 80}
)

// routeSnapshot is what the proxy serves and what the worker has persisted for the "app" workload.
type routeSnapshot struct {
	ingress []Route
	current []Route
	proxy   string
}

func readRoutes(t *testing.T, root string) routeSnapshot {
	t.Helper()
	var snapshot routeSnapshot
	if data, err := os.ReadFile(filepath.Join(root, "ingress.json")); err == nil {
		owned := map[string][]Route{}
		if err = json.Unmarshal(data, &owned); err != nil {
			t.Fatal(err)
		}
		snapshot.ingress = owned["app"]
	}
	data, err := os.ReadFile(filepath.Join(root, "workloads", "app", "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	var current deployedState
	if err = json.Unmarshal(data, &current); err != nil {
		t.Fatal(err)
	}
	snapshot.current = current.Routes
	proxy, _ := os.ReadFile(filepath.Join(root, "ingress.traefik.yaml"))
	snapshot.proxy = string(proxy)
	return snapshot
}

// assertRoutes checks that the persisted routes and the proxy all agree on want.
func assertRoutes(t *testing.T, root string, want Route) {
	t.Helper()
	other := oldRoute
	if want == oldRoute {
		other = newRoute
	}
	got := readRoutes(t, root)
	if !reflect.DeepEqual(got.ingress, []Route{want}) || !reflect.DeepEqual(got.current, []Route{want}) || !strings.Contains(got.proxy, want.Domain) || strings.Contains(got.proxy, other.Domain) {
		t.Fatalf("routes do not converge on %s: ingress=%v current=%v proxy=%s", want.Domain, got.ingress, got.current, got.proxy)
	}
}

func proxyServes(root, domain string) bool {
	data, _ := os.ReadFile(filepath.Join(root, "ingress.traefik.yaml"))
	return strings.Contains(string(data), domain)
}

func routeRequest(id string, routes ...Route) Request {
	return Request{ID: id, Workload: "app", Action: "routes", Kind: "service", Routes: routes}
}

func settle(t *testing.T, e *Engine) {
	t.Helper()
	var jobs sync.WaitGroup
	if err := e.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
}

func status(t *testing.T, e *Engine, id string) Operation {
	t.Helper()
	op, err := e.Status(id)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func retryRoutesImmediately(t *testing.T) {
	previous := routeRecoveryRetry
	routeRecoveryRetry = 0
	t.Cleanup(func() { routeRecoveryRetry = previous })
}

func TestRouteStateWriteFailureAfterProxyAcceptance(t *testing.T) {
	retryRoutesImmediately(t)
	web := newContainer("0123456789ab", "app", "web", "exited")
	before := *web
	e, docker := routeEngine(t, web)
	deployed(t, e, "app", []Route{oldRoute})
	current := filepath.Join(e.Root, "workloads", "app", "current.json")
	original, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	faulted := false
	docker.hook = func(command string) {
		// Once the proxy serves the new hostname, the disk stops accepting the release state.
		if !faulted && strings.Contains(command, " wget ") && proxyServes(e.Root, newRoute.Domain) {
			faulted = true
			if err := os.Remove(current); err != nil {
				t.Error(err)
			}
			if err := os.MkdirAll(filepath.Join(current, "fault"), 0700); err != nil {
				t.Error(err)
			}
		}
	}
	op := runRoutes(t, e, "change", "app", []Route{newRoute})
	if !faulted {
		t.Fatal("fault was not injected")
	}
	// The worker cannot confirm either outcome yet, so it must not report a terminal state.
	if op.State != "running" || op.Error != routeRestoring {
		t.Fatalf("unsettled route change reported %+v", op)
	}
	if again, err := e.Submit(routeRequest("change", newRoute)); err != nil || again.State != "running" {
		t.Fatalf("retried submit changed the unsettled operation: %+v %v", again, err)
	}
	if _, err = e.Submit(routeRequest("next", newRoute)); err != nil {
		t.Fatal(err)
	}
	settle(t, e)
	if op = status(t, e, "change"); op.State != "running" {
		t.Fatalf("route change settled while its state could not be written: %+v", op)
	}
	if next := status(t, e, "next"); next.State != "queued" {
		t.Fatalf("another operation ran while the route change was unsettled: %+v", next)
	}

	// After the disk recovers, the worker settles the operation without outside help.
	if err = os.RemoveAll(current); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(current, original, 0600); err != nil {
		t.Fatal(err)
	}
	settle(t, e)
	if op = status(t, e, "change"); op.State != "failed" || !strings.Contains(op.Error, "previous routes are still applied") {
		t.Fatalf("recovered route change reported %+v", op)
	}
	assertRoutes(t, e.Root, oldRoute)
	logs, _ := e.Logs("change")
	if !strings.Contains(logs, "Restored the previous routes") {
		t.Fatalf("log does not explain the recovery: %s", logs)
	}

	settle(t, e)
	if next := status(t, e, "next"); next.State != "succeeded" {
		t.Fatalf("queued route change did not run after recovery: %+v", next)
	}
	assertRoutes(t, e.Root, newRoute)
	if web.id != before.id || web.startedAt != before.startedAt || web.state != "exited" || len(docker.lifecycle) != 0 {
		t.Fatalf("route recovery touched the application: %+v %v", *web, docker.lifecycle)
	}
}

func TestRouteCancellationAroundCommit(t *testing.T) {
	t.Run("before commit restores previous routes", func(t *testing.T) {
		web := newContainer("0123456789ab", "app", "web", "running")
		e, docker := routeEngine(t, web)
		deployed(t, e, "app", []Route{oldRoute})
		cancelled := false
		docker.hook = func(command string) {
			// The cancellation arrives after the proxy accepted the new routes but before the commit.
			if !cancelled && strings.Contains(command, " wget ") && proxyServes(e.Root, newRoute.Domain) {
				cancelled = true
				if op, err := e.Cancel(routeRequest("change", newRoute)); err != nil || op.State != "running" {
					t.Errorf("cancel during apply: %+v %v", op, err)
				}
			}
		}
		op := runRoutes(t, e, "change", "app", []Route{newRoute})
		if !cancelled || op.State != "cancelled" || !strings.Contains(op.Error, "previous routes are still applied") {
			t.Fatalf("late cancellation reported %+v", op)
		}
		assertRoutes(t, e.Root, oldRoute)
		if len(docker.lifecycle) != 0 {
			t.Fatalf("cancellation touched containers: %v", docker.lifecycle)
		}
	})
	t.Run("after commit keeps the applied routes", func(t *testing.T) {
		e, docker := routeEngine(t, newContainer("0123456789ab", "app", "web", "running"))
		deployed(t, e, "app", []Route{oldRoute})
		if op := runRoutes(t, e, "change", "app", []Route{newRoute}); op.State != "succeeded" {
			t.Fatalf("route change failed: %+v", op)
		}
		commands := len(docker.commands)
		if op, err := e.Cancel(routeRequest("change", newRoute)); err != nil || op.State != "succeeded" {
			t.Fatalf("cancellation after commit reported %+v %v", op, err)
		}
		if op, err := e.Submit(routeRequest("change", newRoute)); err != nil || op.State != "succeeded" {
			t.Fatalf("redelivered submit reported %+v %v", op, err)
		}
		settle(t, e)
		if len(docker.commands) != commands {
			t.Fatalf("redelivery re-ran the route change: %v", docker.commands[commands:])
		}
		assertRoutes(t, e.Root, newRoute)
	})
}

// TestRouteWorkerRestartAtEachBoundary copies the worker's disk at each step of a route change, as
// a crash would leave it, and starts a new worker on the copy.
func TestRouteWorkerRestartAtEachBoundary(t *testing.T) {
	type boundary struct {
		name    string
		crashAt func(command string, root string) bool
		after   func(t *testing.T, root string)
		cancel  bool
		state   string
		want    Route
	}
	journaled := func(command, _ string) bool {
		return strings.HasPrefix(command, "docker ps") && strings.Contains(command, "service=web")
	}
	proxyUpdating := func(command, _ string) bool { return strings.Contains(command, "routes.next") }
	proxyAccepted := func(command, root string) bool {
		return strings.Contains(command, " wget ") && proxyServes(root, newRoute.Domain)
	}
	stateWritten := func(t *testing.T, root string) {
		path := filepath.Join(root, "workloads", "app", "current.json")
		var current deployedState
		data, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(data, &current)
		}
		if err != nil {
			t.Fatal(err)
		}
		current.Routes = []Route{newRoute}
		if err = writeJSON(path, current); err != nil {
			t.Fatal(err)
		}
	}
	editJournal := func(t *testing.T, root string, edit func(*journal)) {
		path := filepath.Join(root, "operations", "change.json")
		var j journal
		data, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(data, &j)
		}
		if err != nil {
			t.Fatal(err)
		}
		edit(&j)
		if err = writeJSON(path, j); err != nil {
			t.Fatal(err)
		}
	}
	committed := func(t *testing.T, root string) {
		stateWritten(t, root)
		editJournal(t, root, func(j *journal) { j.RouteChange.Committed = true })
	}
	for _, b := range []boundary{
		{name: "previous routes journaled", crashAt: journaled, state: "interrupted", want: oldRoute},
		{name: "previous routes journaled then cancelled", crashAt: journaled, cancel: true, state: "cancelled", want: oldRoute},
		{name: "proxy updating", crashAt: proxyUpdating, state: "interrupted", want: oldRoute},
		{name: "proxy accepted", crashAt: proxyAccepted, state: "interrupted", want: oldRoute},
		{name: "release state written", crashAt: proxyAccepted, after: stateWritten, state: "interrupted", want: oldRoute},
		{name: "committed", crashAt: proxyAccepted, after: committed, state: "succeeded", want: newRoute},
		{name: "committed then cancelled", crashAt: proxyAccepted, after: committed, cancel: true, state: "succeeded", want: newRoute},
	} {
		t.Run(b.name, func(t *testing.T) {
			web := newContainer("0123456789ab", "app", "web", "exited")
			e, docker := routeEngine(t, web)
			deployed(t, e, "app", []Route{oldRoute})
			disk := filepath.Join(t.TempDir(), "disk")
			docker.hook = func(command string) {
				if _, err := os.Stat(disk); os.IsNotExist(err) && b.crashAt(command, e.Root) {
					if err = os.CopyFS(disk, os.DirFS(e.Root)); err != nil {
						t.Error(err)
					}
				}
			}
			runRoutes(t, e, "change", "app", []Route{newRoute})
			if _, err := os.Stat(disk); err != nil {
				t.Fatal("crash point was not reached:", err)
			}
			if b.after != nil {
				b.after(t, disk)
			}
			if b.cancel {
				editJournal(t, disk, func(j *journal) { j.CancelRequested = true })
			}

			restarted, err := NewEngine(disk)
			if err != nil {
				t.Fatal(err)
			}
			before := *web
			fresh := &fakeDocker{t: t, root: disk, containers: []*fakeContainer{web}}
			restarted.Run = fresh.run
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- restarted.Serve(ctx) }()
			op := Operation{}
			for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				if op = status(t, restarted, "change"); op.State != "running" {
					break
				}
			}
			cancel()
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			if op.State != b.state {
				t.Fatalf("restarted worker reported %+v, want %s", op, b.state)
			}
			assertRoutes(t, disk, b.want)
			if web.id != before.id || web.startedAt != before.startedAt || web.state != "exited" || len(fresh.lifecycle) != 0 {
				t.Fatalf("recovery touched the application: %+v %v", *web, fresh.lifecycle)
			}
		})
	}
}
