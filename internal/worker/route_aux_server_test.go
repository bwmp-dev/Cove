package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRouteAuxiliaryRefusesForeignHelpers(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		e, err := NewEngine(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(e.Root, "route-aux"), 0700); err != nil {
			t.Fatal(err)
		}
		id := strings.Repeat("a", 64)
		e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
			switch {
			case len(args) > 2 && args[0] == "docker" && args[1] == "ps":
				_, err := io.WriteString(out, id)
				return err
			case len(args) > 2 && args[0] == "docker" && args[1] == "inspect":
				_, err := io.WriteString(out, "/foreign route-aux wrong-owner "+strings.Repeat("b", 64))
				return err
			default:
				t.Fatalf("foreign helper was mutated: %v", args)
				return nil
			}
		}
		if cleanup {
			err = e.pruneRouteAuxiliary(context.Background(), newIngressConfig(), io.Discard)
		} else {
			_, err = e.ensureRouteAuxiliary(context.Background(), strings.Repeat("b", 64), "fixture-image", []byte(`{}`), io.Discard)
		}
		if err == nil {
			t.Fatal("foreign route helper accepted", cleanup)
		}
	}
}

type routeAuthTransport func(*http.Request) (*http.Response, error)

func (f routeAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRouteAuxiliaryPagesRequireProxyAndPreserveStatus(t *testing.T) {
	key := strings.Repeat("a", 32)
	config := routeAuxConfig{Peer: "192.0.2.1", Entries: map[string]routeAuxEntry{key: {Maintenance: &RouteMaintenance{Enabled: true, HTML: "<h1>Maintenance marker</h1>"}, ErrorPages: map[string]string{"404": "<h1>Not found marker</h1>"}}}}
	handler, err := newRouteAuxHandler(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		peer, path string
		status     int
		marker     string
	}{{"192.0.2.1:1000", "/routes/" + key + "/maintenance/deep/path", 503, "Maintenance marker"}, {"192.0.2.1:1000", "/routes/" + key + "/error/404", 404, "Not found marker"}, {"192.0.2.2:1000", "/routes/" + key + "/maintenance", 403, "Forbidden"}, {"192.0.2.1:1000", "/routes/" + key + "/error/500", 404, "not found"}, {"127.0.0.1:1000", "/health", 204, ""}} {
		request := httptest.NewRequest("GET", test.path, nil)
		request.RemoteAddr = test.peer
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.marker) {
			t.Fatal(test, response.Code, response.Body.String())
		}
		if test.status == 503 && (response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Security-Policy") == "") {
			t.Fatal("static pages lack cache/security protections", response.Header())
		}
	}
}

func TestRouteAuxiliaryBindingIsStableAndRequiresFreshRouter(t *testing.T) {
	route := Route{Domain: "pages.example.test", Service: "web", Port: 8080, Policy: &RoutePolicy{CertificateSource: "default", Maintenance: &RouteMaintenance{Enabled: true, HTML: "<h1>maintenance</h1>"}, ForwardAuth: &RouteForwardAuth{URL: "https://auth.example.test/check"}}}
	var previous string
	for range 100 {
		config := newIngressConfig()
		config.addRoute("app", route)
		bindRouteAuxiliary(&config, "http://cove-route-aux-owned:8080", strings.Repeat("c", 64), map[string]routeAuxEntry{routeAuxKey(route): {}})
		for key := range config.HTTP.Routers {
			if strings.Count(key, "-aux-") != 1 {
				t.Fatal("helper binding repeated router suffix", key)
			}
		}
		encoded, _ := json.Marshal(config)
		if strings.Contains(string(encoded), routeAuxPlaceholder) {
			t.Fatal("unbound route helper", string(encoded))
		}
		if previous != "" && previous != string(encoded) {
			t.Fatal("same route helper changed its confirmation identity")
		}
		previous = string(encoded)
	}
}

func TestRouteExtensionPayloadMustFitWorkerProtocol(t *testing.T) {
	r := Request{ID: "oversized", Workload: "app", Action: "routes", Kind: "service"}
	for index := range 6 {
		r.Routes = append(r.Routes, Route{Domain: fmt.Sprintf("r%d.example.test", index), Service: "web", Port: 80, Policy: &RoutePolicy{ErrorPages: map[string]string{"400": strings.Repeat("<", 16384), "401": strings.Repeat("<", 16384), "402": strings.Repeat("<", 16384), "403": strings.Repeat("<", 16384)}}})
	}
	if err := ValidateRequest(r); err == nil || !strings.Contains(err.Error(), "2 MiB") {
		t.Fatal("oversized escaped pages reached the worker queue", err)
	}
}

