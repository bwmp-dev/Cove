package control

import (
	"bytes"
	"context"
	"cove/internal/outbound"
	"cove/internal/worker"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func listenerMergeFixture(t *testing.T) (*accountFixture, string) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('listener-node','Node','localhost',22,'',$1,'ready','{"route_protocol":6,"ingress_exposures":true,"workload_moves":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('listener-project','Project','default')`)
	id := "listener-resource"
	data, _ := f.s.sealJSON(workloadInput{Name: "Resource", Kind: "service", NodeID: "listener-node", ProjectID: "listener-project", Compose: "services: {}", Routes: []worker.Route{{Domain: "app.example.test", Service: "app", Port: 80}}})
	f.sql(`INSERT INTO workloads(id,name,node_id,project_id,team_id,configuration) VALUES($1,'Resource','listener-node','listener-project','default',$2)`, id, data)
	return f, id
}

func TestListenerMergePrivateUptimeCannotBeProbedOrPublished(t *testing.T) {
	f, id := listenerMergeFixture(t)
	check := uptimeCheck{Domain: "app.example.test", Path: "/", Enabled: true, Interval: 300, Timeout: 2, ExpectedMin: 200, ExpectedMax: 299, PublicLabel: "Internal-only label"}
	checkID := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/uptime", check, 201), "id")
	page := f.call("root", "default", "PUT", "/api/projects/listener-project/status-page", map[string]any{"enabled": true, "title": "Status"}, 200)
	slug := jsonString(t, page, "slug")
	if !strings.Contains(f.call("", "", "GET", "/api/status/"+slug, nil, 200).Body.String(), check.PublicLabel) {
		t.Fatal("public fixture check missing")
	}
	data, _ := f.s.sealJSON(workloadInput{Kind: "service", Routes: []worker.Route{{Domain: check.Domain, Service: "app", Port: 80, Policy: &worker.RoutePolicy{Visibility: "lan", Listener: "office", CertificateSource: "default"}}}})
	f.sql(`UPDATE workloads SET configuration=$2 WHERE id=$1`, id, data)
	f.call("root", "default", "POST", "/api/workloads/"+id+"/uptime", check, 400)
	body := f.call("", "", "GET", "/api/status/"+slug, nil, 200).Body.String()
	if strings.Contains(body, check.PublicLabel) || strings.Contains(body, check.Domain) {
		t.Fatal("private check disclosed through status page", body)
	}
	f.s.NotificationTransport = notificationRoundTrip(func(*http.Request) (*http.Response, error) { t.Fatal("private hostname was probed"); return nil, nil })
	f.sql(`UPDATE route_checks SET next_check_at=now()-interval '1 minute' WHERE id=$1`, checkID)
	if err := f.s.collectUptime(context.Background()); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	if err := f.s.DB.QueryRow(t.Context(), `SELECT enabled FROM route_checks WHERE id=$1`, checkID).Scan(&enabled); err != nil || enabled {
		t.Fatal("private check still scheduled", enabled, err)
	}
}

func TestListenerMergeCertificateOwnersAndPrivateNames(t *testing.T) {
	f, id := listenerMergeFixture(t)
	zone := strings.Repeat("a", 32)
	sealed, _ := f.s.sealJSON(cloudflareZone{ZoneID: zone, Domain: "example.test", Enabled: true, Token: strings.Repeat("fixture", 6)})
	f.sql(`INSERT INTO cloudflare_zones(zone_id,domain,owner_id,configuration) VALUES($1,'example.test','bootstrap',$2)`, zone, sealed)
	f.sql(`INSERT INTO cloudflare_zone_teams(zone_id,team_id,domains) VALUES($1,'default',ARRAY['*.example.test'])`, zone)
	credentials, _ := f.s.sealJSON(cloudflareCredentials{Token: strings.Repeat("f", 32)})
	f.sql(`INSERT INTO cloudflare_accounts(id,owner_id,name,kind,credentials) VALUES('listener-account','bootstrap','Account','token',$1)`, credentials)
	verify := func(allowed bool) {
		t.Helper()
		tx, err := f.s.DB.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		_, err = f.s.issuerZone(t.Context(), tx, id, zone, []string{"app.example.test"})
		if (err == nil) != allowed {
			t.Fatal("issuer grant/privacy", err, allowed)
		}
	}
	verify(true)
	data, _ := f.s.sealJSON(workloadInput{Routes: []worker.Route{{Domain: "app.example.test", Service: "app", Port: 80, Policy: &worker.RoutePolicy{Visibility: "tailscale", Listener: "tail", CertificateSource: "default"}}}})
	f.sql(`UPDATE workloads SET configuration=$2 WHERE id=$1`, id, data)
	verify(false)
	f.sql(`UPDATE users SET disabled=true WHERE id='bootstrap'`)
	verify(false)
	if token, err := f.s.cloudflareConnectionToken(t.Context(), "listener-account"); err == nil || token != "" {
		t.Fatal("deprovisioned connection owner still issued credentials")
	}
}

func TestListenerMergeCertificateFailuresUseChannelCatalogue(t *testing.T) {
	f, id := listenerMergeFixture(t)
	f.call("root", "default", "POST", "/api/notifications", connectionConfig{Name: "Certificate outcomes", Notification: &notificationConfig{Provider: "webhook", URL: "https://notify.example.test/events", Enabled: true, Events: []string{"certificate.failed"}}}, 201)
	zone := strings.Repeat("a", 32)
	sealed, _ := f.s.sealJSON(cloudflareZone{ZoneID: zone, Domain: "example.test", Enabled: true})
	f.sql(`INSERT INTO cloudflare_zones(zone_id,domain,owner_id,configuration) VALUES($1,'example.test','bootstrap',$2)`, zone, sealed)
	f.sql(`INSERT INTO route_certificate_issuers(id,workload_id,zone_id,name,provider,domains,validity_days,auto_renew,state) VALUES('listener-issuer',$1,$2,'Issuer','cloudflare_origin',ARRAY['secret.example.test'],7,true,$3)`, id, zone, []byte{})
	f.sql(`UPDATE route_certificate_issuers SET phase='failed',last_error='private failure details' WHERE id='listener-issuer'`)
	var payload []byte
	var count int
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM health_deliveries WHERE event='certificate.failed'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("certificate failure not routed", count, err)
	}
	if err := f.s.DB.QueryRow(t.Context(), `SELECT message FROM health_deliveries WHERE event='certificate.failed'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte("secret.example.test")) || bytes.Contains(payload, []byte("private failure details")) {
		t.Fatal("private certificate details leaked")
	}
	f.sql(`UPDATE route_certificate_issuers SET phase='failed' WHERE id='listener-issuer'`)
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM health_deliveries WHERE event='certificate.failed'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("unchanged failure duplicated", count, err)
	}
}

func TestListenerMergeACMEUsesSelectedOutboundTransport(t *testing.T) {
	s := &Server{outboundState: &outboundProxyState{}}
	// A local CONNECT rejection proves that the selected panel proxy is used before any authority dial.
	hits := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Method != "CONNECT" || r.Host != "authority.example.test:443" {
			t.Fatal(r.Method, r.Host)
		}
		w.WriteHeader(502)
	}))
	defer proxy.Close()
	config := outbound.Config{HTTPS: proxy.URL}
	s.outboundState.value.Store(&config)
	key, err := newCertificateAccountKey()
	if err != nil {
		t.Fatal(err)
	}
	client, err := newCertificateACMEClient("https://authority.example.test/directory", "", key, "", s.OutboundTransport)
	if err != nil {
		t.Fatal(err)
	}
	defer client.HTTPClient.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err = client.Discover(ctx)
	if err == nil || hits != 1 {
		t.Fatal("ACME bypassed selected proxy", err, hits)
	}
}

func TestListenerMergeScopedAdminAndCurrentAccountPolicy(t *testing.T) {
	f, _, cookie := publicFixture(t)
	f.call(cookie, "default", "POST", "/api/access-tokens", map[string]any{"name": "Denied node scope", "scope": "user", "scopes": []string{"nodes:manage"}, "days": 1}, 403)
	f.sql(`UPDATE users SET is_admin=true WHERE id='public-owner'`)
	f.call(cookie, "default", "POST", "/api/access-tokens", map[string]any{"name": "Denied team scope", "scope": "team", "scopes": []string{"nodes:manage"}, "days": 1}, 403)
	_, token := mintPublic(t, f, cookie, "user", "nodes:manage")
	calls := 0
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		calls++
		return worker.RPCResponse{Exposures: &worker.IngressExposureStatus{}}, nil
	}
	path := "/api/v1/nodes/variable-node/ingress-listeners"
	publicCall(t, f, token, "default", "GET", path, nil, 200)
	f.sql(`UPDATE users SET is_admin=false WHERE id='public-owner'`)
	publicCall(t, f, token, "default", "GET", path, nil, 403)
	f.sql(`UPDATE users SET is_admin=true WHERE id='public-owner'`)
	f.sql(`UPDATE control_settings SET require_second_factor=true WHERE id=1`)
	publicCall(t, f, token, "default", "GET", path, nil, 403)
	f.sql(`UPDATE control_settings SET require_second_factor=false WHERE id=1`)
	f.sql(`UPDATE users SET disabled=true WHERE id='public-owner'`)
	publicCall(t, f, token, "default", "GET", path, nil, 401)
	if calls != 1 {
		t.Fatal("revoked token reached listener worker", calls)
	}
}

func TestListenerMergeMoveReservesDestinationAndValidatesListeners(t *testing.T) {
	f, _ := moveFixture(t, "queued")
	route := worker.Route{Protocol: "tcp", Service: "app", Port: 9000, Policy: &worker.RoutePolicy{Listener: "socket", Visibility: "public"}}
	config, _ := f.s.sealJSON(workloadInput{Kind: "service", Routes: []worker.Route{route}})
	f.sql(`UPDATE workloads SET configuration=$1 WHERE id='resource'`, config)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"route_protocol":6,"ingress_exposures":true}'::jsonb WHERE id='target'`)
	settings := worker.IngressExposureSettings{Listeners: []worker.IngressListener{{ID: "socket", Protocol: "tcp", Visibility: "public", Bindings: []worker.IngressBinding{{Address: "127.0.0.1", Port: 19000}}}}}
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "target" || r.Method != "ingress_exposures" {
			t.Fatal(node, r.Method)
		}
		return worker.RPCResponse{Exposures: &worker.IngressExposureStatus{Settings: settings}}, nil
	}
	digest, err := f.s.moveRouteDestination(t.Context(), f.s.DB, "target", []worker.Route{route})
	if err != nil || len(digest) != 64 {
		t.Fatal("valid destination not reviewed", digest, err)
	}
	tx, err := f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = f.s.transportListenerClaims(t.Context(), tx, "another", "target", []worker.Route{route}); err == nil {
		t.Fatal("pending move failed to reserve raw listener")
	}
	if err = f.s.checkIngressListenerClaims(t.Context(), tx, "target", worker.IngressExposureSettings{}); err == nil {
		t.Fatal("pending move allowed target listener removal")
	}
	if err = f.s.checkIngressListenerClaims(t.Context(), tx, "target", settings); err != nil {
		t.Fatal(err)
	}
	tx.Rollback(t.Context())
	settings.Listeners = nil
	if _, err = f.s.moveRouteDestination(t.Context(), f.s.DB, "target", []worker.Route{route}); err == nil {
		t.Fatal("move accepted a missing destination listener")
	}
	f.sql(`UPDATE operations SET state='failed' WHERE id='move-operation'`)
	tx, err = f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = f.s.transportListenerClaims(t.Context(), tx, "another", "target", []worker.Route{route}); err != nil {
		t.Fatal("failed move retained reservation", err)
	}
}

