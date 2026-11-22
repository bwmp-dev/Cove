package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ingressWorker answers custom proxy configuration RPCs on the local worker socket and records
// what the control plane forwarded. Worker-side behavior has its own tests.
type ingressWorker struct {
	mu       sync.Mutex
	requests []worker.RPCRequest
	reply    func(worker.RPCRequest) worker.RPCResponse
}

func (w *ingressWorker) forwarded() []worker.RPCRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]worker.RPCRequest(nil), w.requests...)
}

func startIngressWorker(t *testing.T, reply func(worker.RPCRequest) worker.RPCResponse) (*ingressWorker, string) {
	t.Helper()
	w := &ingressWorker{reply: reply}
	socket := filepath.Join(t.TempDir(), "worker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skip("Unix sockets unavailable:", err)
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var request worker.RPCRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(rw, "invalid request", 400)
			return
		}
		w.mu.Lock()
		w.requests = append(w.requests, request)
		reply := w.reply
		w.mu.Unlock()
		_ = json.NewEncoder(rw).Encode(reply(request))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Close() })
	return w, socket
}

func TestIngressConfigAPIIntegration(t *testing.T) {
	f := newAccountFixture(t)
	admin := f.user("operator-admin", "admin@example.com")
	f.sql("UPDATE users SET is_admin=true WHERE id='operator-admin'")
	owner := f.user("owner", "owner@example.com")
	f.sql("INSERT INTO teams(id,name) VALUES('a','Team A')")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('a','owner','owner','[]')`)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,mode,capabilities) VALUES('local','Local','local',22,'',$1,'ready','local','{}')`, []byte{})
	f.sql("INSERT INTO team_nodes(team_id,node_id) VALUES('a','local')")
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('pa','Project','a')")
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,kind,team_id,project_id) VALUES('app','local','app',$1,'service','a','pa')", []byte{})
	secret := "users: [\"admin:$apr1$secret-hash\"]"
	stale := strings.Repeat("0", 64)
	runner, socket := startIngressWorker(t, func(request worker.RPCRequest) worker.RPCResponse {
		switch request.Method {
		case "ingress_config":
			return worker.RPCResponse{Ingress: &worker.IngressConfigState{Available: true, Running: true, Files: []worker.IngressConfigFile{{Name: "legacy-import.yaml", Status: "unmanaged"}}}}
		case "ingress_config_file":
			if request.Ingress.Name == "missing.yaml" {
				return worker.RPCResponse{Error: worker.IngressFileMissing}
			}
			content := secret
			return worker.RPCResponse{IngressFile: &worker.IngressConfigFile{Name: request.Ingress.Name, Status: "managed", Content: &content}}
		case "ingress_config_change":
			codes := map[string]string{"conflict.yaml": "conflict", "blocked.yaml": "blocked", "invalid.yaml": "invalid", "gone.yaml": "not_found", "stopped.yaml": "unavailable", "broken.yaml": ""}
			if code, ok := codes[request.Ingress.Name]; ok || request.Ingress.Revision == stale {
				if request.Ingress.Revision == stale {
					code = "conflict"
				}
				return worker.RPCResponse{IngressResult: &worker.IngressConfigResult{Code: code, Error: "refused by worker"}}
			}
			return worker.RPCResponse{IngressResult: &worker.IngressConfigResult{Change: &worker.IngressConfigChange{ID: request.Ingress.ChangeID, Action: request.Ingress.Action, Name: request.Ingress.Name, State: "succeeded"}}}
		}
		return worker.RPCResponse{Error: "unsupported method"}
	})
	f.s.LocalSocket = socket
	put := map[string]any{"content": secret, "revision": "", "change_id": "change-1"}

	// The shared proxy routes every team's traffic, so only instance administrators may use it.
	for _, path := range []string{"/api/nodes/local/ingress-config", "/api/nodes/local/ingress-config/files/legacy-import.yaml"} {
		f.call(owner, "a", "GET", path, nil, 403)
	}
	f.call(owner, "a", "PUT", "/api/nodes/local/ingress-config/files/site.yaml", put, 403)
	f.call(owner, "a", "POST", "/api/nodes/local/ingress-config/files/legacy-import.yaml/import", map[string]any{"revision": "", "change_id": "c"}, 403)
	f.call(owner, "a", "DELETE", "/api/nodes/local/ingress-config/files/site.yaml", map[string]any{"revision": "", "change_id": "c"}, 403)
	f.call(owner, "a", "POST", "/api/nodes/local/ingress-config/validate", map[string]any{"name": "site.yaml", "content": "{}"}, 403)
	issued := f.call(owner, "a", "POST", "/api/tokens", map[string]any{"name": "deploy", "workload_id": "app", "permission": "operate", "days": 1}, 201)
	request := httptest.NewRequest("GET", "/api/nodes/local/ingress-config", nil)
	request.Header.Set("Authorization", "Bearer "+jsonString(t, issued, "token"))
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatalf("workload token read proxy configuration: %d", response.Code)
	}
	if forwarded := runner.forwarded(); len(forwarded) != 0 {
		t.Fatalf("unauthorized requests reached the worker: %+v", forwarded)
	}

	state := f.call(admin, "a", "GET", "/api/nodes/local/ingress-config", nil, 200)
	if state.Header().Get("Cache-Control") != "no-store" || !strings.Contains(state.Body.String(), "legacy-import.yaml") {
		t.Fatalf("state response wrong: %v %s", state.Header(), state.Body.String())
	}
	file := f.call(admin, "a", "GET", "/api/nodes/local/ingress-config/files/legacy-import.yaml", nil, 200)
	if file.Header().Get("Cache-Control") != "no-store" || !strings.Contains(file.Body.String(), "secret-hash") {
		t.Fatalf("file response wrong: %v", file.Header())
	}
	f.call(admin, "a", "GET", "/api/nodes/local/ingress-config/files/missing.yaml", nil, 404)
	f.call(admin, "a", "GET", "/api/nodes/local/ingress-config/files/Upper.yaml", nil, 400)
	f.call(admin, "a", "GET", "/api/nodes/absent/ingress-config", nil, 404)

	result := f.call("root", "", "PUT", "/api/nodes/local/ingress-config/files/site.yaml", put, 200)
	if jsonString(t, result, "state") != "succeeded" {
		t.Fatal("change result not returned:", result.Body.String())
	}
	forwarded := runner.forwarded()
	last := forwarded[len(forwarded)-1].Ingress
	if last.Action != "put" || last.Name != "site.yaml" || last.ChangeID != "change-1" || last.Content == nil || *last.Content != secret {
		t.Fatalf("change not forwarded exactly: %+v", last)
	}
	f.call(admin, "a", "POST", "/api/nodes/local/ingress-config/files/legacy-import.yaml/import", map[string]any{"revision": strings.Repeat("a", 64), "change_id": "change-2"}, 200)
	f.call(admin, "a", "DELETE", "/api/nodes/local/ingress-config/files/site.yaml", map[string]any{"revision": strings.Repeat("b", 64), "change_id": "change-3"}, 200)
	forwarded = runner.forwarded()
	if imported, removed := forwarded[len(forwarded)-2].Ingress, forwarded[len(forwarded)-1].Ingress; imported.Action != "import" || imported.Content != nil || removed.Action != "delete" || removed.Revision != strings.Repeat("b", 64) {
		t.Fatalf("revision changes forwarded incorrectly: %+v %+v", imported, removed)
	}
	if validated := f.call(admin, "a", "POST", "/api/nodes/local/ingress-config/validate", map[string]any{"name": "site.yaml", "content": "{}"}, 200); jsonString(t, validated, "action") != "validate" {
		t.Fatal("validation not forwarded:", validated.Body.String())
	}

	for name, status := range map[string]int{"conflict.yaml": 409, "blocked.yaml": 409, "invalid.yaml": 400, "gone.yaml": 404, "stopped.yaml": 409, "broken.yaml": 502} {
		f.call(admin, "a", "PUT", "/api/nodes/local/ingress-config/files/"+name, put, status)
	}
	f.call(admin, "a", "PUT", "/api/nodes/local/ingress-config/files/site.yaml", map[string]any{"content": "{}", "revision": stale, "change_id": "change-4"}, 409)
	before := len(runner.forwarded())
	f.call(admin, "a", "PUT", "/api/nodes/local/ingress-config/files/site.yaml", map[string]any{"content": strings.Repeat("a", worker.MaxCustomIngressBytes+1), "revision": "", "change_id": "change-5"}, 400)
	f.call(admin, "a", "PUT", "/api/nodes/local/ingress-config/files/site.yaml", map[string]any{"content": "{}", "unexpected": true}, 400)
	if len(runner.forwarded()) != before {
		t.Fatal("invalid input reached the worker")
	}

	runner.mu.Lock()
	runner.reply = func(worker.RPCRequest) worker.RPCResponse { return worker.RPCResponse{Error: "unsupported method"} }
	runner.mu.Unlock()
	if body := f.call(admin, "a", "GET", "/api/nodes/local/ingress-config", nil, 409).Body.String(); !strings.Contains(body, "upgrade the worker") {
		t.Fatal("older workers not reported clearly:", body)
	}

	// Audit records the action without file contents.
	var audited, leaked int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FILTER (WHERE action='PUT /api/nodes/{id}/ingress-config/files/{name}'), count(*) FILTER (WHERE actor||action||subject LIKE '%secret-hash%') FROM audit_events").Scan(&audited, &leaked); err != nil {
		t.Fatal(err)
	}
	if audited == 0 || leaked != 0 {
		t.Fatalf("audit wrong: %d changes recorded, %d leaked contents", audited, leaked)
	}
}
