package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func routeDNSFixture(t *testing.T) (*accountFixture, worker.Request, []byte) {
	t.Helper()
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('dns-node','Node','127.0.0.1',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('dns-project','Project','default')`)
	request := worker.Request{ID: "dns-operation", Workload: "dns-app", Action: "routes", Kind: "service", Routes: []worker.Route{{Domain: "late.example.test", Service: "web", Port: 80}}}
	config, err := f.s.sealJSON(workloadInput{Name: "App", NodeID: "dns-node", Kind: "service", Compose: "services: {}"})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO workloads(id,name,node_id,project_id,team_id,configuration) VALUES('dns-app','App','dns-node','dns-project','default',$1)`, config)
	payload, err := f.s.sealJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,$3,$4)`, request.ID, request.Workload, request.Action, payload)
	zone := cloudflareZone{ZoneID: strings.Repeat("c", 32), Domain: "example.test", Token: strings.Repeat("fixture", 8), Enabled: true, Nodes: map[string]string{"dns-node": "8.8.8.8"}}
	sealed, err := f.s.sealJSON(zone)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO cloudflare_zones(zone_id,domain,owner_id,configuration) VALUES($1,$2,'bootstrap',$3)`, zone.ZoneID, zone.Domain, sealed)
	f.sql(`INSERT INTO cloudflare_zone_teams(zone_id,team_id,domains) VALUES($1,'default',ARRAY['late.example.test'])`, zone.ZoneID)
	return f, request, payload
}

func TestRouteDNSPublishesBeforeFirstSubmission(t *testing.T) {
	f, request, payload := routeDNSFixture(t)
	published := false
	submissions := 0
	f.s.CloudflareTransport = dnsTransport(func(r *http.Request) (*http.Response, error) {
		result := any([]cloudflareRecord{})
		if r.Method != "GET" {
			published = true
			result = map[string]any{}
		}
		body, _ := json.Marshal(map[string]any{"success": true, "result": result})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if !published {
			t.Fatal("worker saw route before DNS publication")
		}
		submissions++
		return worker.RPCResponse{Operation: &worker.Operation{ID: request.ID, Workload: request.Workload, Action: request.Action, State: "queued"}}, nil
	}
	if err := f.s.processOperation(context.Background(), request.ID, "dns-node", payload); err != nil {
		t.Fatal(err)
	}
	if submissions != 1 {
		t.Fatal("operation was not submitted", submissions)
	}
	// A lost acknowledgement must not make DNS availability a reconciliation dependency.
	f.s.CloudflareTransport = dnsTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("prepared operation repeated publication")
		return nil, errors.New("unexpected")
	})
	if err := f.s.processOperation(context.Background(), request.ID, "dns-node", payload); err != nil {
		t.Fatal(err)
	}
}

func TestRouteDNSProviderOutageIsBoundedAndWarns(t *testing.T) {
	f, request, payload := routeDNSFixture(t)
	calls := 0
	submissions := 0
	lostReply := true
	f.s.CloudflareTransport = dnsTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("provider unavailable") })
	f.s.workerRPC = func(_ context.Context, _ string, _ worker.RPCRequest) (worker.RPCResponse, error) {
		submissions++
		if lostReply {
			return worker.RPCResponse{}, errors.New("injected lost acknowledgement")
		}
		return worker.RPCResponse{Operation: &worker.Operation{ID: request.ID, Workload: request.Workload, Action: request.Action, State: "queued"}}, nil
	}
	ctx := context.Background()
	now := time.Now().UTC()
	ready, err := f.s.prepareRouteDNS(ctx, "dns-node", request, now)
	if err != nil || ready {
		t.Fatal(ready, err)
	}
	ready, err = f.s.prepareRouteDNS(ctx, "dns-node", request, now.Add(time.Second))
	if err != nil || ready || calls != 1 {
		t.Fatal("backoff not kept", ready, calls, err)
	}
	ready, err = f.s.prepareRouteDNS(ctx, "dns-node", request, now.Add(routeDNSWait))
	if err != nil || !ready || calls != 1 {
		t.Fatal("outage exceeded bound", ready, calls, err)
	}
	var before []byte
	if err = f.s.DB.QueryRow(ctx, `SELECT result FROM operations WHERE id=$1`, request.ID).Scan(&before); err != nil || !strings.Contains(string(before), "dns_warning") {
		t.Fatal("fallback warning was not visible before submission", string(before), err)
	}
	if err = f.s.processOperation(ctx, request.ID, "dns-node", payload); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow(ctx, `SELECT result FROM operations WHERE id=$1`, request.ID).Scan(&before); err != nil || !strings.Contains(string(before), "dns_warning") || !strings.Contains(string(before), "hostnames remain reserved") {
		t.Fatal("ambiguous submit lost warning or route hold", string(before), err)
	}
	lostReply = false
	if err = f.s.processOperation(ctx, request.ID, "dns-node", payload); err != nil {
		t.Fatal(err)
	}
	var result []byte
	if err = f.s.DB.QueryRow(ctx, `SELECT result FROM operations WHERE id=$1`, request.ID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	if submissions != 2 || !strings.Contains(string(result), "dns_warning") || !strings.Contains(string(result), "late.example.test") {
		t.Fatal("fallback lost visible hostname warning", submissions, string(result))
	}
}

