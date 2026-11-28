package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestProtectedRoutesPersistWithoutDowngradeFailOpen(t *testing.T) {
	route := Route{Domain: "app.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "default", ForwardAuth: &RouteForwardAuth{URL: "https://auth.example.test/check"}}}
	for _, value := range []any{deployedState{Routes: []Route{route}}, journal{Request: Request{Routes: []Route{route}}, RouteChange: &routeChange{Previous: []Route{route}}}, backupManifest{State: deployedState{Routes: []Route{route}}}, map[string][]Route{"app": {route}}} {
		data, err := marshalWorkerState(value)
		if err != nil || !bytes.Contains(data, []byte(protectedRouteSource)) {
			t.Fatal(string(data), err)
		}
		var document any
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		var policies []json.RawMessage
		var collect func(any)
		collect = func(value any) {
			switch node := value.(type) {
			case []any:
				for _, child := range node {
					collect(child)
				}
			case map[string]any:
				if policy, ok := node["policy"]; ok {
					encoded, _ := json.Marshal(policy)
					policies = append(policies, encoded)
				}
				for _, child := range node {
					collect(child)
				}
			}
		}
		collect(document)
		for _, encoded := range policies {
			var legacy struct {
				CertificateSource string `json:"certificate_source"`
			}
			if err := json.Unmarshal(encoded, &legacy); err != nil {
				t.Fatal(err)
			}
			if err := (Route{Policy: &RoutePolicy{CertificateSource: legacy.CertificateSource}}).validatePolicy(); err == nil {
				t.Fatal("old route decoder accepted protected on-disk policy")
			}
			var current RoutePolicy
			if err := json.Unmarshal(encoded, &current); err != nil || !RoutesEqual([]Route{route}, []Route{{Domain: route.Domain, Service: route.Service, Port: route.Port, Policy: &current}}) {
				t.Fatal(current, err)
			}
		}
	}
	plain, _ := json.Marshal(route)
	if bytes.Contains(plain, []byte(protectedRouteSource)) {
		t.Fatal("durable metadata leaked into API")
	}
	legacy := deployedState{Routes: []Route{{Domain: "legacy.example.test", Service: "app", Port: 80}}}
	want, _ := json.Marshal(legacy)
	got, err := marshalWorkerState(legacy)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("legacy persistence changed", string(got), err)
	}
	compose := map[string]any{"domain": "user-data", "service": "user-data", "port": 80, "policy": map[string]any{"forward_auth": "user-data"}}
	want, _ = json.Marshal(compose)
	got, err = marshalWorkerState(compose)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("Compose user data rewritten", err)
	}
}

func TestProtectedRouteOperationReplayAfterRestart(t *testing.T) {
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := Request{ID: "protected-replay", Workload: "app", Action: "deploy", Compose: "services:\n  app:\n    image: busybox:1.37\n", Routes: []Route{{Domain: "app.example.test", Service: "app", Port: 80, Policy: &RoutePolicy{ForwardAuth: &RouteForwardAuth{URL: "https://auth.example.test/check"}}}}}
	if response := engine.RPC(context.Background(), RPCRequest{Method: "submit", Request: &request}); response.Error == "" {
		t.Fatal("unversioned protected request accepted")
	}
	if response := engine.RPC(context.Background(), RPCRequest{Method: "submit_routes_v2", Request: &request}); response.Error != "" || response.Operation == nil {
		t.Fatal(response)
	}
	data, err := os.ReadFile(engine.path(request.ID))
	if err != nil || !bytes.Contains(data, []byte(protectedRouteSource)) {
		t.Fatal(string(data), err)
	}
	engine, err = NewEngine(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Submit(request); err != nil {
		t.Fatal("immutable replay changed after durable decode", err)
	}
	journal, err := engine.read(request.ID)
	if err != nil || !RoutesEqual(journal.Request.Routes, request.Routes) {
		t.Fatal(journal, err)
	}
	if err := writeJSON(filepath.Join(engine.Root, "ingress.json"), map[string][]Route{"app": request.Routes}); err != nil {
		t.Fatal(err)
	}
	loaded, err := engine.loadIngress()
	if err != nil || !RoutesEqual(loaded["app"], request.Routes) {
		t.Fatal(loaded, err)
	}
}

func TestRoutePolicyDecoderRejectsUnknownAndBrokenProtection(t *testing.T) {
	for _, data := range []string{`{"forward_auth":{"url":"https://auth.example.test"},"typo":true}`, `{"certificate_source":"cove-protected-route-v2"}`, `{"cove_route_protocol":2}`, `{"certificate_source":"cove-protected-route-v2","cove_route_protocol":2,"cove_certificate_source":"acme"}`} {
		var policy RoutePolicy
		if err := json.Unmarshal([]byte(data), &policy); err == nil {
			t.Fatal("invalid policy accepted", data)
		}
	}
}
