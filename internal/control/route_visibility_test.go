package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"strings"
	"testing"
)

func TestPrivateRouteSavesPreserveProtectionAndRequireExplicitRemoval(t *testing.T) {
	old := worker.Route{Domain: "office.example.test", Service: "web", Port: 8080, Policy: &worker.RoutePolicy{Visibility: "lan", Listener: "office", CertificateSource: "default"}}
	for _, payload := range []string{`{"domain":"office.example.test","service":"web","port":8080}`, `{"domain":"office.example.test","service":"web","port":8080,"policy":{"path_prefix":"/new"}}`} {
		var input routeInput
		if err := json.Unmarshal([]byte(payload), &input); err != nil {
			t.Fatal(err)
		}
		routes, err := retainedRouteAuth([]routeInput{input}, []worker.Route{old})
		if err != nil || routes[0].Policy.Visibility != "lan" || routes[0].Policy.Listener != "office" || routes[0].Policy.CertificateSource != "default" {
			t.Fatal("privacy dropped on older-client save", routes, err)
		}
	}
	var input routeInput
	if err := json.Unmarshal([]byte(`{"domain":"office.example.test","service":"web","port":8080,"policy":{"visibility":"public","listener":null}}`), &input); err != nil {
		t.Fatal(err)
	}
	routes, err := retainedRouteAuth([]routeInput{input}, []worker.Route{old})
	if err != nil || routes[0].Policy.Visibility != "public" || routes[0].Policy.Listener != "" {
		t.Fatal("explicit public switch not applied", routes, err)
	}
}

func TestPrivateRouteListenerAPIAndNodeReservations(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('local','Local','localhost',22,'',$1,'ready')", []byte{})
	f.sql(`UPDATE nodes SET capabilities='{"route_protocol":5,"route_policies":true,"live_routes":true,"ingress_exposures":true}' WHERE id='local'`)
	project := jsonString(t, f.call("root", "default", "POST", "/api/projects", map[string]any{"name": "Private routes"}, 201), "id")
	settings := worker.IngressExposureSettings{Listeners: []worker.IngressListener{{ID: "office", Protocol: "https", Visibility: "lan", Bindings: []worker.IngressBinding{{Address: "192.168.10.2", Port: 8443}}}}}
	f.s.workerRPC = func(_ context.Context, node string, request worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "local" {
			t.Fatal(node)
		}
		if request.Method == "ingress_exposures_plan" {
			return worker.RPCResponse{ExposurePlan: &worker.IngressExposurePlan{Settings: request.Exposures.Settings}}, nil
		}
		return worker.RPCResponse{Exposures: &worker.IngressExposureStatus{Settings: settings}}, nil
	}
	route := worker.Route{Domain: "office.example.test", Service: "web", Port: 8080, Policy: &worker.RoutePolicy{Visibility: "lan", Listener: "office", CertificateSource: "default"}}
	resource := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Private", NodeID: "local", ProjectID: project, Kind: "service", Compose: "services: {}", Routes: []worker.Route{route}}, 201), "id")
	viewer := f.user("private-viewer", "private-viewer@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','private-viewer','member',$1)`, `[{"scope":"project","scope_id":"`+project+`","role":"viewer"}]`)
	body := f.call(viewer, "default", "GET", "/api/workloads/"+resource+"/route-listeners", nil, 200).Body.String()
	if !strings.Contains(body, "office") || strings.Contains(body, "change") || strings.Contains(body, "revision") {
		t.Fatal("listener metadata response is wrong", body)
	}
	f.call(viewer, "default", "PUT", "/api/workloads/"+resource+"/routes", map[string]any{"routes": []worker.Route{route}}, 403)
	outsider := f.user("private-outsider", "private-outsider@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','private-outsider','member','[]')`)
	f.call(outsider, "default", "GET", "/api/workloads/"+resource+"/route-listeners", nil, 403)
	f.call("root", "default", "POST", "/api/nodes/local/ingress-listeners/plan", worker.IngressExposureSettings{}, 409)
	f.call("root", "default", "POST", "/api/nodes/local/ingress-listeners/plan", settings, 200)
	changed := settings
	changed.Listeners = []worker.IngressListener{{ID: "office", Protocol: "https", Visibility: "tailscale", Bindings: []worker.IngressBinding{{Address: "100.64.1.2", Port: 8443}}}}
	f.call("root", "default", "POST", "/api/nodes/local/ingress-listeners/plan", changed, 409)
	route.Policy.Listener = "missing"
	f.call("root", "default", "PUT", "/api/workloads/"+resource+"/routes", map[string]any{"routes": []worker.Route{route}}, 409)
	f.sql(`UPDATE nodes SET capabilities='{"route_protocol":4,"route_policies":true,"live_routes":true}' WHERE id='local'`)
	route.Policy.Listener = "office"
	f.call("root", "default", "PUT", "/api/workloads/"+resource+"/routes", map[string]any{"routes": []worker.Route{route}}, 409)
	f.sql(`UPDATE nodes SET capabilities='{"route_protocol":5,"route_policies":true,"live_routes":true}' WHERE id='local'`)
	f.call("root", "default", "PUT", "/api/workloads/"+resource+"/routes", map[string]any{"routes": []worker.Route{}}, 200)
	payload, err := json.Marshal(worker.Request{ID: "private-reservation", Workload: resource, Action: "routes", Routes: []worker.Route{route}})
	if err != nil {
		t.Fatal(err)
	}
	protected, err := seal(f.s.Key, payload)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,state) VALUES('private-reservation',$1,'routes',$2,'queued')`, resource, protected)
	f.call("root", "default", "POST", "/api/nodes/local/ingress-listeners/plan", worker.IngressExposureSettings{}, 409)
	f.sql(`UPDATE operations SET state='failed' WHERE id='private-reservation'`)
	f.call("root", "default", "POST", "/api/nodes/local/ingress-listeners/plan", worker.IngressExposureSettings{}, 200)
}
