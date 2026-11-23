package worker

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRoutePolicies(t *testing.T) {
	r := Route{Domain: "app.example.com", Service: "web", Port: 80, Policy: &RoutePolicy{PathPrefix: "/api", SecurityHeaders: true, RequestHeaders: map[string]string{"X-App": "api"}, ResponseHeaders: map[string]string{"X-Version": "1"}, IPAllowList: []string{"10.0.0.0/8", "fd00::/8"}, Upstreams: []RouteUpstream{{Service: "api", Port: 8080}}, Sticky: true, CertificateSource: "default"}}
	root := Route{Domain: r.Domain, Service: r.Service, Port: r.Port, Policy: &RoutePolicy{CertificateSource: "default"}}
	data, err := ingressConfig(map[string][]Route{"app": {r, root}})
	if err != nil {
		t.Fatal(err)
	}
	var config dynamicIngress
	if err = json.Unmarshal([]byte(data), &config); err != nil {
		t.Fatal(err)
	}
	if len(config.HTTP.Routers) != 2 {
		t.Fatal("path route overwrote host route", data)
	}
	for _, router := range config.HTTP.Routers {
		if !strings.Contains(router.Rule, "PathPrefix") {
			continue
		}
		service := config.HTTP.Services[router.Service]
		if len(service.LoadBalancer.Servers) != 2 || service.LoadBalancer.Sticky == nil || len(router.TLS) != 0 || len(router.Middlewares) != 2 {
			t.Fatal(data)
		}
	}
	if !strings.Contains(data, routeAlias("app", "api")+":8080") || !strings.Contains(data, `"stsSeconds":31536000`) {
		t.Fatal(data)
	}
	var copy Route
	encoded, _ := json.Marshal(r)
	if err = json.Unmarshal(encoded, &copy); err != nil {
		t.Fatal(err)
	}
	if !RoutesEqual([]Route{r}, []Route{copy}) {
		t.Fatal("equal decoded policies differ")
	}
	copy.Policy.ResponseHeaders["X-Version"] = "2"
	if RoutesEqual([]Route{r}, []Route{copy}) {
		t.Fatal("different policies compare equal")
	}
	changed, err := ingressConfig(map[string][]Route{"app": {copy, root}})
	if err != nil || changed == data {
		t.Fatal("changed middleware must change router identity", err)
	}
	if targets := routeTargets([]Route{r}); len(targets) != 2 || targets[1].Service != "api" {
		t.Fatal(targets)
	}
}

func TestRoutePolicyUpstreamsAndRollback(t *testing.T) {
	web := newContainer("web-id", "app", "web", "running")
	api := newContainer("api-id", "app", "api", "running")
	e, docker := routeEngine(t, web, api)
	previous := Route{Domain: "app.example.com", Service: "web", Port: 80, Policy: &RoutePolicy{ResponseHeaders: map[string]string{"X-Version": "1"}}}
	deployed(t, e, "app", []Route{previous})
	next := previous
	next.Policy = &RoutePolicy{ResponseHeaders: map[string]string{"X-Version": "2"}, Upstreams: []RouteUpstream{{Service: "api", Port: 8080}}}
	if op := runRoutes(t, e, "policy-update", "app", []Route{next}); op.State != "succeeded" {
		t.Fatal(op)
	}
	for _, container := range []*fakeContainer{web, api} {
		if !slices.Contains(container.aliases["cove-ingress"], routeAlias("app", container.service)) {
			t.Fatal("upstream not attached", container)
		}
	}
	if len(docker.lifecycle) != 0 {
		t.Fatal("route policy restarted application", docker.lifecycle)
	}
	failed := next
	failed.Policy = &RoutePolicy{ResponseHeaders: map[string]string{"X-Version": "3"}, Upstreams: []RouteUpstream{{Service: "missing", Port: 80}}}
	if op := runRoutes(t, e, "missing-upstream", "app", []Route{failed}); op.State != "failed" {
		t.Fatal(op)
	}
	docker.failMove = 1
	if op := runRoutes(t, e, "policy-rollback", "app", []Route{previous}); op.State != "failed" {
		t.Fatal(op)
	}
	current, _, err := readCurrent(filepath.Join(e.Root, "workloads", "app", "current.json"))
	if err != nil || !RoutesEqual(current.Routes, []Route{next}) {
		t.Fatal("policy rollback lost current state", current, err)
	}
	owned, err := e.loadIngress()
	if err != nil || !RoutesEqual(owned["app"], []Route{next}) {
		t.Fatal("policy rollback lost ingress state", owned, err)
	}
}

func TestRoutePolicyValidation(t *testing.T) {
	for _, p := range []RoutePolicy{
		{PathPrefix: "/`) || Host(`evil"}, {PathPrefix: "relative"}, {CertificateSource: "unknown"},
		{IPAllowList: []string{"10.0.0.1"}}, {RequestHeaders: map[string]string{"X-Test": "a\r\nb"}},
		{RequestHeaders: map[string]string{"X-Forwarded-For": "127.0.0.1"}},
		{ResponseHeaders: map[string]string{"x-test": "a", "X-Test": "b"}},
		{SecurityHeaders: true, ResponseHeaders: map[string]string{"Strict-Transport-Security": "max-age=0"}},
		{Upstreams: []RouteUpstream{{Service: "web", Port: 80}}}, {Upstreams: []RouteUpstream{{Service: "http://metadata", Port: 80}}},
		{RedirectHost: "app.example.com"}, {RedirectHost: "https://example.com"}, {RedirectPermanent: true},
	} {
		if err := ValidateRoutes([]Route{{Domain: "app.example.com", Service: "web", Port: 80, Policy: &p}}, "service"); err == nil {
			t.Fatalf("invalid policy accepted: %+v", p)
		}
	}
	r := Route{Domain: "app.example.com", Service: "web", Port: 80, Policy: &RoutePolicy{RedirectHost: "www.example.com", RedirectPermanent: true}}
	data, err := ingressConfig(map[string][]Route{"app": {r}})
	if err != nil || !strings.Contains(data, `https://www.example.com${1}`) {
		t.Fatal(data, err)
	}
	if ValidateRoutes([]Route{r, r}, "service") == nil {
		t.Fatal("duplicate host and path accepted")
	}
}

func TestRoutePolicyStickyCookiesAreDistinct(t *testing.T) {
	routes := []Route{
		{Domain: "app.example.com", Service: "web", Port: 80, Policy: &RoutePolicy{Sticky: true}},
		{Domain: "app.example.com", Service: "api", Port: 80, Policy: &RoutePolicy{PathPrefix: "/api", Sticky: true}},
	}
	data, err := ingressConfig(map[string][]Route{"app": routes})
	if err != nil {
		t.Fatal(err)
	}
	var config dynamicIngress
	if err = json.Unmarshal([]byte(data), &config); err != nil {
		t.Fatal(err)
	}
	names := map[any]bool{}
	for _, service := range config.HTTP.Services {
		cookie := service.LoadBalancer.Sticky["cookie"].(map[string]any)
		if names[cookie["name"]] {
			t.Fatal("routes on one hostname share a sticky cookie", data)
		}
		names[cookie["name"]] = true
	}
	if len(names) != 2 {
		t.Fatal(data)
	}
}
