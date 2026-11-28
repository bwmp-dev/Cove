package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWildcardRouteClaimsMatchOneLabelAndPreferExactRoutes(t *testing.T) {
	for _, pair := range []struct {
		a, b string
		want bool
	}{{"*.example.test", "app.example.test", true}, {"*.example.test", "example.test", false}, {"*.example.test", "a.b.example.test", false}, {"*.example.test", "*.example.test", true}, {"*.example.test", "*.sub.example.test", false}, {"app.example.test", "app.example.test", true}, {"app.example.test", "other.example.test", false}} {
		if RouteDomainsOverlap(pair.a, pair.b) != pair.want || RouteDomainsOverlap(pair.b, pair.a) != pair.want {
			t.Fatalf("overlap %q %q", pair.a, pair.b)
		}
	}
	wildcard := Route{Domain: "*.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "uploaded", CertificateID: strings.Repeat("a", 32), PathPrefix: "/deep/path"}}
	exact := Route{Domain: "app.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "uploaded", CertificateID: strings.Repeat("a", 32)}}
	if err := ValidateRoutes([]Route{wildcard, exact}, "service"); err != nil {
		t.Fatal(err)
	}
	implicit := exact
	implicit.Policy = nil
	if err := ValidateRoutes([]Route{wildcard, implicit}, "service"); err == nil || !strings.Contains(err.Error(), "stored certificate") {
		t.Fatal("wildcard TLS could shadow an exact route's default/ACME selection", err)
	}
	if _, err := ingressConfig(map[string][]Route{"one": {wildcard}, "two": {exact}}); err == nil {
		t.Fatal("wildcard stole another resource hostname")
	}
	raw, err := ingressConfig(map[string][]Route{"one": {wildcard, exact}})
	if err != nil {
		t.Fatal(err)
	}
	var config dynamicIngress
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	wildcardPriority, exactPriority := 0, 0
	for _, router := range config.HTTP.Routers {
		if strings.HasPrefix(router.Rule, "HostRegexp(") {
			wildcardPriority = router.Priority
			if router.TLS["certResolver"] != "" || !strings.Contains(router.Rule, "PathPrefix(`/deep/path`)") {
				t.Fatal("wildcard enabled HTTP-01 or lost path routing")
			}
		} else {
			exactPriority = router.Priority
		}
	}
	if wildcardPriority == 0 || exactPriority <= wildcardPriority {
		t.Fatal("exact hostname must beat every wildcard path")
	}
	if RouteProtocol([]Route{exact, wildcard}) != 4 || RouteProtocol([]Route{{Domain: exact.Domain, Policy: &RoutePolicy{CertificateSource: "default"}}}) != 4 {
		t.Fatal("wildcards and cached-ACME suppression need protocol4")
	}
	for _, domain := range []string{"*.test", "*.*.example.test", "a.*.example.test", "*.Example.test"} {
		bad := wildcard
		bad.Domain = domain
		if ValidateRoutes([]Route{bad}, "service") == nil {
			t.Fatalf("accepted %q", domain)
		}
	}
	wildcard.Policy = nil
	if ValidateRoutes([]Route{wildcard}, "service") == nil {
		t.Fatal("wildcard was accepted without an assigned certificate")
	}
}

func TestWildcardCertificateProbeAvoidsExactHostnameOverrides(t *testing.T) {
	domains := map[string]bool{"a.example.test": true, "b.example.test": true}
	if got := routeProbeHostname("*.example.test", domains); got != "c.example.test" {
		t.Fatal(got)
	}
}

func TestExactHostRetainsExistingProxyRulePriority(t *testing.T) {
	for _, route := range []Route{{Domain: "app.example.test", Service: "app", Port: 8080}, {Domain: "app.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{PathPrefix: "/long/path", CertificateSource: "default"}}} {
		raw, err := ingressConfig(map[string][]Route{"app": {route}})
		if err != nil {
			t.Fatal(err)
		}
		var config dynamicIngress
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			t.Fatal(err)
		}
		for _, router := range config.HTTP.Routers {
			if router.Priority != 0 {
				t.Fatal("existing exact-host ordering relative to custom routers changed")
			}
		}
	}
}

func TestWildcardRouteConfirmationRejectsStalePriority(t *testing.T) {
	route := Route{Domain: "*.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "uploaded", CertificateID: strings.Repeat("a", 32)}}
	config := newIngressConfig()
	config.addRoute("app", route)
	for name, router := range config.HTTP.Routers {
		live := []map[string]any{{"name": name + "@file", "status": "enabled", "rule": router.Rule, "service": router.Service, "priority": router.Priority, "entryPoints": router.EntryPoints}}
		raw, _ := json.Marshal(live)
		if !ingressAccepted(string(raw), config) {
			t.Fatal("current wildcard priority was not accepted")
		}
		live[0]["priority"] = 0
		raw, _ = json.Marshal(live)
		if ingressAccepted(string(raw), config) {
			t.Fatal("stale wildcard priority was accepted")
		}
	}
}

func TestRouteCertificateSourceCannotDependOnHTTPPath(t *testing.T) {
	routes := []Route{{Domain: "app.example.test", Service: "app", Port: 8080}, {Domain: "app.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{PathPrefix: "/private", CertificateSource: "default"}}}
	if err := ValidateRoutes(routes, "service"); err == nil || !strings.Contains(err.Error(), "same certificate source") {
		t.Fatal("TLS selection depended on the HTTP path", err)
	}
	routes[1].Policy.CertificateSource = "acme"
	if err := ValidateRoutes(routes, "service"); err != nil {
		t.Fatal("implicit and explicit ACME selections should agree", err)
	}
}
