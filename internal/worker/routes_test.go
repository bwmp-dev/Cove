package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

type fakeContainer struct {
	id, project, service, state, startedAt string
	aliases                                map[string][]string
}

// fakeDocker models the Docker state that route updates touch: application containers,
// their network attachments and the managed Traefik proxy.
type fakeDocker struct {
	t          *testing.T
	root       string
	mu         sync.Mutex
	containers []*fakeContainer
	lifecycle  []string
	failMove   int
	commands   []string
	// hook runs before each command, for fault injection at a precise point of a route change.
	hook func(command string)
}

func (d *fakeDocker) container(id string) *fakeContainer {
	for _, c := range d.containers {
		if c.id == id {
			return c
		}
	}
	return nil
}

func filterValue(args []string, prefix string) string {
	for i, arg := range args {
		if arg == "--filter" && i+1 < len(args) && strings.HasPrefix(args[i+1], prefix) {
			return strings.TrimPrefix(args[i+1], prefix)
		}
	}
	return ""
}

func (d *fakeDocker) run(_ context.Context, _ string, args, _ []string, out io.Writer) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	command := strings.Join(args, " ")
	d.commands = append(d.commands, command)
	if d.hook != nil {
		d.hook(command)
	}
	write := func(text string) error { _, err := io.WriteString(out, text); return err }
	switch {
	case len(args) > 2 && (args[1] == "compose" || args[1] == "restart" || args[1] == "stop" || args[1] == "rm" || args[1] == "kill" || (args[1] == "start" && args[2] != "cove-ingress")):
		// Any lifecycle command would recreate or restart application containers.
		d.lifecycle = append(d.lifecycle, command)
		for _, c := range d.containers {
			c.startedAt, c.state = "restarted", "running"
			if args[1] == "compose" {
				c.id += "-recreated"
			}
		}
		return nil
	case len(args) > 3 && (args[1] == "network" || args[1] == "volume") && args[2] == "inspect":
		return write("ingress")
	case args[1] == "ps" && filterValue(args, "name=") == "^/cove-ingress$":
		return write("ingressid")
	case args[1] == "ps":
		project, service := filterValue(args, "label=com.docker.compose.project="), filterValue(args, "label=com.docker.compose.service=")
		var ids []string
		for _, c := range d.containers {
			if c.project == project && c.service == service {
				ids = append(ids, c.id)
			}
		}
		return write(strings.Join(ids, "\n"))
	case args[1] == "inspect" && args[len(args)-1] == "cove-ingress":
		switch {
		case strings.Contains(command, "cove.managed"):
			return write("ingress")
		case strings.Contains(command, ".Config.Image"):
			return write(ingressImage)
		default:
			return write("<no value>")
		}
	case args[1] == "inspect":
		c := d.container(args[len(args)-1])
		if c == nil {
			return errors.New("no such container")
		}
		if aliases, ok := c.aliases["cove-ingress"]; ok {
			return write(strings.TrimSpace("connected " + strings.Join(aliases, " ")))
		}
		return nil
	case len(args) == 7 && args[1] == "network" && args[2] == "connect" && args[5] == "cove-ingress":
		c := d.container(args[6])
		if c == nil {
			return errors.New("no such container")
		}
		c.aliases["cove-ingress"] = []string{args[4]}
		return nil
	case args[1] == "cp" || (args[1] == "start" && args[2] == "cove-ingress"):
		return nil
	case args[1] == "exec" && args[3] == "sh":
		if d.failMove > 0 {
			d.failMove--
			return errors.New("proxy configuration could not be replaced")
		}
		return nil
	case args[1] == "exec" && args[3] == "wget":
		data, err := os.ReadFile(filepath.Join(d.root, "ingress.traefik.yaml"))
		if err != nil {
			return err
		}
		var config dynamicIngress
		if err = json.Unmarshal(data, &config); err != nil {
			return err
		}
		routers := []map[string]any{}
		for name, router := range config.HTTP.Routers {
			routers = append(routers, map[string]any{"name": name + "@file", "status": "enabled", "rule": router.Rule, "service": router.Service + "@file", "middlewares": router.Middlewares, "entryPoints": router.EntryPoints})
		}
		return json.NewEncoder(out).Encode(routers)
	}
	d.t.Fatalf("unexpected command: %s", command)
	return nil
}

func routeEngine(t *testing.T, containers ...*fakeContainer) (*Engine, *fakeDocker) {
	t.Helper()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	docker := &fakeDocker{t: t, root: e.Root, containers: containers}
	e.Run = docker.run
	return e, docker
}