func TestRouteDNSExcludesPrivateUnsharedAndTunnelHosts(t *testing.T) {
	for _, scope := range []string{"private", "unshared", "disabled", "owner", "tunnel"} {
		t.Run(scope, func(t *testing.T) {
			f, request, _ := routeDNSFixture(t)
			switch scope {
			case "private":
				request.Routes[0].Policy = &worker.RoutePolicy{Visibility: "lan", CertificateSource: "default"}
			case "unshared":
				f.sql(`DELETE FROM cloudflare_zone_teams`)
			case "disabled":
				var raw []byte
				f.s.DB.QueryRow(context.Background(), `SELECT configuration FROM cloudflare_zones`).Scan(&raw)
				var zone cloudflareZone
				f.s.unsealJSON(raw, &zone)
				zone.Enabled = false
				sealed, _ := f.s.sealJSON(zone)
				f.sql(`UPDATE cloudflare_zones SET configuration=$1`, sealed)
			case "owner":
				f.sql(`UPDATE cloudflare_zones SET owner_id=NULL`)
			case "tunnel":
				f.sql(`INSERT INTO cloudflare_accounts(id,owner_id,name,kind,credentials) VALUES('dns-account','bootstrap','Fixture','token',$1)`, []byte{})
				f.sql(`INSERT INTO node_cloudflare_tunnels(node_id,id,zone_id,connection_id,owner_id,hostnames,image,state) SELECT 'dns-node','dns-tunnel',zone_id,'dns-account','bootstrap',ARRAY['late.example.test'],'fixture',$1 FROM cloudflare_zones`, []byte{})
			}
			f.s.CloudflareTransport = dnsTransport(func(*http.Request) (*http.Response, error) {
				t.Fatal("published excluded hostname")
				return nil, errors.New("unexpected")
			})
			ready, err := f.s.prepareRouteDNS(context.Background(), "dns-node", request, time.Now())
			if err != nil || !ready {
				t.Fatal(ready, err)
			}
		})
	}
}

func TestManagedRegistryVerificationRetryClassification(t *testing.T) {
	for _, test := range []struct {
		message  string
		retry    bool
		contains string
	}{{worker.ErrRegistryDNSPending.Error(), true, "cached negative DNS"}, {worker.ErrRegistryHTTPSPending.Error(), true, "retry automatically"}, {"registry authenticated HTTPS endpoint refused verification", false, "credentials"}} {
		message, retry := registryVerificationFailure(&RemoteError{Message: test.message})
		if retry != test.retry || !strings.Contains(message, test.contains) {
			t.Fatal(message, retry)
		}
	}
}

