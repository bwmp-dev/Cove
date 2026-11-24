package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestTransportRoutesRequireDedicatedListenersAndRejectHTTPSettings(t *testing.T) {
	base := Route{Protocol: "tcp", Service: "db", Port: 5432, Policy: &RoutePolicy{Visibility: "public", Listener: "database"}}
	if err := ValidateRoutes([]Route{base}, "service"); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Route){
		func(r *Route) { r.Protocol = "other" },
		func(r *Route) { r.Domain = "db.example.test" },
		func(r *Route) { r.Auth = "secret" },
		func(r *Route) { r.Policy.Listener = "" },
		func(r *Route) { r.Policy.Visibility = "" },
		func(r *Route) { r.Policy.CertificateSource = "default" },
		func(r *Route) { r.Policy.PathPrefix = "/" },
		func(r *Route) { r.Policy.Sticky = true },
		func(r *Route) { r.Policy.Timeouts = &RouteTimeouts{DialSeconds: 5} },
		func(r *Route) { r.Protocol = "udp"; r.Policy.IPAllowList = []string{"127.0.0.0/8"} },
	} {
		next := base
		policy := *base.Policy
		next.Policy = &policy
		change(&next)
		if ValidateRoutes([]Route{next}, "service") == nil {
			t.Fatalf("accepted invalid raw route: %#v %#v", next, next.Policy)
		}
	}
	if ValidateRoutes([]Route{base, base}, "service") == nil || ValidateRoutes([]Route{base}, "job") == nil {
		t.Fatal("accepted duplicate listener or job route")
	}
	scale := Request{Action: "deploy", Kind: "service", Routes: []Route{base}, Rollout: &RolloutOptions{Strategy: "rolling"}, Scale: &CPUScaleOptions{Generation: "previous", Before: map[string]int{"db": 1}, Replicas: map[string]int{"db": 2}, CPUPercent: map[string]float64{"db": 80}}}
	if validateCPUScale(scale) == nil {
		t.Fatal("CPU scaling accepted raw traffic")
	}
	if RouteProtocol([]Route{base}) != 6 || idleAuthEligibility([]Route{base}) == nil {
		t.Fatal("raw route protocol or idle isolation missing")
	}
	settings := IngressExposureSettings{Listeners: []IngressListener{{ID: "database", Protocol: "tcp", Visibility: "public"}}}
	if err := ValidateRouteListeners([]Route{base}, settings); err != nil {
		t.Fatal(err)
	}
	settings.Listeners[0].Protocol = "udp"
	if ValidateRouteListeners([]Route{base}, settings) == nil {
		t.Fatal("accepted mismatched transport")
	}
	if validateTransportOwners(map[string][]Route{"one": {base}, "two": {base}}) == nil {
		t.Fatal("listener shared by resources")
	}
}

