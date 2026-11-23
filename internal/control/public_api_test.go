package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cove/internal/worker"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func publicCall(t *testing.T, f *accountFixture, token, team, method, path string, input any, status int) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Cove-Team", team)
	r.Header.Set("Origin", f.s.PublicURL)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: got%d want%d: %s", method, path, w.Code, status, w.Body.String())
	}
	return w
}
func publicFixture(t *testing.T) (*accountFixture, string, string) {
	t.Helper()
	f, id := variablesFixture(t)
	cookie := f.user("public-owner", "public-owner@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','public-owner','owner')`)
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('default','variable-node')`)
	return f, id, cookie
}
func mintPublic(t *testing.T, f *accountFixture, cookie, scope string, scopes ...string) (string, string) {
	t.Helper()
	body := f.call(cookie, "default", "POST", "/api/access-tokens", map[string]any{"name": "Fixture", "scope": scope, "scopes": scopes, "days": 1}, 201)
	return jsonString(t, body, "id"), jsonString(t, body, "token")
}
func TestPublicTokensRevalidateGrantsScopesExpiryAndTeams(t *testing.T) {
	f, id, cookie := publicFixture(t)
	tokenID, token := mintPublic(t, f, cookie, "user", "workloads:read", "projects:read", "workloads:operate")
	publicCall(t, f, token, "default", "GET", "/api/v1/workloads/"+id, nil, 200)
	publicCall(t, f, token, "default", "GET", "/api/workloads/"+id, nil, 401)
	publicCall(t, f, token, "default", "POST", "/api/v1/workloads/"+id+"/variables/export", nil, 403)
	publicCall(t, f, token, "default", "POST", "/api/v1/workloads/"+id+"/operations", apiOperationInput{Action: "restore"}, 403)
	list := f.call(cookie, "default", "GET", "/api/access-tokens", nil, 200)
	if strings.Contains(list.Body.String(), token) {
		t.Fatal("token secret exposed")
	}
	f.sql(`UPDATE team_members SET role='member',grants='[{"scope":"environment","scope_id":"variable-env","role":"viewer"}]' WHERE user_id='public-owner'`)
	publicCall(t, f, token, "default", "GET", "/api/v1/workloads/"+id, nil, 200)
	publicCall(t, f, token, "default", "POST", "/api/v1/workloads/"+id+"/operations", apiOperationInput{Action: "deploy"}, 403)
	f.sql(`UPDATE team_members SET grants='[]' WHERE user_id='public-owner'`)
	publicCall(t, f, token, "default", "GET", "/api/v1/workloads/"+id, nil, 403)
	f.sql(`UPDATE team_members SET role='owner' WHERE user_id='public-owner'`)
	_, teamToken := mintPublic(t, f, cookie, "team", "workloads:read")
	publicCall(t, f, teamToken, "other-team", "GET", "/api/v1/session", nil, 403)
	f.sql(`INSERT INTO teams(id,name) VALUES('other-team','Other')`)
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('other-team','public-owner','owner')`)
	publicCall(t, f, token, "other-team", "GET", "/api/v1/projects", nil, 200)
	f.call(cookie, "default", "DELETE", "/api/access-tokens/"+tokenID, nil, 200)
	publicCall(t, f, token, "default", "GET", "/api/v1/session", nil, 401)
	f.sql(`UPDATE access_tokens SET expires_at=now()-interval '1 minute'`)
	publicCall(t, f, teamToken, "default", "GET", "/api/v1/session", nil, 401)
	_, token = mintPublic(t, f, cookie, "user", "workloads:read")
	f.sql(`UPDATE users SET disabled=true WHERE id='public-owner'`)
	publicCall(t, f, token, "default", "GET", "/api/v1/session", nil, 401)
}
func TestPublicAPIContractsSecretsAndExec(t *testing.T) {
	f, id, cookie := publicFixture(t)
	_, token := mintPublic(t, f, cookie, "user", "projects:read", "projects:write", "workloads:read", "secrets:read", "secrets:write", "exec:run")
	assertSchema := func(path, method string, w *httptest.ResponseRecorder) {
		t.Helper()
		doc := f.s.openAPIDocument()
		operation := doc["paths"].(map[string]any)[path].(map[string]any)[strings.ToLower(method)].(map[string]any)
		response := operation["responses"].(map[string]any)[httpStatus(w.Code)].(map[string]any)
		schema := response["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
		raw, _ := json.Marshal(schema)
		var definition jsonschema.Schema
		if err := json.Unmarshal(raw, &definition); err != nil {
			t.Fatal(err)
		}
		resolved, err := definition.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err = json.Unmarshal(w.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if err = resolved.Validate(value); err != nil {
			t.Fatal("handler response violates generated schema", err)
		}
	}
	spec := publicCall(t, f, "", "", "GET", "/api/v1/openapi.json", nil, 200)
	if !strings.Contains(spec.Body.String(), `"openapi":"3.1.0"`) {
		t.Fatal("missing OpenAPI3.1")
	}
	created := publicCall(t, f, token, "default", "POST", "/api/v1/projects", apiNameInput{Name: "API project"}, 201)
	assertSchema("/projects", "POST", created)
	projectID := jsonString(t, created, "id")
	assertSchema("/projects/{id}", "GET", publicCall(t, f, token, "default", "GET", "/api/v1/projects/"+projectID, nil, 200))
	publicCall(t, f, token, "default", "PUT", "/api/v1/projects/"+projectID, apiNameInput{Name: "Renamed API project"}, 200)
	env := publicCall(t, f, token, "default", "POST", "/api/v1/projects/"+projectID+"/environments", apiNameInput{Name: "Staging"}, 201)
	environmentID := jsonString(t, env, "id")
	assertSchema("/environments/{id}", "GET", publicCall(t, f, token, "default", "GET", "/api/v1/environments/"+environmentID, nil, 200))
	assertSchema("/projects", "GET", publicCall(t, f, token, "default", "GET", "/api/v1/projects", nil, 200))
	assertSchema("/workloads/{id}", "GET", publicCall(t, f, token, "default", "GET", "/api/v1/workloads/"+id, nil, 200))
	path := "/api/v1/workloads/" + id + "/variables/KEY"
	publicCall(t, f, token, "default", "PUT", path, apiVariableInput{Value: "fixture-secret-value"}, 200)
	read := publicCall(t, f, token, "default", "GET", path, nil, 200)
	assertSchema("/workloads/{id}/variables/{variable}", "GET", read)
	if strings.Contains(read.Body.String(), "fixture-secret-value") {
		t.Fatal("value disclosed by ordinary variable read")
	}
	exported := publicCall(t, f, token, "default", "POST", "/api/v1/workloads/"+id+"/variables/resolve", nil, 200)
	if !strings.Contains(exported.Body.String(), "fixture-secret-value") || exported.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("explicit resolved export failed")
	}
	var audits int
	f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE subject=$1 AND action='workload resolved variables exported'`, id).Scan(&audits)
	if audits != 1 {
		t.Fatal("disclosure audit missing")
	}
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"exec":true}'::jsonb WHERE id='variable-node'`)
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "variable-node" || r.Method != "exec" || r.Exec.Workload != id {
			t.Fatal("unsafe exec target")
		}
		return worker.RPCResponse{Exec: &worker.ExecResult{Output: "fixture-output", ExitCode: 7, Truncated: true}}, nil
	}
	body := worker.ExecRequest{Service: "app", Command: []string{"printf", "fixture"}, TimeoutSeconds: 2}
	assertSchema("/workloads/{id}/exec", "POST", publicCall(t, f, token, "default", "POST", "/api/v1/workloads/"+id+"/exec", body, 200))
	body.Workload = "different"
	publicCall(t, f, token, "default", "POST", "/api/v1/workloads/"+id+"/exec", body, 400)
	publicCall(t, f, token, "default", "DELETE", path, nil, 200)
	publicCall(t, f, token, "default", "GET", path, nil, 404)
}
func httpStatus(status int) string { return fmt.Sprint(status) }
func TestMCPDisabledScopeFilteringAndRevocation(t *testing.T) {
	f, id, cookie := publicFixture(t)
	tokenID, token := mintPublic(t, f, cookie, "user", "workloads:read", "projects:read", "logs:read", "workloads:operate")
	publicCall(t, f, token, "default", "POST", "/mcp", map[string]any{}, 404)
	f.call(cookie, "default", "PUT", "/api/mcp", map[string]bool{"enabled": true}, 403)
	f.call("root", "default", "PUT", "/api/mcp", map[string]bool{"enabled": true}, 200)
	publicCall(t, f, f.s.AdminToken, "default", "POST", "/mcp", map[string]any{}, 401)
	var rejected int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action='POST /mcp' AND status=401`).Scan(&rejected); err != nil || rejected == 0 {
		t.Fatal("MCP authorization denial was not audited", err)
	}

	httpClient := &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Cove-Team", "default")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w.Result(), nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sdk := mcp.NewClient(&mcp.Implementation{Name: "cove-fixture", Version: "1"}, nil)
	session, err := sdk.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://127.0.0.1/mcp", HTTPClient: httpClient, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 7 {
		t.Fatal("MCP tool registration failed", err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	for _, name := range []string{"deploy", "restart", "logs", "pull_and_deploy", "rollout_promote", "rollout_abort", "image_check"} {
		if !names[name] {
			t.Fatal("scoped MCP tool missing", name)
		}
	}
	resources, err := session.ListResources(ctx, nil)
	if err != nil || len(resources.Resources) != 2 {
		t.Fatal("MCP resources unavailable", err)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "deploy", Arguments: map[string]any{"workload": id}})
	if err != nil || result.IsError {
		t.Fatal("MCP deploy did not use public API", err, result)
	}
	f.call(cookie, "default", "DELETE", "/api/access-tokens/"+tokenID, nil, 200)
	if _, err = session.ListTools(ctx, nil); err == nil {
		t.Fatal("MCP continued after token revocation")
	}
}

func TestPublicVerifiedLegacyAccountCanCreateTokens(t *testing.T) {
	f, _, _ := publicFixture(t)
	body := map[string]any{"name": "Legacy fixture", "scope": "user", "scopes": []string{"projects:read"}, "days": 1}
	f.call("root", "default", "POST", "/api/access-tokens", body, 409)
	f.sql(`UPDATE users SET email='verified-legacy@example.com',email_verified=true WHERE id='bootstrap'`)
	created := f.call("root", "default", "POST", "/api/access-tokens", body, 201)
	token := jsonString(t, created, "token")
	publicCall(t, f, token, "default", "GET", "/api/v1/projects", nil, 200)
}