func TestRouteAuxiliaryAuthBoundsRedirectsAndTrustedHeaders(t *testing.T) {
	key := strings.Repeat("b", 32)
	status := 302
	body := "provider redirect"
	calls := 0
	transport := routeAuthTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://auth.example.test/check" || r.Method != "GET" || r.Header.Get("Cookie") != "session=fake" || r.Header.Get("X-Forwarded-Uri") != "/private?q=1" || r.Header.Get("X-Unrelated-Credential") != "" || r.Header.Get("X-Cove-Auth-Hop") != "1" {
			t.Fatal("unexpected auth request", r.URL, r.Method, r.Header)
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Location": []string{"https://auth.example.test/login"}, "Remote-User": []string{"alice"}, "X-Unselected": []string{"not-forwarded"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	config := routeAuxConfig{Peer: "192.0.2.1", Entries: map[string]routeAuxEntry{key: {Auth: &RouteForwardAuth{URL: "https://auth.example.test/check", ResponseHeaders: []string{"Remote-User"}}}}}
	handler, err := newRouteAuxHandler(config, transport)
	if err != nil {
		t.Fatal(err)
	}
	request := func(hop string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/routes/"+key+"/auth", strings.NewReader("must not be forwarded"))
		r.RemoteAddr = "192.0.2.1:2000"
		r.Header.Set("Cookie", "session=fake")
		r.Header.Set("X-Forwarded-Uri", "/private?q=1")
		r.Header.Set("X-Unrelated-Credential", "private")
		r.Header.Set("X-Cove-Auth-Hop", hop)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	response := request("")
	if response.Code != 302 || calls != 1 || response.Header().Get("Remote-User") != "alice" || response.Header().Get("X-Unselected") != "" {
		t.Fatal(response, calls)
	}
	status = 200
	body = ""
	if response = request(""); response.Code != 200 {
		t.Fatal(response)
	}
	body = strings.Repeat("x", 32769)
	if response = request(""); response.Code != 503 {
		t.Fatal("unbounded auth response accepted", response.Code)
	}
	before := calls
	if response = request("1"); response.Code != 503 || calls != before {
		t.Fatal("recursive auth was forwarded", response.Code, calls)
	}
}

func TestPublicForwardAuthCannotDialPrivateOrReservedAddresses(t *testing.T) {
	transport := publicAuthTransport(nil)
	defer transport.CloseIdleConnections()
	for _, address := range []string{"127.0.0.1:443", "10.0.0.1:443", "100.64.0.1:443", "169.254.169.254:443", "[::1]:443", "[::ffff:127.0.0.1]:443", "[fd00::1]:443"} {
		if connection, err := transport.DialContext(context.Background(), "tcp", address); err == nil {
			connection.Close()
			t.Fatal("private authentication dial accepted", address)
		}
	}
}

func TestRouteExtensionsRejectUnsafeAndOversizedSettings(t *testing.T) {
	for _, policy := range []*RoutePolicy{{Maintenance: &RouteMaintenance{HTML: strings.Repeat("x", 16385)}}, {ErrorPages: map[string]string{"200": "bad"}}, {ErrorPages: map[string]string{"500": ""}}, {ForwardAuth: &RouteForwardAuth{URL: "http://auth.example.test/"}}, {ForwardAuth: &RouteForwardAuth{URL: "https://user:fake@auth.example.test/"}}, {ForwardAuth: &RouteForwardAuth{URL: "https://127.0.0.1/"}}, {ForwardAuth: &RouteForwardAuth{Service: "auth", Port: 8080, Path: "//other-host/path"}}, {ForwardAuth: &RouteForwardAuth{Service: "auth", Port: 8080, ResponseHeaders: []string{"X-Forwarded-For"}}}, {ForwardAuth: &RouteForwardAuth{Service: "auth", Port: 8080, ResponseHeaders: []string{"Remote-User", "remote-user"}}}} {
		if err := validateRouteExtensions(policy); err == nil {
			t.Fatal("unsafe route extension accepted", policy)
		}
	}
	if err := idleAuthEligibility([]Route{{Domain: "app.example.test", Policy: &RoutePolicy{ForwardAuth: &RouteForwardAuth{Service: "auth", Port: 9091}}}}); err == nil {
		t.Fatal("sleeping auth dependency accepted")
	}
	routes := []Route{{Domain: "app.example.test", Policy: &RoutePolicy{ForwardAuth: &RouteForwardAuth{URL: "https://auth.example.test/check"}}}}
	if err := idleAuthEligibility(routes); err != nil {
		t.Fatal(err)
	}
}