func TestManagedRegistryDNSVerificationRecoversAutomatically(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('registry-node','Registry','localhost',22,'',$1,'ready','{"managed_registry":true,"file_secrets":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('registry-project','Registry project','default')`)
	created := f.call("root", "default", "POST", "/api/managed-registries", managedRegistryInput{Name: "Late DNS", Node: "registry-node", Project: "registry-project", Hostname: "registry.example.test", Policy: &worker.ManagedRegistryRetention{KeepPerRepository: 5, MinimumAgeDays: 7}}, 202)
	id, operation := jsonString(t, created, "id"), jsonString(t, created, "operation_id")
	f.sql(`UPDATE operations SET state='succeeded' WHERE id=$1`, operation)
	probes := 0
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method != "managed_registry_verify" {
			t.Fatal(r.Method)
		}
		probes++
		if probes == 1 {
			return worker.RPCResponse{Error: worker.ErrRegistryDNSPending.Error()}, &RemoteError{Message: worker.ErrRegistryDNSPending.Error()}
		}
		if probes == 2 {
			return worker.RPCResponse{Error: worker.ErrRegistryHTTPSPending.Error()}, &RemoteError{Message: worker.ErrRegistryHTTPSPending.Error()}
		}
		return worker.RPCResponse{}, nil
	}
	check := func() {
		t.Helper()
		if err := f.s.reconcileManagedRegistries(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	check()
	var state, message string
	var next *time.Time
	if err := f.s.DB.QueryRow(context.Background(), `SELECT state,last_error,next_verification_at FROM managed_registries WHERE id=$1`, id).Scan(&state, &message, &next); err != nil || state != "failed" || next == nil || !strings.Contains(message, "does not resolve") {
		t.Fatal(state, message, next, err)
	}
	check()
	if probes != 1 {
		t.Fatal("verification ignored persisted backoff", probes)
	}
	f.sql(`UPDATE managed_registries SET next_verification_at=now()-interval '1 second' WHERE id=$1`, id)
	check()
	if probes != 2 {
		t.Fatal(probes)
	}
	f.sql(`UPDATE managed_registries SET next_verification_at=now()-interval '1 second' WHERE id=$1`, id)
	check()
	var connection *string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT state,connection_id,next_verification_at FROM managed_registries WHERE id=$1`, id).Scan(&state, &connection, &next); err != nil || state != "ready" || connection == nil || next != nil {
		t.Fatal(state, connection, next, err)
	}
	check()
	if probes != 3 {
		t.Fatal("ready registry was reverified", probes)
	}
}

func TestRouteCertificateRetryStatusIsScopedAndExplainsOldWorkers(t *testing.T) {
	f, request, _ := routeDNSFixture(t)
	path := "/api/workloads/" + request.Workload + "/certificate-retries"
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "dns-node" || r.ID != request.Workload || r.Method != "certificate_retries" {
			t.Fatal(node, r.Method, r.ID)
		}
		return worker.RPCResponse{}, &RemoteError{Message: "unsupported method"}
	}
	response := f.call("root", "default", "GET", path, nil, 409)
	if !strings.Contains(response.Body.String(), "Upgrade this worker") {
		t.Fatal(response.Body.String())
	}
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{}, errors.New("private transport diagnostic")
	}
	response = f.call("root", "default", "GET", path, nil, 503)
	if strings.Contains(response.Body.String(), "private transport diagnostic") {
		t.Fatal("internal transport error leaked")
	}
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{CertificateRetries: []worker.RouteCertificateRetry{{Hostname: "late.example.test", NextAttemptAt: time.Now().Add(time.Hour), Message: "pending"}}}, nil
	}
	viewer := f.user("dns-viewer", "dns-viewer@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','dns-viewer','member','[{"scope":"project","scope_id":"dns-project","role":"viewer"}]')`)
	response = f.call(viewer, "default", "GET", path, nil, 200)
	if !strings.Contains(response.Body.String(), "late.example.test") {
		t.Fatal("reader could not inspect its route status")
	}
	_, token := mintPublic(t, f, viewer, "user", "workloads:read")
	publicCall(t, f, token, "default", "GET", "/api/v1/workloads/"+request.Workload+"/certificate-retries", nil, 200)
	f.sql(`INSERT INTO teams(id,name) VALUES('dns-other','Other')`)
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		t.Fatal("foreign team reached worker")
		return worker.RPCResponse{}, nil
	}
	f.call("root", "dns-other", "GET", path, nil, 404)
}

func TestRouteDNSDoesNotGatePrivateOrRawOnlyOperations(t *testing.T) {
	server := &Server{}
	for _, route := range []worker.Route{{Domain: "private.example.test", Policy: &worker.RoutePolicy{Visibility: "lan", CertificateSource: "default"}}, {Protocol: "tcp", Policy: &worker.RoutePolicy{Visibility: "public", Listener: "raw"}}} {
		ready, err := server.prepareRouteDNS(context.Background(), "node", worker.Request{ID: "private", Action: "routes", Routes: []worker.Route{route}}, time.Now())
		if err != nil || !ready {
			t.Fatal("private or raw route required DNS access", ready, err)
		}
	}
}
