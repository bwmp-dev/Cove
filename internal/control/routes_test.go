package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// routeWorker serves a real worker engine on the local worker socket. Docker is replaced by a
// fixture that records every command and answers the managed-proxy queries a route update makes.
type routeWorker struct {
	t        *testing.T
	root     string
	socket   string
	mu       sync.Mutex
	engine   *worker.Engine
	stop     func()
	commands []string
	// failMoves makes the proxy refuse that many configuration replacements; proxyDown refuses all.
	failMoves int
	proxyDown bool
	// tamper replaces the worker's reply after the engine handled the request, as a lost or garbled reply would.
	tamper func(worker.RPCRequest, worker.RPCResponse) worker.RPCResponse
}

func (w *routeWorker) run(_ context.Context, _ string, args, _ []string, out io.Writer) error {
	command := strings.Join(args, " ")
	w.mu.Lock()
	w.commands = append(w.commands, command)
	refuse := w.proxyDown || w.failMoves > 0
	if strings.HasPrefix(command, "docker exec cove-ingress sh") && w.failMoves > 0 {
		w.failMoves--
	}
	w.mu.Unlock()
	write := func(text string) error { _, err := io.WriteString(out, text); return err }
	switch {
	case strings.HasPrefix(command, "docker exec cove-ingress sh") && refuse:
		return errors.New("proxy configuration could not be replaced")
	case strings.Contains(command, "inspect") && strings.HasSuffix(command, " cove-ingress") && strings.Contains(command, "cove.panel.domain"):
		return write("<no value>")
	case strings.Contains(command, "Config.Image"):
		return write("traefik:v3.7.19")
	case strings.Contains(command, "inspect") && strings.HasSuffix(command, " cove-ingress"):
		return write("ingress")
	case strings.HasPrefix(command, "docker ps") && strings.Contains(command, "name=^/cove-ingress$"):
		return write("ingressid")
	case strings.HasPrefix(command, "docker ps"):
		return write("0123456789ab")
	case strings.HasPrefix(command, "docker exec cove-ingress wget"):
		data, err := os.ReadFile(filepath.Join(w.root, "ingress.traefik.yaml"))
		if err != nil {
			return err
		}
		var config struct {
			HTTP struct {
				Routers map[string]struct{ Rule, Service string } `json:"routers"`
			} `json:"http"`
		}
		if err = json.Unmarshal(data, &config); err != nil {
			return err
		}
		routers := []map[string]string{}
		for name, router := range config.HTTP.Routers {
			routers = append(routers, map[string]string{"name": name + "@file", "status": "enabled", "rule": router.Rule, "service": router.Service})
		}
		return json.NewEncoder(out).Encode(routers)
	}
	return nil
}

func (w *routeWorker) lifecycle() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var found []string
	for _, command := range w.commands {
		if strings.HasPrefix(command, "docker compose") || strings.HasPrefix(command, "docker restart") || strings.HasPrefix(command, "docker stop") || (strings.HasPrefix(command, "docker start") && command != "docker start cove-ingress") {
			found = append(found, command)
		}
	}
	return found
}

// proxyServes reports whether the proxy's active configuration routes domain.
func (w *routeWorker) proxyServes(domain string) bool {
	data, _ := os.ReadFile(filepath.Join(w.root, "ingress.traefik.yaml"))
	return strings.Contains(string(data), domain)
}

func (w *routeWorker) status(id string) worker.Operation {
	w.t.Helper()
	w.mu.Lock()
	engine := w.engine
	w.mu.Unlock()
	op, err := engine.Status(id)
	if err != nil {
		w.t.Fatal(err)
	}
	return op
}

func startRouteWorker(t *testing.T) (*routeWorker, string) {
	t.Helper()
	w := &routeWorker{t: t, root: t.TempDir(), socket: filepath.Join(t.TempDir(), "worker.sock")}
	w.start()
	t.Cleanup(func() { w.stop() })
	return w, w.socket
}