func TestListenerMergeCanonicalTransportAndPrivateRoundTrip(t *testing.T) {
	routes := append(mergedProtectedRoutes(), worker.Route{Domain: "office.example.test", Protocol: "http", Service: "web", Port: 8080, Policy: &worker.RoutePolicy{Visibility: "lan", Listener: "office", CertificateSource: "uploaded", CertificateID: strings.Repeat("a", 32), Timeouts: &worker.RouteTimeouts{ResponseHeaderSeconds: 17}}}, worker.Route{Protocol: "tcp", Service: "web", Port: 9000, Policy: &worker.RoutePolicy{Visibility: "public", Listener: "socket", IPAllowList: []string{"192.0.2.0/24"}}}, worker.Route{Protocol: "udp", Service: "web", Port: 9001, Policy: &worker.RoutePolicy{Visibility: "lan", Listener: "datagrams"}})
	config := workloadInput{Name: "App", NodeID: "node", Kind: "service", Compose: "services: {}", Routes: routes}
	manifest := projectConfiguration{APIVersion: "cove/v1", Project: "Project", Resources: []configurationResource{{Key: "app", Configuration: config}}}
	raw, err := json.Marshal(publicProjectConfiguration(manifest, false))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseProjectConfiguration(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := mergeManifestResource(parsed.Resources[0].Configuration, config)
	if err != nil || !worker.RoutesEqual(merged.Routes, routes) {
		t.Fatal("canonical export/import lost transport, visibility, timeout or certificate metadata", err)
	}
	for _, secret := range []string{"protected-", "PRIVATE KEY", "CERTIFICATE REQUEST", "private_key_pem", "acme_state"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("portable state exposes route secret", secret)
		}
	}
	var oldWriter routeInput
	if err = json.Unmarshal([]byte(`{"domain":"office.example.test","service":"web","port":8080}`), &oldWriter); err != nil {
		t.Fatal(err)
	}
	retained, err := retainedRouteAuth([]routeInput{oldWriter}, routes)
	if err != nil || !worker.RoutesEqual(retained, routes[2:3]) {
		t.Fatal("older writer dropped private listener or certificate", err)
	}
	doc := (&Server{}).openAPIDocument()
	paths := doc["paths"].(map[string]any)
	if paths["/nodes/{id}/ingress-listeners"].(map[string]any)["put"].(map[string]any)["x-cove-scope"] != "nodes:manage" {
		t.Fatal("listener change scope missing")
	}
	schema := apiSchema(reflect.TypeFor[worker.Route]())
	encoded, _ := json.Marshal(schema)
	for _, field := range []string{"protocol", "visibility", "listener", "timeouts", "certificate_id"} {
		if !bytes.Contains(encoded, []byte(field)) {
			t.Fatal("OpenAPI route field missing", field)
		}
	}
}