func deployed(t *testing.T, e *Engine, workload string, routes []Route) {
	t.Helper()
	dir := filepath.Join(e.Root, "workloads", workload)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "current.json"), deployedState{ComposePath: filepath.Join(dir, "release.yaml"), Routes: routes}); err != nil {
		t.Fatal(err)
	}
	if len(routes) > 0 {
		owned, err := e.loadIngress()
		if err != nil {
			t.Fatal(err)
		}
		owned[workload] = routes
		if err = writeJSON(filepath.Join(e.Root, "ingress.json"), owned); err != nil {
			t.Fatal(err)
		}
	}
}

func runRoutes(t *testing.T, e *Engine, id, workload string, routes []Route) Operation {
	t.Helper()
	if _, err := e.Submit(Request{ID: id, Workload: workload, Action: "routes", Kind: "service", Routes: routes}); err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	if err := e.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	op, err := e.Status(id)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func newContainer(id, workload, service, state string) *fakeContainer {
	return &fakeContainer{id: id, project: "cove-" + workload, service: service, state: state, startedAt: "2026-11-18T08:00:00.000000001Z", aliases: map[string][]string{"cove-app": {service}}}
}

func TestRouteUpdateKeepsRunningContainerIdentity(t *testing.T) {
	web := newContainer("0123456789ab", "app", "web", "running")
	before := *web
	e, docker := routeEngine(t, web)
	deployed(t, e, "app", nil)
	route := Route{Domain: "app.example.com", Service: "web", Port: 8080}
	op := runRoutes(t, e, "add-route", "app", []Route{route})
	if op.State != "succeeded" {
		logs, _ := e.Logs(op.ID)
		t.Fatalf("route update failed: %+v\n%s", op, logs)
	}
	if web.id != before.id || web.startedAt != before.startedAt || web.state != "running" {
		t.Fatalf("container changed: before %+v after %+v", before, *web)
	}
	if len(docker.lifecycle) != 0 {
		t.Fatalf("route update ran lifecycle commands: %v", docker.lifecycle)
	}
	if !slices.Equal(web.aliases["cove-ingress"], []string{routeAlias("app", "web")}) || !slices.Equal(web.aliases["cove-app"], []string{"web"}) {
		t.Fatalf("ingress attachment incorrect: %v", web.aliases)
	}
	owned, err := e.loadIngress()
	if err != nil || !reflect.DeepEqual(owned["app"], []Route{route}) {
		t.Fatalf("desired routes not persisted: %v %v", owned, err)
	}
	var current deployedState
	data, _ := os.ReadFile(filepath.Join(e.Root, "workloads", "app", "current.json"))
	if json.Unmarshal(data, &current) != nil || !reflect.DeepEqual(current.Routes, []Route{route}) {
		t.Fatalf("current release routes not updated: %+v", current)
	}
	logs, _ := e.Logs(op.ID)
	if !strings.Contains(logs, "were not restarted or started") {
		t.Fatalf("missing apply confirmation: %s", logs)
	}

	connects := 0
	for _, command := range docker.commands {
		if strings.Contains(command, "network connect") {
			connects++
		}
	}
	if op = runRoutes(t, e, "remove-route", "app", nil); op.State != "succeeded" {
		t.Fatalf("route removal failed: %+v", op)
	}
	if owned, _ = e.loadIngress(); len(owned) != 0 || len(docker.lifecycle) != 0 || web.startedAt != before.startedAt {
		t.Fatalf("route removal changed more than the proxy: %v %v", owned, docker.lifecycle)
	}
	if op = runRoutes(t, e, "restore-route", "app", []Route{route}); op.State != "succeeded" {
		t.Fatalf("route restore failed: %+v", op)
	}
	reconnects := 0
	for _, command := range docker.commands {
		if strings.Contains(command, "network connect") {
			reconnects++
		}
	}
	if reconnects != connects {
		t.Fatal("an already attached container was attached again")
	}
}

func TestRouteUpdateLeavesStoppedResourceStopped(t *testing.T) {
	web := newContainer("abcdef012345", "app", "web", "exited")
	e, docker := routeEngine(t, web)
	deployed(t, e, "app", nil)
	op := runRoutes(t, e, "stopped-route", "app", []Route{{Domain: "stopped.example.com", Service: "web", Port: 80}})
	if op.State != "succeeded" {
		t.Fatalf("route update on stopped resource failed: %+v", op)
	}
	if web.state != "exited" || len(docker.lifecycle) != 0 {
		t.Fatalf("stopped resource was started: state=%s commands=%v", web.state, docker.lifecycle)
	}
	if !slices.Equal(web.aliases["cove-ingress"], []string{routeAlias("app", "web")}) {
		t.Fatal("stopped container was not prepared for routing when started")
	}
}

func TestRouteUpdateFailuresRestorePreviousProxy(t *testing.T) {
	existing := Route{Domain: "old.example.com", Service: "web", Port: 80}
	t.Run("not deployed", func(t *testing.T) {
		e, docker := routeEngine(t)
		op := runRoutes(t, e, "undeployed", "app", []Route{existing})
		logs, _ := e.Logs(op.ID)
		// Nothing serves the resource yet, so the routes are only recorded for its first deploy.
		if op.State != "succeeded" || !strings.Contains(logs, "first deploy") || len(docker.commands) != 0 {
			t.Fatalf("undeployed resource touched Docker: %+v %v %s", op, docker.commands, logs)
		}
	})
	t.Run("missing service", func(t *testing.T) {
		e, docker := routeEngine(t, newContainer("0123456789ab", "app", "web", "running"))
		deployed(t, e, "app", []Route{existing})
		op := runRoutes(t, e, "missing", "app", []Route{{Domain: "new.example.com", Service: "api", Port: 80}})
		logs, _ := e.Logs(op.ID)
		owned, _ := e.loadIngress()
		if op.State != "failed" || !strings.Contains(logs, `route service "api" has no container`) || !reflect.DeepEqual(owned["app"], []Route{existing}) || len(docker.lifecycle) != 0 {
			t.Fatalf("missing service accepted or proxy changed: %+v %v %s", op, owned, logs)
		}
	})
	t.Run("hostname owned by another resource", func(t *testing.T) {
		e, _ := routeEngine(t, newContainer("0123456789ab", "app", "web", "running"))
		deployed(t, e, "other", []Route{{Domain: "taken.example.com", Service: "site", Port: 80}})
		deployed(t, e, "app", nil)
		op := runRoutes(t, e, "conflict", "app", []Route{{Domain: "taken.example.com", Service: "web", Port: 80}})
		owned, _ := e.loadIngress()
		if op.State != "failed" || len(owned) != 1 || owned["app"] != nil {
			t.Fatalf("hostname takeover accepted: %+v %v", op, owned)
		}
	})
	t.Run("proxy rejects update", func(t *testing.T) {
		e, docker := routeEngine(t, newContainer("0123456789ab", "app", "web", "running"))
		deployed(t, e, "app", []Route{existing})
		docker.failMove = 1
		op := runRoutes(t, e, "rejected", "app", []Route{{Domain: "new.example.com", Service: "web", Port: 80}})
		owned, _ := e.loadIngress()
		if op.State != "failed" || !reflect.DeepEqual(owned["app"], []Route{existing}) {
			t.Fatalf("failed apply did not restore previous routes: %+v %v", op, owned)
		}
		data, _ := os.ReadFile(filepath.Join(e.Root, "ingress.traefik.yaml"))
		if !strings.Contains(string(data), existing.Domain) || strings.Contains(string(data), "new.example.com") {
			t.Fatalf("proxy configuration not restored: %s", data)
		}
	})
	t.Run("foreign ingress attachment", func(t *testing.T) {
		web := newContainer("0123456789ab", "app", "web", "running")
		web.aliases["cove-ingress"] = []string{"web"}
		e, _ := routeEngine(t, web)
		deployed(t, e, "app", nil)
		op := runRoutes(t, e, "foreign", "app", []Route{{Domain: "new.example.com", Service: "web", Port: 80}})
		logs, _ := e.Logs(op.ID)
		if op.State != "failed" || !strings.Contains(logs, "without Cove's routing alias") {
			t.Fatalf("unroutable attachment accepted: %+v %s", op, logs)
		}
	})
}

func TestRouteRequestsRejectJobs(t *testing.T) {
	if err := ValidateRequest(Request{ID: "op", Workload: "app", Action: "routes", Kind: "job", JobService: "migrate"}); err == nil {
		t.Fatal("job route update accepted")
	}
	if err := ValidateRequest(Request{ID: "op", Workload: "app", Action: "routes", Routes: []Route{{Domain: "Bad.example.com", Service: "web", Port: 80}}}); err == nil {
		t.Fatal("unsafe route accepted")
	}
}
