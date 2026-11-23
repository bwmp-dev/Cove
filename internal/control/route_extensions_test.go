package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"strings"
	"testing"
)

func TestRouteExtensionsRequireProtocolAndPersistAfterWorkerConfirmation(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('pages-node','Node','localhost',22,'',$1,'ready','{"route_policies":true,"live_routes":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('pages-project','Project','default')`)
	initial := worker.Route{Domain: "pages.example.test", Service: "web", Port: 8080, Auth: "user:$2y$10$" + strings.Repeat("a", 53)}
	config := workloadInput{Name: "Pages", NodeID: "pages-node", ProjectID: "pages-project", Kind: "service", Compose: "services: {}", Routes: []worker.Route{initial}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	path := "/api/workloads/" + id
	next := initial
	next.Auth = ""
	next.Policy = &worker.RoutePolicy{Maintenance: &worker.RouteMaintenance{Enabled: true, HTML: "<h1>maintenance-marker</h1>"}, ErrorPages: map[string]string{"404": "<h1>not found</h1>"}, ForwardAuth: &worker.RouteForwardAuth{URL: "https://auth.example.test/check", ResponseHeaders: []string{"Remote-User"}}}
	input := map[string]any{"routes": []any{map[string]any{"domain": next.Domain, "service": next.Service, "port": next.Port, "policy": next.Policy}}}
	f.call("root", "default", "PUT", path+"/routes", input, 409)
	f.sql(`UPDATE nodes SET capabilities='{"route_policies":true,"live_routes":true,"route_protocol":2}' WHERE id='pages-node'`)
	f.call("root", "default", "PUT", path+"/routes", input, 200)
	var saved workloadInput
	if err := json.Unmarshal(f.call("root", "default", "GET", path, nil, 200).Body.Bytes(), &saved); err != nil || saved.Routes[0].Policy.Maintenance.HTML != "<h1>maintenance-marker</h1>" {
		t.Fatal(saved, err)
	}
	viewer := f.user("pages-viewer", "pages-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','pages-viewer','member','[{"scope":"project","scope_id":"pages-project","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", path, nil, 200)
	f.call(viewer, "default", "PUT", path+"/routes", input, 403)
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,state) VALUES('pages-initial',$1,'deploy',$2,'succeeded')`, id, []byte{})
	next.Policy.Maintenance.HTML = "<h1>updated-marker</h1>"
	op := jsonString(t, f.call("root", "default", "PUT", path+"/routes", input, 202), "id")
	var payload []byte
	if err := f.s.DB.QueryRow(ctx, `SELECT payload FROM operations WHERE id=$1`, op).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "pages-node" || r.Method != "submit_routes_v2" || r.Request == nil || r.Request.Routes[0].Auth != initial.Auth || r.Request.Routes[0].Policy.Maintenance.HTML != "<h1>updated-marker</h1>" {
			t.Fatal("route protocol or preserved auth incorrect", node, r)
		}
		return worker.RPCResponse{Operation: &worker.Operation{ID: op, Action: "routes", State: "succeeded"}}, nil
	}
	if err := f.s.processOperation(ctx, op, "pages-node", payload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(f.call("root", "default", "GET", path, nil, 200).Body.Bytes(), &saved); err != nil || saved.Routes[0].Policy.Maintenance.HTML != "<h1>updated-marker</h1>" {
		t.Fatal(saved, err)
	}
	f.sql(`UPDATE nodes SET capabilities='{"route_policies":true}' WHERE id='pages-node'`)
	if err := f.s.prepareInfrastructure(ctx, &worker.Request{Workload: id, Action: "deploy", Routes: []worker.Route{next}}); err != errRouteProtocolUnsupported {
		t.Fatal("old worker accepted protected route extension", err)
	}
}

func TestRouteExtensionsOmittedFieldsKeepProtection(t *testing.T) {
	previous := []worker.Route{{Domain: "app.example.test", Service: "app", Port: 80, Auth: "secret", Policy: &worker.RoutePolicy{ForwardAuth: &worker.RouteForwardAuth{URL: "https://auth.example.test/check"}, Maintenance: &worker.RouteMaintenance{Enabled: true}, ErrorPages: map[string]string{"404": "not found"}}}}
	for _, policy := range []string{"", `,"policy":{"path_prefix":"/new"}`} {
		var input []routeInput
		if err := json.Unmarshal([]byte(`[{"domain":"app.example.test","service":"app","port":80`+policy+`}]`), &input); err != nil {
			t.Fatal(err)
		}
		routes, err := retainedRouteAuth(input, previous)
		if err != nil || routes[0].Auth != "secret" || routes[0].Policy.ForwardAuth == nil || routes[0].Policy.Maintenance == nil || len(routes[0].Policy.ErrorPages) != 1 {
			t.Fatal(routes, err)
		}
	}
	var input []routeInput
	if err := json.Unmarshal([]byte(`[{"domain":"app.example.test","service":"app","port":80,"auth":"","policy":{"forward_auth":null,"maintenance":null,"error_pages":null}}]`), &input); err != nil {
		t.Fatal(err)
	}
	routes, err := retainedRouteAuth(input, previous)
	if err != nil || routes[0].Auth != "" || worker.HasRouteExtensions(routes) {
		t.Fatal(routes, err)
	}
	previous = append(previous, worker.Route{Domain: previous[0].Domain, Policy: &worker.RoutePolicy{PathPrefix: "/other"}})
	if err := json.Unmarshal([]byte(`[{"domain":"app.example.test","service":"app","port":80,"auth":"","policy":{"path_prefix":"/new"}}]`), &input); err != nil {
		t.Fatal(err)
	}
	if _, err := retainedRouteAuth(input, previous); err == nil {
		t.Fatal("ambiguous path silently lost protection")
	}
}
