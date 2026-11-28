package worker

import (
	"context"
	"encoding/json"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateRouteListenersNeverFallBackToPublic(t *testing.T) {
	settings := IngressExposureSettings{Listeners: []IngressListener{{ID: "office", Protocol: "https", Visibility: "lan"}, {ID: "tailnet", Protocol: "https", Visibility: "tailscale"}, {ID: "stream", Protocol: "tcp", Visibility: "public"}}}
	for _, test := range []struct {
		protocol, visibility, id string
		valid                    bool
	}{{"http", "public", "", true}, {"http", "", "", true}, {"http", "lan", "office", true}, {"http", "tailscale", "tailnet", true}, {"tcp", "public", "stream", true}, {"http", "lan", "", false}, {"http", "tailscale", "office", false}, {"http", "public", "office", false}, {"tcp", "public", "office", false}, {"udp", "public", "stream", false}, {"http", "lan", "removed", false}} {
		err := validateRouteListener(test.protocol, test.visibility, test.id, settings)
		if (err == nil) != test.valid {
			t.Fatalf("%s/%s/%s: %v", test.protocol, test.visibility, test.id, err)
		}
	}
}

func TestPrivateRouteDenialCoversEveryPathOnPublicHTTPS(t *testing.T) {
	for _, domain := range []string{"office.example.test", "*.office.example.test"} {
		guard := privateRouteDenial(domain)
		if guard.Rule != routeHostRule(domain) || strings.Contains(guard.Rule, "Path") || len(guard.EntryPoints) != 1 || guard.EntryPoints[0] != "websecure" || guard.Service != "noop@internal" || len(guard.TLS) != 0 || guard.Priority <= routePriority(Route{Domain: domain}) {
			t.Fatal("private route guard can fall through to a public wildcard", guard)
		}
	}
}

func TestPrivateRouteSourceRangesKeepLANAndTailscaleSeparate(t *testing.T) {
	for _, test := range []struct {
		address   string
		lan, tail bool
	}{{"10.20.30.40", true, false}, {"172.16.2.1", true, false}, {"192.168.1.2", true, false}, {"fc00::123", true, false}, {"fd7a:115c:a1df:ffff::1", true, false}, {"fd7a:115c:a1e1::1", true, false}, {"100.64.0.1", false, true}, {"100.127.255.255", false, true}, {"fd7a:115c:a1e0::1", false, true}, {"fd7a:115c:a1e0:ffff::1", false, true}, {"100.63.255.255", false, false}, {"8.8.8.8", false, false}, {"2001:db8::1", false, false}, {"127.0.0.1", false, false}} {
		for visibility, wanted := range map[string]bool{"lan": test.lan, "tailscale": test.tail} {
			matched := false
			for _, cidr := range privateRouteRanges(visibility) {
				matched = matched || netip.MustParsePrefix(cidr).Contains(netip.MustParseAddr(test.address))
			}
			if matched != wanted {
				t.Fatalf("%s/%s allowed=%v", visibility, test.address, matched)
			}
		}
	}
}

func TestPrivateRoutePolicyValidationAndPublicDenials(t *testing.T) {
	route := Route{Domain: "office.example.test", Service: "web", Port: 8080, Policy: &RoutePolicy{Visibility: "lan", Listener: "office", CertificateSource: "default", PathPrefix: "/api"}}
	for _, visibility := range []string{"lan", "tailscale"} {
		route.Policy.Visibility = visibility
		if err := ValidateRoutes([]Route{route}, "service"); err != nil {
			t.Fatal(err)
		}
		raw, err := ingressConfig(map[string][]Route{"app": {route}})
		if err != nil {
			t.Fatal(err)
		}
		var config dynamicIngress
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			t.Fatal(err)
		}
		if len(config.HTTP.Routers) != 3 {
			t.Fatal("private route needs HTTP and HTTPS denials")
		}
		for _, router := range config.HTTP.Routers {
			if router.Service == "noop@internal" {
				if strings.Contains(router.Rule, "Path") {
					t.Fatal("path-specific public denial")
				}
				continue
			}
			if len(router.EntryPoints) != 1 || router.EntryPoints[0] != "cove-listener-office" || len(router.Middlewares) != 1 {
				t.Fatal("private binding or source check missing", router)
			}
			allow := config.HTTP.Middlewares[router.Middlewares[0]].IPAllowList
			if allow == nil {
				t.Fatal("private source allowlist missing")
			}
		}
	}
	for _, policy := range []RoutePolicy{{Visibility: "unknown"}, {Visibility: "lan", CertificateSource: "default"}, {Visibility: "public", Listener: "office"}, {Visibility: "lan", Listener: "office", CertificateSource: "acme"}} {
		route.Policy = &policy
		if err := ValidateRoutes([]Route{route}, "service"); err == nil {
			t.Fatal("invalid privacy policy accepted", policy)
		}
	}
}