// start runs a worker on the fixture's disk, as the worker service does after a restart.
func (w *routeWorker) start() {
	w.t.Helper()
	engine, err := worker.NewEngine(w.root)
	if err != nil {
		w.t.Fatal(err)
	}
	engine.Run = w.run
	listener, err := net.Listen("unix", w.socket)
	if err != nil {
		w.t.Skip("Unix sockets unavailable:", err)
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var request worker.RPCRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(rw, "invalid request", 400)
			return
		}
		response := engine.RPC(r.Context(), request)
		w.mu.Lock()
		tamper := w.tamper
		w.mu.Unlock()
		if tamper != nil {
			response = tamper(request, response)
		}
		_ = json.NewEncoder(rw).Encode(response)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = engine.Serve(ctx); close(done) }()
	go func() { _ = server.Serve(listener) }()
	w.mu.Lock()
	w.engine = engine
	w.mu.Unlock()
	w.stop = func() { cancel(); server.Close(); <-done }
}

func (w *routeWorker) restart() {
	w.stop()
	w.start()
}

func (f *accountFixture) routeConfig(id string) workloadInput {
	f.t.Helper()
	var encrypted []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT configuration FROM workloads WHERE id=$1", id).Scan(&encrypted); err != nil {
		f.t.Fatal(err)
	}
	data, err := unseal(f.s.Key, encrypted)
	if err != nil {
		f.t.Fatal(err)
	}
	var config workloadInput
	if err = json.Unmarshal(data, &config); err != nil {
		f.t.Fatal(err)
	}
	return config
}

func (f *accountFixture) settle(operation string) string {
	f.t.Helper()
	state, _ := f.await(operation, func(state, _ string) bool { return state != "queued" && state != "running" })
	return state
}

