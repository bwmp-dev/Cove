package control

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"cove/internal/worker"
)

func TestCertificatesMergeHeaderRedactionPreservesAndReplacesValues(t *testing.T) {
	old := mergedProtectedRoutes()[:1]
	public := routesWithoutSecrets(old)
	if public[0].Policy.RequestHeaders["X-App"] != worker.RedactedRouteHeader || old[0].Policy.RequestHeaders["X-App"] != "protected-request-header-marker" {
		t.Fatal("header redaction mutated sealed values")
	}
	input := portableRouteInput(public, old)
	retained, err := retainedRouteAuth(input, old)
	if err != nil || !worker.RoutesEqual(retained, old) {
		t.Fatal("redacted route lost protection", err)
	}
	if _, err = retainedRouteAuth(input, nil); err == nil {
		t.Fatal("new route accepted a hidden value without a source")
	}
	public[0].Policy.RequestHeaders["X-App"] = ""
	public[0].Policy.ResponseHeaders["X-Version"] = "replacement"
	retained, err = retainedRouteAuth(portableRouteInput(public, old), old)
	if err != nil || retained[0].Policy.RequestHeaders["X-App"] != "" || retained[0].Policy.ResponseHeaders["X-Version"] != "replacement" {
		t.Fatal("explicit replacement/removal was ignored", err)
	}
	var clearing routeInput
	if err = json.Unmarshal([]byte(`{"domain":"app.example.test","service":"web","port":8080,"policy":{"path_prefix":"/api","request_headers":{},"response_headers":null}}`), &clearing); err != nil {
		t.Fatal(err)
	}
	retained, err = retainedRouteAuth([]routeInput{clearing}, old)
	if err != nil || len(retained[0].Policy.RequestHeaders) != 0 || len(retained[0].Policy.ResponseHeaders) != 0 {
		t.Fatal("explicit empty header maps were retained", err)
	}
	second := old[0]
	policy := *second.Policy
	policy.PathPrefix = "/other"
	policy.RequestHeaders = map[string]string{"X-App": "different-secret"}
	second.Policy = &policy
	public = routesWithoutSecrets(old)
	public[0].Policy.PathPrefix = "/new"
	if _, err = retainedRouteAuth(portableRouteInput(public, old), append(old, second)); err == nil {
		t.Fatal("ambiguous path retained the wrong hidden header")
	}
}

func TestCertificatesMergeCanonicalWildcardReferencesAndNullableRoutes(t *testing.T) {
	config := workloadInput{Name: "Wildcard", NodeID: "node", Kind: "service", Compose: "services: {}", Routes: []worker.Route{{Domain: "*.example.test", Service: "web", Port: 80, Policy: &worker.RoutePolicy{CertificateSource: "uploaded", CertificateID: strings.Repeat("a", 32), RequestHeaders: map[string]string{"X-Key": "private-header"}}}}}
	manifest := projectConfiguration{APIVersion: "cove/v1", Project: "Project", Resources: []configurationResource{{Key: "app", Configuration: config}}}
	public := publicProjectConfiguration(manifest, false)
	encoded, err := json.Marshal(public)
	if err != nil || strings.Contains(string(encoded), "private-header") || !strings.Contains(string(encoded), strings.Repeat("a", 32)) {
		t.Fatal("canonical export lost metadata or exposed headers", err)
	}
	parsed, err := parseProjectConfiguration(string(encoded))
	if err != nil {
		t.Fatal("wildcard canonical schema no longer parses", err)
	}
	merged, err := mergeManifestResource(parsed.Resources[0].Configuration, config)
	if err != nil || !worker.RoutesEqual(merged.Routes, config.Routes) {
		t.Fatal("wildcard declaration lost route policy", err)
	}
	parsed.Resources[0].Configuration.Routes[0].Policy.RequestHeaders = map[string]string{}
	merged, err = mergeManifestResource(parsed.Resources[0].Configuration, config)
	if err != nil || len(merged.Routes[0].Policy.RequestHeaders) != 0 {
		t.Fatal("explicit empty canonical header map was ignored", err)
	}
	config.Routes = nil
	manifest.Resources[0].Configuration = config
	encoded, _ = json.Marshal(publicProjectConfiguration(manifest, false))
	parsed, err = parseProjectConfiguration(string(encoded))
	if err != nil || parsed.Resources[0].Configuration.Routes != nil {
		t.Fatal("new-resource nil routes changed during canonical round trip", err)
	}
}