func TestTransportConfigurationSeparatesProtocolsAndUpstreams(t *testing.T) {
	tcp := Route{Protocol: "tcp", Service: "one", Port: 9000, Policy: &RoutePolicy{Visibility: "lan", Listener: "socket", IPAllowList: []string{"192.168.0.0/16"}, Upstreams: []RouteUpstream{{Service: "two", Port: 9001}}}}
	udp := Route{Protocol: "udp", Service: "one", Port: 9002, Policy: &RoutePolicy{Visibility: "public", Listener: "datagram"}}
	data, err := ingressConfig(map[string][]Route{"app": {tcp, udp}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data, `"http"`) || strings.Contains(data, `"tls"`) {
		t.Fatal("raw routes emitted HTTP or TLS", data)
	}
	var config dynamicIngress
	if err := json.Unmarshal([]byte(data), &config); err != nil {
		t.Fatal(err)
	}
	if len(config.TCP.Routers) != 1 || len(config.UDP.Routers) != 1 || len(config.TCP.Middlewares) != 2 || len(config.UDP.Middlewares) != 0 {
		t.Fatal(data)
	}
	for _, router := range config.TCP.Routers {
		if router.Rule != "HostSNI(`*`)" || router.EntryPoints[0] != "cove-listener-socket" || len(config.TCP.Services[router.Service].LoadBalancer.Servers) != 2 {
			t.Fatal(data)
		}
	}
	for _, router := range config.UDP.Routers {
		if router.Rule != "" || router.EntryPoints[0] != "cove-listener-datagram" {
			t.Fatal(data)
		}
	}
}

func TestTransportReservationsRefuseCustomAndLiveInterception(t *testing.T) {
	refs := map[string]string{"cove-listener-socket": "tcp", "cove-listener-datagram": "udp"}
	for _, section := range []string{"http", "tcp", "udp"} {
		for _, entries := range []string{"", ", entryPoints: [cove-listener-socket]"} {
			if validateTransportCustomConfig(section+": {routers: {other: {service: app"+entries+"}}}", refs) == nil {
				t.Fatal(section, entries)
			}
		}
	}
	if err := validateTransportCustomConfig("tcp: {routers: {other: {service: app, entryPoints: [other]}}}", refs); err != nil {
		t.Fatal(err)
	}
	if validateTransportLiveData([]byte(`{"tcpRouters":{"other@file":{"entryPoints":["cove-listener-socket"],"service":"app"}}}`), refs, dynamicIngress{}) == nil {
		t.Fatal("live interception accepted")
	}
	owned := dynamicIngress{TCP: &ingressTransportConfig{Routers: map[string]ingressTransportRouter{"owned": {}}}}
	if err := validateTransportLiveData([]byte(`{"tcpRouters":{"owned@file":{"entryPoints":["cove-listener-socket"],"service":"app"}}}`), refs, owned); err != nil {
		t.Fatal(err)
	}
}

func TestRouteTimeoutsAndConfirmation(t *testing.T) {
	route := Route{Domain: "app.example.test", Service: "web", Port: 8080, Policy: &RoutePolicy{Timeouts: &RouteTimeouts{DialSeconds: 4, ResponseHeaderSeconds: 5, IdleConnectionSeconds: 6}}}
	data, err := ingressConfig(map[string][]Route{"app": {route}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data, `"dialTimeout":"4s"`) || !strings.Contains(data, `"responseHeaderTimeout":"5s"`) || !strings.Contains(data, `"idleConnTimeout":"6s"`) || RouteProtocol([]Route{route}) != 6 {
		t.Fatal(data)
	}
	for _, value := range []RouteTimeouts{{}, {DialSeconds: -1}, {DialSeconds: 601}, {ResponseHeaderSeconds: 601}, {IdleConnectionSeconds: 3601}} {
		route.Policy.Timeouts = &value
		if ValidateRoutes([]Route{route}, "service") == nil {
			t.Fatal(value)
		}
	}
	desired := &ingressTransportConfig{Routers: map[string]ingressTransportRouter{"cove-route-tcp-test": {EntryPoints: []string{"cove-listener-socket"}, Rule: "HostSNI(`*`)", Service: "app"}}}
	accepted := `[{"name":"cove-route-tcp-test@file","status":"enabled","entryPoints":["cove-listener-socket"],"rule":"HostSNI(` + "`*`" + `)","service":"app@file"}]`
	if !transportIngressAccepted(accepted, desired) || transportIngressAccepted(accepted, nil) || transportIngressAccepted(strings.Replace(accepted, "enabled", "disabled", 1), desired) || transportIngressAccepted("[]", desired) {
		t.Fatal("incorrect live transport acceptance")
	}
}

func TestUDPRemovalReplacesRetainedHandlerBeforeRemovingRouter(t *testing.T) {
	previous := newIngressConfig()
	previous.setTransportRoute(Route{Protocol: "udp", Policy: &RoutePolicy{Listener: "datagram"}}, []map[string]string{{"address": "192.168.1.1:9000"}})
	desired := newIngressConfig()
	transition, needed, err := retiringUDPIngress(previous, desired)
	if err != nil || !needed || transition.UDP == nil {
		t.Fatal(transition, needed, err)
	}
	for _, service := range transition.UDP.Services {
		if service.LoadBalancer.Servers[0]["address"] != "127.0.0.1:0" {
			t.Fatal("retirement still forwards to app", service)
		}
	}
	if desired.UDP != nil {
		t.Fatal("final desired configuration mutated")
	}
	if _, needed, err := retiringUDPIngress(previous, previous); err != nil || needed {
		t.Fatal("unchanged handler retired", needed, err)
	}
}

func TestTransportRouteProtocolPersistenceAndInterruptedDecisions(t *testing.T) {
	route := Route{Protocol: "udp", Service: "app", Port: 9001, Policy: &RoutePolicy{Visibility: "public", Listener: "datagram"}}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := Request{ID: "raw-protocol", Workload: "app", Action: "deploy", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n", Routes: []Route{route}}
	if response := e.RPC(context.Background(), RPCRequest{Method: "submit_routes_v5", Request: &request}); response.Error == "" {
		t.Fatal("old protocol accepted raw route")
	}
	if response := e.RPC(context.Background(), RPCRequest{Method: "submit_routes_v6", Request: &request}); response.Error != "" {
		t.Fatal(response.Error)
	}
	data, err := marshalWorkerState(map[string][]Route{"app": {route}})
	if err != nil || !bytes.Contains(data, []byte(protectedRouteSource)) {
		t.Fatal("raw route not protected on disk", err)
	}
	var saved map[string][]Route
	if err := json.Unmarshal(data, &saved); err != nil || !RoutesEqual(saved["app"], []Route{route}) {
		t.Fatal(saved, err)
	}
	for _, committed := range []bool{false, true} {
		next, err := NewEngine(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(next.Root, "ingress.json"), map[string][]Route{}); err != nil {
			t.Fatal(err)
		}
		change := journal{Operation: Operation{ID: "raw-remove", State: "running"}, Request: Request{ID: "raw-remove", Workload: "app", Action: "routes"}, RouteChange: &routeChange{Previous: []Route{route}, Committed: committed}}
		if err := writeJSON(next.path("raw-remove"), change); err != nil {
			t.Fatal(err)
		}
		if err := next.Recover(); err != nil {
			t.Fatal(err)
		}
		if err := next.recoverIngressDecisions(); err != nil {
			t.Fatal(err)
		}
		actual, err := next.loadIngress()
		if err != nil {
			t.Fatal(err)
		}
		if committed && len(actual["app"]) != 0 || !committed && !RoutesEqual(actual["app"], []Route{route}) {
			t.Fatal("interrupted raw removal projected wrong decision", committed, actual)
		}
	}
}
