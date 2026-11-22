package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIngressValidationAndOwnership(t *testing.T) {
	for _, domain := range []string{"https://example.com", "x.com\nadmin off", "*.example.com", "example.com:443", "example.com/", "-bad.example", "EXAMPLE.com", "x..com"} {
		if ValidateRoutes([]Route{{Domain: domain, Service: "web", Port: 80}}, "service") == nil {
			t.Fatal("unsafe hostname accepted", domain)
		}
	}
	route := Route{Domain: "app.example.com", Service: "web", Port: 8080}
	if ValidateRoutes([]Route{route}, "job") == nil {
		t.Fatal("job ingress accepted")
	}
	if _, err := ingressConfig(map[string][]Route{"one": {route}, "two": {route}}); err == nil {
		t.Fatal("hostname takeover accepted")
	}
	config, err := ingressConfig(map[string][]Route{"one": {route}})
	if err != nil || !strings.Contains(config, "app.example.com") || !strings.Contains(config, routeAlias("one", "web")+":8080") {
		t.Fatal(config, err)
	}
	if routeAlias("one", "web") == routeAlias("two", "web") {
		t.Fatal("ambiguous upstream alias")
	}
}

func TestIngressConfigurationAcceptance(t *testing.T) {
	empty, err := ingressConfig(map[string][]Route{})
	if err != nil || empty != "{}" {
		t.Fatal("empty ingress must omit unsupported empty nested maps", empty, err)
	}
	config := newIngressConfig()
	config.add("app.example.com", "http://app:8080", "")
	var name string
	var router ingressRouter
	for name, router = range config.HTTP.Routers {
	}
	current := []map[string]string{{"name": name + "@file", "status": "enabled", "rule": router.Rule, "service": router.Service}}
	encode := func() string {
		data, err := json.Marshal(current)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if !ingressAccepted(encode(), config) {
		t.Fatal("valid routes rejected")
	}
	current[0]["status"] = "disabled"
	if ingressAccepted(encode(), config) {
		t.Fatal("disabled route accepted")
	}
	current[0]["status"] = "enabled"
	updated := newIngressConfig()
	updated.add("app.example.com", "http://app:9090", "")
	if ingressAccepted(encode(), updated) {
		t.Fatal("stale upstream accepted")
	}
	if ingressAccepted(encode(), newIngressConfig()) {
		t.Fatal("removed route still active")
	}
	if ingressAccepted("[]", config) || ingressAccepted("invalid", newIngressConfig()) {
		t.Fatal("missing or invalid response accepted")
	}
	if !ingressAccepted("[]", newIngressConfig()) {
		t.Fatal("empty config rejected")
	}
}

func TestIngressACMEConfiguration(t *testing.T) {
	for _, directory := range []string{"http://ca.test/dir", "https://user:password@ca.test/dir", "https://ca.test/dir\n", "https://ca.test/dir#fragment"} {
		if _, err := (&Engine{ACMEDirectory: directory}).ingressArgs(); err == nil {
			t.Fatal("unsafe ACME directory accepted", directory)
		}
	}
	if _, err := (&Engine{ACMERoot: "ca.pem"}).ingressArgs(); err == nil {
		t.Fatal("root without directory accepted")
	}
	args, err := (&Engine{ACMEDirectory: "https://ca.test/dir", ACMERoot: "ca.pem"}).ingressArgs()
	if err != nil || !strings.Contains(strings.Join(args, " "), "--certificatesresolvers.cove.acme.cacertificates=/data/acme-root.pem") {
		t.Fatal(args, err)
	}
	for _, arg := range args {
		if strings.Contains(strings.ToLower(arg), "skipverify") {
			t.Fatal("TLS verification bypassed")
		}
	}
}