func TestCertificatesMergeScopedWritesRevalidatePermissionsAndPolicy(t *testing.T) {
	f, id, cookie := publicFixture(t)
	_, token := mintPublic(t, f, cookie, "user", "workloads:read", "certificates:write")
	base := "/api/v1/workloads/" + id
	certificate, key := certificateFixture(t, "app.example.test")
	input := routeCertificateInput{Name: "Scoped certificate", CertificatePEM: certificate, PrivateKeyPEM: key}
	f.sql(`INSERT INTO team_roles(team_id,id,name,permissions) VALUES('default','certificate-writer','Certificate writer',ARRAY['view','edit_configuration'])`)
	f.sql(`UPDATE team_members SET role='member',grants='[{"scope":"environment","scope_id":"variable-env","role":"certificate-writer"}]' WHERE user_id='public-owner'`)
	result := publicCall(t, f, token, "default", "POST", base+"/certificates", input, 201)
	if strings.Contains(result.Body.String(), "PRIVATE KEY") || strings.Contains(result.Body.String(), "private_key_pem") {
		t.Fatal("certificate write returned private material")
	}
	publicCall(t, f, token, "default", "GET", base+"/certificates", nil, 200)
	f.sql(`UPDATE team_roles SET permissions=ARRAY['view'] WHERE id='certificate-writer'`)
	publicCall(t, f, token, "default", "POST", base+"/certificates", input, 403)
	f.sql(`UPDATE team_roles SET permissions=ARRAY['view','edit_configuration'] WHERE id='certificate-writer'`)
	f.sql(`UPDATE teams SET require_second_factor=true WHERE id='default'`)
	publicCall(t, f, token, "default", "GET", base+"/certificates", nil, 403)
	f.sql(`UPDATE teams SET require_second_factor=false WHERE id='default'`)
	f.sql(`UPDATE control_settings SET panel_allowlist=ARRAY['127.0.0.1/32']`)
	publicCall(t, f, token, "default", "GET", base+"/certificates", nil, 403)
}

func TestCertificatesMergeOpenAPIAndTLSValidation(t *testing.T) {
	doc := (&Server{}).openAPIDocument()
	paths := doc["paths"].(map[string]any)
	post := paths["/workloads/{id}/certificates"].(map[string]any)["post"].(map[string]any)
	if post["x-cove-scope"] != "certificates:write" {
		t.Fatal("wrong certificate scope")
	}
	properties := apiSchema(reflect.TypeFor[routeCertificateInput]())["properties"].(map[string]any)
	if properties["private_key_pem"].(map[string]any)["writeOnly"] != true {
		t.Fatal("private key is not write-only")
	}
	input := databaseTestCertificate(t, "data.example.test", time.Now().Add(48*time.Hour))
	if _, _, err := validateDatabaseCertificate(input); err != nil {
		t.Fatal("native database trust anchor rejected", err)
	}
	if _, err := worker.ParseRouteCertificate(worker.RouteCertificate{ID: strings.Repeat("a", 32), CertificatePEM: input.Certificate, PrivateKeyPEM: input.PrivateKey}, time.Now()); err == nil {
		t.Fatal("route SNI store accepted a CA certificate")
	}
	input.PrivateKey += "\nextra-data"
	if _, _, err := validateDatabaseCertificate(input); err == nil {
		t.Fatal("database accepted trailing private key data")
	}
}

func TestCertificatesMergeWildcardUptimeUsesAnExactHost(t *testing.T) {
	f, id := variablesFixture(t)
	config := workloadInput{Name: "Wildcard", NodeID: "variable-node", Compose: "services: {}", Kind: "service", Routes: []worker.Route{{Domain: "*.example.test", Service: "web", Port: 80}}}
	protected, err := f.s.sealJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE workloads SET configuration=$2 WHERE id=$1`, id, protected)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"builtin_variables":true}' WHERE id='variable-node'`)
	request := worker.Request{Workload: id, Routes: config.Routes}
	if err = f.s.resolveDeploymentVariables(t.Context(), &request); err != nil || request.BuiltinDefaults[worker.CoveURL] != "" {
		t.Fatal("wildcard invented an application URL", err)
	}
	request.Routes = append(request.Routes, worker.Route{Domain: "app.example.test"})
	if err = f.s.resolveDeploymentVariables(t.Context(), &request); err != nil || request.BuiltinDefaults[worker.CoveURL] != "https://app.example.test" {
		t.Fatal("exact route URL unavailable", err)
	}
	base := "/api/workloads/" + id + "/uptime"
	check := uptimeCheck{Domain: "app.example.test", Path: "/health", Enabled: true, Interval: 60, Timeout: 2, ExpectedMin: 200, ExpectedMax: 299, PublicLabel: "Application"}
	f.call("root", "default", "POST", base, check, 201)
	for _, domain := range []string{"*.example.test", "example.test", "deep.app.example.test", "foreign.test"} {
		check.Domain = domain
		f.call("root", "default", "POST", base, check, 400)
	}
	calls := 0
	f.s.NotificationTransport = notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://app.example.test/health" {
			t.Fatal("wildcard used as a probe destination", r.URL)
		}
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if err = f.s.collectUptime(t.Context()); err != nil || calls != 1 {
		t.Fatal("concrete wildcard check did not run", calls, err)
	}
}