func TestPrivateRouteVisibilityCannotDifferAcrossPathsOrWildcards(t *testing.T) {
	private := Route{Domain: "office.example.test", Service: "web", Port: 8080, Policy: &RoutePolicy{Visibility: "lan", Listener: "office", CertificateSource: "uploaded", CertificateID: strings.Repeat("a", 32)}}
	for _, domain := range []string{private.Domain, "*.example.test"} {
		public := Route{Domain: domain, Service: "web", Port: 8080, Policy: &RoutePolicy{CertificateSource: "uploaded", CertificateID: strings.Repeat("a", 32), PathPrefix: "/other"}}
		if err := ValidateRoutes([]Route{private, public}, "service"); err == nil {
			t.Fatal("mixed visibility allowed", domain)
		}
	}
}

func TestPrivateRouteDowngradeAndPendingListenerRemoval(t *testing.T) {
	route := Route{Domain: "office.example.test", Service: "web", Port: 8080, Policy: &RoutePolicy{Visibility: "lan", Listener: "office", CertificateSource: "default"}}
	if RouteProtocol([]Route{route}) != 5 {
		t.Fatal("private routes require version5")
	}
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := Request{ID: "private-pending", Workload: "app", Action: "deploy", Kind: "service", Compose: "services:\n  web:\n    image: busybox:1.37\n", Routes: []Route{route}}
	if response := engine.RPC(context.Background(), RPCRequest{Method: "submit_routes_v4", Request: &request}); response.Error == "" {
		t.Fatal("older protocol accepted private route")
	}
	if response := engine.RPC(context.Background(), RPCRequest{Method: "submit_routes_v5", Request: &request}); response.Error != "" {
		t.Fatal(response.Error)
	}
	if err := engine.validatePendingRouteListeners(IngressExposureSettings{}); err == nil {
		t.Fatal("queued private route lost its listener")
	}
	settings := IngressExposureSettings{Listeners: []IngressListener{{ID: "office", Protocol: "https", Visibility: "lan"}}}
	if err := engine.validatePendingRouteListeners(settings); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(engine.Root, "ingress.json"), map[string][]Route{"app": {route}}); err != nil {
		t.Fatal(err)
	}
	engine, err = NewEngine(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := engine.loadIngress()
	if err != nil || !RoutesEqual(loaded["app"], []Route{route}) {
		t.Fatal("private restart persistence failed", err)
	}
	encoded, err := marshalWorkerState(map[string][]Route{"app": {route}})
	if err != nil {
		t.Fatal(err)
	}
	type oldPolicy struct {
		CertificateSource string `json:"certificate_source"`
	}
	var legacy map[string][]struct {
		Policy oldPolicy `json:"policy"`
	}
	if err := json.Unmarshal(encoded, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy["app"][0].Policy.CertificateSource != protectedRouteSource {
		t.Fatal("old workers can ignore private route metadata")
	}
}
