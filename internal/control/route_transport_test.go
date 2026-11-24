package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"testing"
)

func TestTransportListenerReservationsAndProtocol(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('raw-node','Raw','localhost',22,'',$1,'ready','{"route_protocol":6,"route_policies":true,"live_routes":true,"ingress_exposures":true}')`, []byte{})
	project := jsonString(t, f.call("root", "default", "POST", "/api/projects", map[string]any{"name": "Raw routes"}, 201), "id")
	settings := worker.IngressExposureSettings{Listeners: []worker.IngressListener{{ID: "socket", Protocol: "tcp", Visibility: "public", Bindings: []worker.IngressBinding{{Address: "127.0.0.1", Port: 15432}}}}}
	f.s.workerRPC = func(_ context.Context, _ string, request worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{Exposures: &worker.IngressExposureStatus{Settings: settings}}, nil
	}
	route := worker.Route{Protocol: "tcp", Service: "db", Port: 5432, Policy: &worker.RoutePolicy{Listener: "socket", Visibility: "public"}}
	create := func(name string, routes []worker.Route, code int) string {
		response := f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: name, Kind: "service", NodeID: "raw-node", ProjectID: project, Compose: "services: {}", Routes: routes}, code)
		if code == 201 {
			return jsonString(t, response, "id")
		}
		return ""
	}
	one := create("One", []worker.Route{route}, 201)
	create("Conflict", []worker.Route{route}, 409)
	two := create("Two", nil, 201)
	f.call("root", "default", "PUT", "/api/workloads/"+two+"/routes", map[string]any{"routes": []worker.Route{route}}, 409)
	f.call("root", "default", "PUT", "/api/workloads/"+one+"/routes", map[string]any{"routes": []worker.Route{}}, 200)
	payload, err := json.Marshal(worker.Request{ID: "raw-pending", Workload: one, Action: "routes", Routes: []worker.Route{route}})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := seal(f.s.Key, payload)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,state) VALUES('raw-pending',$1,'routes',$2,'queued')`, one, sealed)
	f.call("root", "default", "PUT", "/api/workloads/"+two+"/routes", map[string]any{"routes": []worker.Route{route}}, 409)
	f.sql(`UPDATE operations SET state='failed' WHERE id='raw-pending'`)
	f.sql(`UPDATE nodes SET capabilities='{"route_protocol":5,"route_policies":true,"live_routes":true}' WHERE id='raw-node'`)
	f.call("root", "default", "PUT", "/api/workloads/"+two+"/routes", map[string]any{"routes": []worker.Route{route}}, 409)
	f.sql(`UPDATE nodes SET capabilities='{"route_protocol":6,"route_policies":true,"live_routes":true,"ingress_exposures":true}' WHERE id='raw-node'`)
	f.call("root", "default", "PUT", "/api/workloads/"+two+"/routes", map[string]any{"routes": []worker.Route{route}}, 200)
	f.call("root", "default", "POST", "/api/nodes/raw-node/ingress-listeners/plan", worker.IngressExposureSettings{}, 409)
	var config workloadInput
	if err := json.Unmarshal(f.call("root", "default", "GET", "/api/workloads/"+two, nil, 200).Body.Bytes(), &config); err != nil || config.Routes[0].Protocol != "tcp" {
		t.Fatal(config, err)
	}
}

func TestRouteTimeoutsRetainedAndRawRoutesMatchedByListener(t *testing.T) {
	old := []worker.Route{{Domain: "app.example.test", Service: "web", Port: 8080, Policy: &worker.RoutePolicy{Timeouts: &worker.RouteTimeouts{ResponseHeaderSeconds: 10}}}}
	for _, explicit := range []bool{false, true} {
		input := `{"domain":"app.example.test","service":"web","port":8080}`
		if explicit {
			input = `{"domain":"app.example.test","service":"web","port":8080,"policy":{"timeouts":null}}`
		}
		var row routeInput
		if err := json.Unmarshal([]byte(input), &row); err != nil {
			t.Fatal(err)
		}
		got, err := retainedRouteAuth([]routeInput{row}, old)
		if err != nil {
			t.Fatal(err)
		}
		if !explicit && (got[0].Policy == nil || got[0].Policy.Timeouts.ResponseHeaderSeconds != 10) {
			t.Fatal("timeout lost")
		}
		if explicit && got[0].Policy.Timeouts != nil {
			t.Fatal("explicit timeout removal lost")
		}
	}
	previous := []worker.Route{{Protocol: "tcp", Service: "one", Port: 1234, Policy: &worker.RoutePolicy{Listener: "one", Visibility: "lan"}}, {Protocol: "udp", Service: "two", Port: 1234, Policy: &worker.RoutePolicy{Listener: "two", Visibility: "public"}}}
	var next []routeInput
	data, _ := json.Marshal([]worker.Route{previous[1], previous[0]})
	if err := json.Unmarshal(data, &next); err != nil {
		t.Fatal(err)
	}
	got, err := retainedRouteAuth(next, previous)
	if err != nil || got[0].Policy.Listener != "two" || got[1].Policy.Listener != "one" {
		t.Fatal(got, err)
	}
}

func TestPreviewPublishingDoesNotCopyRawOrPrivateListeners(t *testing.T) {
	for _, route := range []worker.Route{{Protocol: "tcp", Service: "db", Port: 5432, Policy: &worker.RoutePolicy{Listener: "database", Visibility: "public"}}, {Domain: "office.example.test", Service: "web", Port: 8080, Policy: &worker.RoutePolicy{Visibility: "lan", Listener: "office", CertificateSource: "default"}}} {
		source := &previewSource{ID: "app", Name: "App", Kind: "service", Config: workloadInput{Routes: []worker.Route{route}}}
		for _, publish := range []bool{false, true} {
			recipe := previewRecipe{}
			if publish {
				recipe.DomainTemplate = "{resource}-{preview}.example.test"
			}
			plan := new(Server).planPreviewResource(recipe, source, &previewTopology{}, map[string]bool{"app": true}, nil)
			found := false
			for _, blocker := range plan.Blockers {
				found = found || blocker.Code == "route_listener"
			}
			if found != publish || len(plan.Routes) != 0 {
				t.Fatal("preview published or copied a production listener", publish, plan)
			}
		}
	}
}

func TestCPUScalingRejectsRawRouteConfigurations(t *testing.T) {
	config := workloadInput{Kind: "service", Routes: []worker.Route{{Protocol: "tcp", Service: "app", Port: 9000, Policy: &worker.RoutePolicy{Listener: "socket", Visibility: "public"}}}, Rollout: &worker.RolloutOptions{Strategy: "rolling"}, Limits: map[string]worker.ServiceLimits{"app": {CPUs: 1, MemoryMB: 64}}, Autoscale: &worker.AutoscalePolicy{Enabled: true, Services: map[string]worker.AutoscaleService{"app": {MinReplicas: 1, MaxReplicas: 2, TargetCPUPercent: 70}}}}
	if validateAutoscale(config) == nil {
		t.Fatal("CPU scaling enabled on raw route")
	}
	config.Autoscale.Enabled = false
	if err := validateAutoscale(config); err != nil {
		t.Fatal("disabled scaling blocked raw route", err)
	}
}