// await dispatches the operation like the control plane's dispatcher until done accepts its state and result.
func (f *accountFixture) await(operation string, done func(state, result string) bool) (string, string) {
	f.t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var node, state string
		var payload, result []byte
		if err := f.s.DB.QueryRow(ctx, "SELECT w.node_id,o.state,o.payload,o.result FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE o.id=$1", operation).Scan(&node, &state, &payload, &result); err != nil {
			f.t.Fatal(err)
		}
		if done(state, string(result)) {
			return state, string(result)
		}
		if err := f.s.processOperation(ctx, operation, node, payload); err != nil {
			f.t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	f.t.Fatal("route operation did not reach the expected state")
	return "", ""
}

func TestRouteUpdatesIntegration(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("alice", "alice@example.com")
	viewer := f.user("viewer", "viewer@example.com")
	operator := f.user("operator", "operator@example.com")
	rival := f.user("bob", "bob@example.com")
	f.sql("INSERT INTO teams(id,name) VALUES('a','Team A'),('b','Team B')")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('a','alice','owner','[]'),('b','bob','owner','[]'),
		('a','viewer','member','[{"scope":"project","scope_id":"pa","role":"viewer"}]'),
		('a','operator','member','[{"scope":"project","scope_id":"pa","role":"operator"}]')`)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,mode,capabilities) VALUES
		('local','Local','local',22,'',$1,'ready','local','{"live_routes":true}'),('old','Old','old',22,'',$1,'ready','ssh','{}')`, []byte{})
	f.sql("INSERT INTO team_nodes(team_id,node_id) VALUES('a','local'),('a','old'),('b','local')")
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('pa','Project','a'),('pb','Other','b')")
	compose := "services:\n  web:\n    image: busybox:1.37\n"
	seed := func(id, node, team, project, kind string, routes []worker.Route) {
		input := workloadInput{Name: id, NodeID: node, Compose: compose, Kind: kind, Routes: routes, Environment: map[string]string{"SECRET": "route-test-secret"}}
		if kind == "job" {
			input.JobService = "web"
		}
		data, _ := json.Marshal(input)
		config, err := seal(f.s.Key, data)
		if err != nil {
			t.Fatal(err)
		}
		f.sql("INSERT INTO workloads(id,node_id,name,configuration,kind,team_id,project_id) VALUES($1,$2,$1,$3,$4,$5,$6)", id, node, config, kind, team, project)
	}
	existing := []worker.Route{{Domain: "live.example.com", Service: "web", Port: 80}}
	seed("pending", "local", "a", "pa", "service", nil)
	seed("live", "local", "a", "pa", "service", existing)
	seed("old", "old", "a", "pa", "service", nil)
	seed("job", "local", "a", "pa", "job", nil)
	seed("foreign", "local", "b", "pb", "service", []worker.Route{{Domain: "taken.example.com", Service: "web", Port: 80}})
	seed("rival", "local", "b", "pb", "service", nil)
	seed("attempted", "local", "a", "pa", "service", nil)
	seed("old-failed", "old", "a", "pa", "service", nil)
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES('deployed-live','live','deploy','succeeded',$1),('deployed-old','old','deploy','succeeded',$1),
		('cancelled-deploy','attempted','deploy','cancelled',$1),('failed-deploy','old-failed','deploy','failed',$1)`, []byte{})
	f.sql("UPDATE workloads SET manually_stopped=true WHERE id='live'")
	routes := func(list ...worker.Route) map[string]any { return map[string]any{"routes": list} }
	route := func(domain string) worker.Route { return worker.Route{Domain: domain, Service: "web", Port: 80} }

	// Changing routes is configuration and needs editor access.
	f.call(viewer, "a", "PUT", "/api/workloads/pending/routes", routes(route("app.example.com")), 403)
	f.call(operator, "a", "PUT", "/api/workloads/pending/routes", routes(route("app.example.com")), 403)
	issued := f.call(owner, "a", "POST", "/api/tokens", map[string]any{"name": "operator token", "workload_id": "live", "permission": "operate", "days": 1}, 201)
	token := jsonString(t, issued, "token")
	request := httptest.NewRequest("PUT", "/api/workloads/live/routes", strings.NewReader(`{"routes":[]}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatalf("operate token changed routes: %d %s", response.Code, response.Body.String())
	}

	// Undeployed services store desired routes for the first deploy.
	if state := jsonString(t, f.call(owner, "a", "PUT", "/api/workloads/pending/routes", routes(route("app.example.com")), 200), "state"); state != "saved" {
		t.Fatal("undeployed routes not saved:", state)
	}
	if got := f.routeConfig("pending").Routes; len(got) != 1 || got[0].Domain != "app.example.com" || f.routeConfig("pending").Environment["SECRET"] != "route-test-secret" {
		t.Fatalf("saved routes or secrets wrong: %+v", got)
	}
	if state := jsonString(t, f.call(owner, "a", "PUT", "/api/workloads/pending/routes", routes(route("app.example.com")), 200), "state"); state != "unchanged" {
		t.Fatal("unchanged routes queued work:", state)
	}
	detail := f.call(owner, "a", "GET", "/api/workloads/pending", nil, 200).Body.String()
	if !strings.Contains(detail, `"deployed":false`) || !strings.Contains(detail, "app.example.com") || strings.Contains(detail, "route-test-secret") {
		t.Fatal("detail missing route state or leaked secret:", detail)
	}

	// Hostnames are unique across teams and cannot take over the dashboard.
	for _, domain := range []string{"taken.example.com", "cove.example.com", "live.example.com"} {
		body := f.call(owner, "a", "PUT", "/api/workloads/pending/routes", routes(route(domain)), 409).Body.String()
		if strings.Contains(body, "foreign") || strings.Contains(body, "Other") {
			t.Fatal("conflict leaked another resource:", body)
		}
	}
	f.call(owner, "a", "POST", "/api/workloads", map[string]any{"name": "copy", "node_id": "local", "project_id": "pa", "compose": compose, "routes": []worker.Route{route("taken.example.com")}}, 409)
	f.call(owner, "a", "PUT", "/api/workloads/pending/routes", map[string]any{"routes": []map[string]any{{"domain": "Upper.example.com", "service": "web", "port": 80}}}, 400)
	f.call(owner, "a", "PUT", "/api/workloads/job/routes", routes(route("job.example.com")), 400)
	f.call(owner, "a", "PUT", "/api/workloads/old/routes", routes(route("old.example.com")), 409)

	// Full configuration saves keep routes when omitted and cannot change live routes.
	full := map[string]any{"name": "live", "node_id": "local", "kind": "service", "compose": compose, "source": nil}
	f.call(owner, "a", "PUT", "/api/workloads/live", full, 200)
	if got := f.routeConfig("live").Routes; len(got) != 1 || got[0] != existing[0] {
		t.Fatal("configuration save dropped routes:", got)
	}
	full["routes"] = []worker.Route{}
	f.call(owner, "a", "PUT", "/api/workloads/live", full, 409)

	// A deploy that did not succeed may still have applied routes, so only the worker may change them.
	f.call(owner, "a", "PUT", "/api/workloads/old-failed/routes", routes(route("old-failed.example.com")), 409)
	f.call(owner, "a", "PUT", "/api/workloads/old-failed", map[string]any{"name": "old-failed", "node_id": "old", "kind": "service", "compose": compose, "routes": []worker.Route{route("old-failed.example.com")}}, 409)
	if got := f.routeConfig("old-failed").Routes; len(got) != 0 {
		t.Fatal("routes saved past a failed deploy:", got)
	}

	runner, socket := startRouteWorker(t)
	f.s.LocalSocket = socket
	attempted := jsonString(t, f.call(owner, "a", "PUT", "/api/workloads/attempted/routes", routes(route("attempted.example.com")), 202), "id")
	if state := f.settle(attempted); state != "succeeded" {
		t.Fatal("route update after a cancelled first deploy failed:", state)
	}
	if got := f.routeConfig("attempted").Routes; len(got) != 1 || got[0].Domain != "attempted.example.com" || runner.proxyServes("attempted.example.com") {
		t.Fatal("routes for a resource without a release were applied to the proxy or not saved:", got)
	}

	dir := filepath.Join(runner.root, "workloads", "live")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "current.json"), []byte(`{"compose_path":"release.yaml","environment":{},"routes":[{"domain":"live.example.com","service":"web","port":80}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runner.root, "ingress.json"), []byte(`{"live":[{"domain":"live.example.com","service":"web","port":80}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	setProxy := func(failMoves int, down bool) {
		runner.mu.Lock()
		runner.failMoves, runner.proxyDown = failMoves, down
		runner.mu.Unlock()
	}

	// A proxy that rejects the change leaves the saved and served routes and the stop intent unchanged.
	setProxy(1, false)
	queued := f.call(owner, "a", "PUT", "/api/workloads/live/routes", routes(route("new.example.com")), 202)
	failed := jsonString(t, queued, "id")
	f.call(owner, "a", "PUT", "/api/workloads/live/routes", routes(route("other.example.com")), 409)
	f.call(owner, "a", "PUT", "/api/workloads/pending/routes", routes(route("new.example.com")), 409)
	if got := f.routeConfig("live").Routes; len(got) != 1 || got[0] != existing[0] {
		t.Fatal("routes saved before the worker applied them:", got)
	}
	if state := f.settle(failed); state != "failed" {
		t.Fatal("rejected route update did not fail:", state)
	}
	if got := f.routeConfig("live").Routes; len(got) != 1 || got[0] != existing[0] || !runner.proxyServes("live.example.com") || runner.proxyServes("new.example.com") {
		t.Fatal("failed route update changed saved or served routes:", got)
	}

	// While the worker cannot confirm what the proxy serves, the change stays running and keeps its hostname.
	setProxy(0, true)
	unsettled := jsonString(t, f.call(owner, "a", "PUT", "/api/workloads/live/routes", routes(route("new.example.com")), 202), "id")
	f.await(unsettled, func(state, result string) bool {
		return state == "running" && strings.Contains(result, "Restoring the previous routes")
	})
	f.call(rival, "b", "PUT", "/api/workloads/rival/routes", routes(route("new.example.com")), 409)
	f.call(rival, "b", "PUT", "/api/workloads/rival/routes", routes(route("live.example.com")), 409)
	f.call(owner, "a", "POST", "/api/workloads/live/operations", map[string]string{"action": "deploy"}, 409)

	// Once the proxy works again, a worker restart settles the change without a redeploy.
	setProxy(0, false)
	runner.restart()
	state, result := f.await(unsettled, func(state, _ string) bool { return state != "queued" && state != "running" })
	if state != "interrupted" || !strings.Contains(result, "previous routes are still applied") {
		t.Fatal("unsettled route change did not recover:", state, result)
	}
	if got := f.routeConfig("live").Routes; len(got) != 1 || got[0] != existing[0] || !runner.proxyServes("live.example.com") || runner.proxyServes("new.example.com") {
		t.Fatal("recovered route change left the new routes:", got)
	}
	if state := jsonString(t, f.call(rival, "b", "PUT", "/api/workloads/rival/routes", routes(route("new.example.com")), 200), "state"); state != "saved" {
		t.Fatal("hostname not released after the previous routes were restored:", state)
	}

	// A committed change survives a lost acknowledgement and a cancellation requested afterwards.
	applied := jsonString(t, f.call(owner, "a", "PUT", "/api/workloads/live/routes", routes(route("fresh.example.com")), 202), "id")
	var payload []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE id=$1", applied).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if err := f.s.processOperation(context.Background(), applied, "local", payload); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); runner.status(applied).State != "succeeded"; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("worker did not apply the routes:", runner.status(applied))
		}
	}
	f.call(rival, "b", "PUT", "/api/workloads/rival/routes", routes(route("fresh.example.com")), 409)
	f.call(owner, "a", "POST", "/api/operations/"+applied+"/cancel", nil, 202)
	if state := f.settle(applied); state != "succeeded" {
		t.Fatal("cancellation after the commit reported:", state)
	}
	if err := f.s.processOperation(context.Background(), applied, "local", payload); err != nil {
		t.Fatal(err)
	}
	if state := f.settle(applied); state != "succeeded" {
		t.Fatal("replayed acknowledgement changed the result:", state)
	}
	if got := f.routeConfig("live").Routes; len(got) != 1 || got[0].Domain != "fresh.example.com" || !runner.proxyServes("fresh.example.com") || runner.proxyServes("live.example.com") {
		t.Fatal("applied routes not saved or served:", got)
	}
	f.call(rival, "b", "PUT", "/api/workloads/rival/routes", routes(route("fresh.example.com")), 409)
	if commands := runner.lifecycle(); len(commands) != 0 {
		t.Fatal("route update restarted or started containers:", commands)
	}
	var stopped bool
	if err := f.s.DB.QueryRow(context.Background(), "SELECT manually_stopped FROM workloads WHERE id='live'").Scan(&stopped); err != nil || !stopped {
		t.Fatal("route update cleared manual stop intent", err)
	}
	deploy := jsonString(t, f.call(owner, "a", "POST", "/api/workloads/live/operations", map[string]string{"action": "deploy"}, 202), "id")
	if err := f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE id=$1", deploy).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	plain, err := unseal(f.s.Key, payload)
	if err != nil || !strings.Contains(string(plain), "fresh.example.com") {
		t.Fatal("redeploy lost applied routes")
	}

	// A push received before a route change deploys the routes saved since, not the hostnames it carried.
	seed("hooked", "local", "a", "pa", "service", []worker.Route{route("hooked.example.com")})
	f.sql("UPDATE workloads SET webhook_secret=$1 WHERE id='hooked'", []byte("secret"))
	stale, _ := json.Marshal(worker.Request{ID: "pushed", Workload: "hooked", Action: "deploy", Compose: compose, Kind: "service", Routes: []worker.Route{route("live.example.com")}})
	sealed, err := seal(f.s.Key, stale)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("INSERT INTO webhook_deliveries(workload_id,delivery,payload) VALUES('hooked','push-1',$1)", sealed)
	if err = f.s.dispatchWebhook(context.Background(), "hooked"); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE id='pushed'").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var pushed worker.Request
	if plain, err = unseal(f.s.Key, payload); err == nil {
		err = json.Unmarshal(plain, &pushed)
	}
	if err != nil || len(pushed.Routes) != 1 || pushed.Routes[0].Domain != "hooked.example.com" {
		t.Fatal("webhook deploy replayed stale routes:", pushed.Routes, err)
	}
}

