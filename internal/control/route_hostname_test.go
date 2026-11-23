package control

import (
	"cove/internal/worker"
	"testing"
)

func TestWildcardRouteClaimsIncludePanelOtherResourcesAndQueuedChanges(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('wild-node','Node','localhost',22,'',$1,'ready','{"route_policies":true,"live_routes":true,"route_protocol":4}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('wild-project','Project','default')`)
	create := func(name string) string {
		return jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: name, NodeID: "wild-node", ProjectID: "wild-project", Kind: "service", Compose: "services: {}"}, 201), "id")
	}
	first, second := create("Wildcard"), create("Exact")
	certificate, key := certificateFixture(t, "*.example.test")
	version := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+first+"/certificates", map[string]string{"name": "Wildcard", "certificate_pem": certificate, "private_key_pem": key}, 201), "id")
	wildcard := worker.Route{Domain: "*.example.test", Service: "web", Port: 8080, Policy: &worker.RoutePolicy{CertificateSource: "uploaded", CertificateID: version}}
	exact := worker.Route{Domain: "app.example.test", Service: "web", Port: 8080}
	save := func(resource string, routes []worker.Route, status int) {
		f.call("root", "default", "PUT", "/api/workloads/"+resource+"/routes", map[string]any{"routes": routes}, status)
	}
	save(second, []worker.Route{exact}, 200)
	save(first, []worker.Route{wildcard}, 409)
	save(second, []worker.Route{}, 200)
	save(first, []worker.Route{wildcard}, 200)
	save(second, []worker.Route{exact}, 409)
	save(second, []worker.Route{{Domain: "deep.app.example.test", Service: "web", Port: 8080}}, 200)
	save(first, []worker.Route{}, 200)
	payload, err := f.s.sealJSON(worker.Request{ID: "pending-wildcard", Workload: first, Action: "routes", Kind: "service", Routes: []worker.Route{wildcard}})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES('pending-wildcard',$1,'routes',$2)`, first, payload)
	save(second, []worker.Route{exact}, 409)
	f.sql(`UPDATE operations SET state='cancelled' WHERE id='pending-wildcard'`)
	save(second, []worker.Route{}, 200)
	f.sql(`UPDATE nodes SET capabilities='{"route_policies":true,"live_routes":true,"route_protocol":3}' WHERE id='wild-node'`)
	save(first, []worker.Route{wildcard}, 409)
	save(second, []worker.Route{{Domain: exact.Domain, Service: exact.Service, Port: exact.Port, Policy: &worker.RoutePolicy{CertificateSource: "default"}}}, 409)
	f.sql(`UPDATE nodes SET capabilities='{"route_policies":true,"live_routes":true,"route_protocol":4}' WHERE id='wild-node'`)
	panelCertificate, panelKey := certificateFixture(t, "*.example.com")
	panelVersion := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+first+"/certificates", map[string]string{"name": "Panel overlap", "certificate_pem": panelCertificate, "private_key_pem": panelKey}, 201), "id")
	wildcard.Domain, wildcard.Policy.CertificateID = "*.example.com", panelVersion
	save(first, []worker.Route{wildcard}, 409)
}