// A route reply the controller cannot interpret says nothing about what the worker applied: its
// journal may already exist when submit reports an error, so hostnames stay claimed until a retry
// gets a definite answer.
func TestAmbiguousRouteRepliesKeepHostnameClaims(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("alice", "alice@example.com")
	rival := f.user("bob", "bob@example.com")
	f.sql("INSERT INTO teams(id,name) VALUES('a','Team A'),('b','Team B')")
	f.sql("INSERT INTO team_members(team_id,user_id,role,grants) VALUES('a','alice','owner','[]'),('b','bob','owner','[]')")
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,mode,capabilities) VALUES('local','Local','local',22,'',$1,'ready','local','{"live_routes":true}')`, []byte{})
	f.sql("INSERT INTO team_nodes(team_id,node_id) VALUES('a','local'),('b','local')")
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('pa','Project','a'),('pb','Other','b')")
	route := func(domain string) worker.Route { return worker.Route{Domain: domain, Service: "web", Port: 80} }
	routes := func(list ...worker.Route) map[string]any { return map[string]any{"routes": list} }
	existing := route("live.example.com")
	for _, w := range []struct {
		id, team, project string
		routes            []worker.Route
	}{{"live", "a", "pa", []worker.Route{existing}}, {"rival", "b", "pb", nil}} {
		data, _ := json.Marshal(workloadInput{Name: w.id, NodeID: "local", Compose: "services:\n  web:\n    image: busybox:1.37\n", Kind: "service", Routes: w.routes})
		config, err := seal(f.s.Key, data)
		if err != nil {
			t.Fatal(err)
		}
		f.sql("INSERT INTO workloads(id,node_id,name,configuration,kind,team_id,project_id) VALUES($1,'local',$1,$2,'service',$3,$4)", w.id, config, w.team, w.project)
	}
	f.sql("INSERT INTO operations(id,workload_id,action,state,payload) VALUES('deployed-live','live','deploy','succeeded',$1)", []byte{})

	runner, socket := startRouteWorker(t)
	f.s.LocalSocket = socket
	dir := filepath.Join(runner.root, "workloads", "live")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "current.json"), []byte(`{"compose_path":"release.yaml","environment":{},"routes":[{"domain":"live.example.com","service":"web","port":80}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runner.root, "ingress.json"), []byte(`{"live":[{"domain":"live.example.com","service":"web","port":80}]}`), 0600); err != nil {
		t.Fatal(err)
	}

	id := jsonString(t, f.call(owner, "a", "PUT", "/api/workloads/live/routes", routes(route("new.example.com")), 202), "id")
	var payload []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE id=$1", id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	replies := []struct {
		name  string
		reply func(worker.RPCResponse) worker.RPCResponse
	}{
		{"submit error after the journal was written", func(worker.RPCResponse) worker.RPCResponse {
			return worker.RPCResponse{Error: "could not sync operation directory"}
		}},
		{"reply without an operation", func(worker.RPCResponse) worker.RPCResponse { return worker.RPCResponse{} }},
		{"unknown operation state", func(response worker.RPCResponse) worker.RPCResponse {
			if response.Operation != nil {
				op := *response.Operation
				op.State = "reconciling"
				response.Operation = &op
			}
			return response
		}},
	}
	for _, c := range replies {
		runner.mu.Lock()
		runner.tamper = func(request worker.RPCRequest, response worker.RPCResponse) worker.RPCResponse {
			if request.Method == "submit" {
				return c.reply(response)
			}
			return response
		}
		runner.mu.Unlock()
		if err := f.s.processOperation(context.Background(), id, "local", payload); err != nil {
			t.Fatal(c.name, err)
		}
		// Fails the test when the worker kept no journal for the change.
		runner.status(id)
		var state, result string
		if err := f.s.DB.QueryRow(context.Background(), "SELECT state,result::text FROM operations WHERE id=$1", id).Scan(&state, &result); err != nil {
			t.Fatal(err)
		}
		if state != "queued" || !strings.Contains(result, "hostnames remain reserved") {
			t.Fatalf("%s: route operation became %s %s", c.name, state, result)
		}
		f.call(rival, "b", "PUT", "/api/workloads/rival/routes", routes(route("new.example.com")), 409)
		f.call(rival, "b", "PUT", "/api/workloads/rival/routes", routes(existing), 409)
		if got := f.routeConfig("live").Routes; len(got) != 1 || got[0] != existing {
			t.Fatalf("%s: routes saved without a confirmed outcome: %+v", c.name, got)
		}
	}

	// The next definite reply settles the same operation without resubmitting different input.
	runner.mu.Lock()
	runner.tamper = nil
	runner.mu.Unlock()
	if state := f.settle(id); state != "succeeded" {
		t.Fatal("route change did not converge after ambiguous replies:", state)
	}
	if got := f.routeConfig("live").Routes; len(got) != 1 || got[0].Domain != "new.example.com" || !runner.proxyServes("new.example.com") || runner.proxyServes("live.example.com") {
		t.Fatalf("converged route change not saved or served: %+v", got)
	}
	f.call(rival, "b", "PUT", "/api/workloads/rival/routes", routes(route("new.example.com")), 409)
	if state := jsonString(t, f.call(rival, "b", "PUT", "/api/workloads/rival/routes", routes(existing), 200), "state"); state != "saved" {
		t.Fatal("replaced hostname not released:", state)
	}
	if commands := runner.lifecycle(); len(commands) != 0 {
		t.Fatal("route change restarted or started containers:", commands)
	}
}
