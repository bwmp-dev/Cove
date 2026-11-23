package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"cove/internal/outbound"
	"cove/internal/worker"
)

func TestEnvironmentFileParsingAndRoundTrip(t *testing.T) {
	input := "\ufeff# variables\r\nexport ONE = plain # comment\r\nURL=https://example.com/#fragment\nSINGLE='literal $VAR # text'\nDOUBLE=\"a\\nline \\\"quote\\\" \\$literal\"\nMULTI='first\nsecond'\nEMPTY=\nONE=last\n"
	values, err := parseEnvironmentFile(input)
	if err != nil {
		t.Fatal(err)
	}
	if values["ONE"] != "last" || values["URL"] != "https://example.com/#fragment" || values["SINGLE"] != "literal $VAR # text" || values["DOUBLE"] != "a\nline \"quote\" $literal" || values["MULTI"] != "first\nsecond" || values["EMPTY"] != "" {
		t.Fatalf("unexpected parsed values: %#v", values)
	}
	values["SPECIAL"] = "<>&\t\r\n\\\"$HOME ${NOT_EXPANDED} ?"
	encoded := encodeEnvironmentFile(values)
	got, err := parseEnvironmentFile(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range values {
		if got[key] != value {
			t.Fatalf("roundtrip changed %s", key)
		}
	}
	for _, bad := range []string{"PATH=bad", "DOCKER_HOST=bad", "COMPOSE_FILE=bad", "LD_PRELOAD=bad", "ONE=\x00", "1KEY=value", "KEY", "KEY='unterminated", "KEY=\"bad\\q\"", "KEY='value' trailing", "KEY=" + strings.Repeat("x", 65537)} {
		if _, err := parseEnvironmentFile(bad); err == nil {
			t.Fatalf("accepted invalid input %q", bad[:min(40, len(bad))])
		}
	}
}

func variablesFixture(t *testing.T) (*accountFixture, string) {
	t.Helper()
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('variable-node','Node','localhost',22,'',$1,'ready','{"builtin_variables":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('variable-project','Variable Project','default')`)
	f.sql(`INSERT INTO environments(id,name,project_id,team_id) VALUES('variable-env','Production','variable-project','default')`)
	in := workloadInput{Name: "Variables", NodeID: "variable-node", ProjectID: "variable-project", EnvironmentID: "variable-env", Compose: "services:\n  app:\n    image: busybox:1.37\n", Environment: map[string]string{"KEEP": "retained", "SECRET": "saved-secret"}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", in, 201), "id")
	return f, id
}

func TestEnvironmentFileAccessAtomicImportExport(t *testing.T) {
	f, id := variablesFixture(t)
	viewer := f.user("variable-viewer", "variable-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','variable-viewer','member',$1)`, `[{"scope":"environment","scope_id":"variable-env","role":"viewer"}]`)
	base := "/api/workloads/" + id + "/variables/"
	f.call(viewer, "default", "POST", base+"export", nil, 403)
	f.call(viewer, "default", "POST", base+"import", map[string]string{"dotenv": "KEY=no"}, 403)
	f.call("root", "default", "POST", base+"import", map[string]string{"dotenv": "SECRET=new-secret\nMULTILINE='first\nsecond'"}, 200)
	f.call("root", "default", "POST", base+"import", map[string]string{"dotenv": "SECRET=bad\nPATH=reserved"}, 400)
	exported := f.call("root", "default", "POST", base+"export", nil, 200)
	if exported.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("export cached")
	}
	var body struct {
		Dotenv string `json:"dotenv"`
	}
	json.Unmarshal(exported.Body.Bytes(), &body)
	values, err := parseEnvironmentFile(body.Dotenv)
	if err != nil || values["KEEP"] != "retained" || values["SECRET"] != "new-secret" || values["MULTILINE"] != "first\nsecond" {
		t.Fatal("merge or atomicity failed", err)
	}
	ordinary := f.call("root", "default", "GET", "/api/workloads/"+id, nil, 200)
	if strings.Contains(ordinary.Body.String(), "new-secret") {
		t.Fatal("ordinary read revealed value")
	}
	var count int
	if err = f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE subject=$1 AND action='workload variables exported' AND status=200`, id).Scan(&count); err != nil || count != 1 {
		t.Fatal("missing export audit", err, count)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES('active-variable-op',$1,'deploy',$2)`, id, []byte{})
	f.call("root", "default", "POST", base+"import", map[string]string{"dotenv": "SECRET=busy"}, 409)
}

func TestExternalSecretProvidersResolveAndFailClosed(t *testing.T) {
	f, id := variablesFixture(t)
	tests := []struct{ kind, ref, path, body string }{
		{"vault", "kv/app#password", "/v1/kv/data/app", `{"data":{"data":{"password":"resolved-value"}}}`},
		{"openbao", "kv/path/app#password", "/v1/kv/data/path/app", `{"data":{"data":{"password":"resolved-value"}}}`},
		{"infisical", "/folder/PASSWORD", "/api/v4/secrets/PASSWORD", `{"secret":{"secretValue":"resolved-value","secretValueHidden":false}}`},
		{"doppler", "PASSWORD", "/v3/configs/config/secrets/download", `{"PASSWORD":"resolved-value"}`},
		{"onepassword-connect", "vault/item/password", "/v1/vaults/vault/items/item", `{"fields":[{"id":"password","value":"resolved-value"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			config := secretProviderConfig{Name: "Test " + tc.kind, Kind: tc.kind, URL: "https://provider.example", Token: "fixture-provider-token", Project: "project-id", Environment: "production", Namespace: "tenant"}
			if tc.kind == "doppler" {
				config.URL = "https://api.doppler.com"
			}
			created := f.call("root", "default", "POST", "/api/secret-providers", config, 201)
			connection := jsonString(t, created, "id")
			ref := "{{secret." + connection + ":" + tc.ref + "}}"
			calls := 0
			status := 200
			response := tc.body
			f.s.OAuthClient = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.Path != tc.path {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				if tc.kind == "vault" || tc.kind == "openbao" {
					if r.Header.Get("X-Vault-Token") != config.Token || r.Header.Get("X-Vault-Namespace") != "tenant" {
						t.Fatal("missing Vault headers")
					}
				} else if r.Header.Get("Authorization") != "Bearer "+config.Token {
					t.Fatal("missing provider auth")
				}
				if tc.kind == "infisical" && (r.URL.Query().Get("projectId") != "project-id" || r.URL.Query().Get("secretPath") != "/folder" || r.URL.Query().Get("viewSecretValue") != "true") {
					t.Fatal("wrong Infisical scope")
				}
				if tc.kind == "doppler" && (r.URL.Query().Get("secrets") != "PASSWORD" || r.URL.Query().Get("include_dynamic_secrets") != "false") {
					t.Fatal("wrong Doppler selection")
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{}}, nil
			})}
			got, err := f.s.resolveExternalVariables(context.Background(), id, map[string]string{"FIRST": "prefix-" + ref, "SECOND": ref})
			if err != nil || got["FIRST"] != "prefix-resolved-value" || got["SECOND"] != "resolved-value" || calls != 1 {
				t.Fatal("resolve/cache failed", err, calls)
			}
			listed := f.call("root", "default", "GET", "/api/secret-providers", nil, 200)
			if strings.Contains(listed.Body.String(), config.Token) || strings.Contains(listed.Body.String(), "resolved-value") || strings.Contains(created.Body.String(), config.Token) {
				t.Fatal("provider value exposed")
			}
			for _, failure := range []struct {
				status int
				body   string
			}{{403, `{"error":"sensitive-server-detail"}`}, {200, `{}`}, {200, strings.Repeat("x", 2*1024*1024+1)}} {
				status, response = failure.status, failure.body
				_, err = f.s.resolveExternalVariables(context.Background(), id, map[string]string{"FIRST": ref})
				if err == nil || strings.Contains(err.Error(), "sensitive-server-detail") || strings.Contains(err.Error(), config.Token) {
					t.Fatal("provider failure not safe", err)
				}
			}
			f.call("root", "default", "DELETE", "/api/secret-providers/"+connection, nil, 200)
			if _, err = f.s.resolveExternalVariables(context.Background(), id, map[string]string{"FIRST": ref}); err == nil {
				t.Fatal("deleted provider resolved")
			}
		})
	}
}

func TestExternalSecretTeamBoundaryAndSnapshot(t *testing.T) {
	f, id := variablesFixture(t)
	c := secretProviderConfig{Name: "Vault", Kind: "vault", URL: "https://vault.example", Token: "fixture-token"}
	connection := jsonString(t, f.call("root", "default", "POST", "/api/secret-providers", c, 201), "id")
	ref := "{{secret." + connection + ":kv/app#password}}"
	calls := 0
	f.s.OAuthClient = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"data":{"password":"resolved-value"}}}`))}, nil
	})}
	f.sql(`INSERT INTO teams(id,name) VALUES('other-variable-team','Other')`)
	f.sql(`UPDATE secret_providers SET team_id='other-variable-team' WHERE id=$1`, connection)
	if _, err := f.s.resolveExternalVariables(context.Background(), id, map[string]string{"KEY": ref}); err == nil || calls != 0 {
		t.Fatal("cross-team secret resolved")
	}
	f.call("root", "default", "PUT", "/api/secret-providers/"+connection, c, 404)
	f.sql(`UPDATE secret_providers SET team_id='default' WHERE id=$1`, connection)
	f.call("root", "default", "PATCH", "/api/shared-variables", map[string]any{"patch": map[string]string{"REFERENCE": ref}}, 200)
	operation := resourceID()
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'deploy',$3)`, operation, id, []byte{})
	request := worker.Request{ID: operation, Workload: id, Action: "deploy", Environment: map[string]string{"KEY": "{{team.REFERENCE}}"}, Routes: []worker.Route{{Domain: "app.example.com"}}}
	if err := f.s.prepareInfrastructure(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if request.Environment["KEY"] != "resolved-value" || request.BuiltinDefaults[worker.CoveProject] != "Variable Project" || request.BuiltinDefaults[worker.CoveEnvironment] != "Production" || request.BuiltinDefaults[worker.CoveRelease] != operation || request.BuiltinDefaults[worker.CoveURL] != "https://app.example.com" {
		t.Fatal("resolver composition or builtins failed")
	}
	f.s.OAuthClient = nil
	if err := f.s.prepareInfrastructure(context.Background(), &request); err != nil || calls != 1 {
		t.Fatal("retry changed secret snapshot", err, calls)
	}
	var raw []byte
	f.s.DB.QueryRow(context.Background(), `SELECT payload FROM operations WHERE id=$1`, operation).Scan(&raw)
	if strings.Contains(string(raw), "resolved-value") {
		t.Fatal("operation snapshot not sealed")
	}
	for _, bad := range []string{"{{secret.invalid:ref}}", "{{secret." + connection + ":}}", "{{secret." + connection + ":../bad#field}}"} {
		if _, err := f.s.resolveExternalVariables(context.Background(), id, map[string]string{"KEY": bad}); err == nil {
			t.Fatal("invalid reference accepted")
		}
	}
	c.URL = "https://changed.example"
	f.call("root", "default", "PUT", "/api/secret-providers/"+connection, c, 409)
}

func TestSecretProviderValueValidationAndOpaqueExpansion(t *testing.T) {
	f, id := variablesFixture(t)
	c := secretProviderConfig{Name: "Vault", Kind: "vault", URL: "https://vault.example", Token: "fixture-token"}
	connection := jsonString(t, f.call("root", "default", "POST", "/api/secret-providers", c, 201), "id")
	ref := "{{secret." + connection + ":kv/app#value}}"
	body := `{"data":{"data":{"value":"{{secret.literal:unexpanded}}"}}}`
	f.s.OAuthClient = &http.Client{Transport: oauthTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	got, err := f.s.resolveExternalVariables(context.Background(), id, map[string]string{"KEY": ref})
	if err != nil || got["KEY"] != "{{secret.literal:unexpanded}}" {
		t.Fatal("returned values recursively expanded", err)
	}
	for _, value := range []any{nil, 42, "bad\x00value", strings.Repeat("x", 65537)} {
		raw, _ := json.Marshal(map[string]any{"data": map[string]any{"data": map[string]any{"value": value}}})
		body = string(raw)
		if _, err = f.s.resolveExternalVariables(context.Background(), id, map[string]string{"KEY": ref}); err == nil {
			t.Fatal("invalid external value accepted")
		}
	}
	raw, _ := json.Marshal(map[string]any{"data": map[string]any{"data": map[string]any{"value": strings.Repeat("x", 40000)}}})
	body = string(raw)
	if _, err = f.s.resolveExternalVariables(context.Background(), id, map[string]string{"KEY": ref + ref}); err == nil {
		t.Fatal("oversized expanded value accepted")
	}
	c.Token = ""
	c.Name = "Renamed"
	f.call("root", "default", "PUT", "/api/secret-providers/"+connection, c, 200)
	raw = nil
	f.s.DB.QueryRow(context.Background(), `SELECT configuration FROM secret_providers WHERE id=$1`, connection).Scan(&raw)
	var saved secretProviderConfig
	if f.s.unsealJSON(raw, &saved) != nil || saved.Token != "fixture-token" {
		t.Fatal("blank rotation lost credential")
	}
	viewer := f.user("provider-viewer", "provider-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','provider-viewer','member','[{"scope":"team","scope_id":"","role":"viewer"}]')`)
	f.call(viewer, "default", "POST", "/api/secret-providers", c, 403)
	f.call(viewer, "default", "DELETE", "/api/secret-providers/"+connection, nil, 403)
	builtins := f.call(viewer, "default", "GET", "/api/workloads/"+id+"/variables/builtins", nil, 200)
	if !strings.Contains(builtins.Body.String(), `"available":true`) || !strings.Contains(builtins.Body.String(), "COVE_COMMIT_SHA") {
		t.Fatal("builtin capability not exposed")
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='variable-node'`)
	builtins = f.call(viewer, "default", "GET", "/api/workloads/"+id+"/variables/builtins", nil, 200)
	if !strings.Contains(builtins.Body.String(), `"available":false`) {
		t.Fatal("old worker incorrectly advertised builtins")
	}
}

func TestPreviewRequiresExternalSecretAndSharedVariableDecisions(t *testing.T) {
	f := newPreviewFixture(t)
	recipe := f.setup()
	id := f.ids["worker"]
	var raw []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT configuration FROM workloads WHERE id=$1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var config workloadInput
	if err := f.s.unsealJSON(raw, &config); err != nil {
		t.Fatal(err)
	}
	config.Environment["OPAQUE"] = "{{secret." + strings.Repeat("a", 32) + ":kv/app#password}}"
	config.Environment["SHARED"] = "{{team.REFERENCE}}"
	raw, err := f.s.sealPreview(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE workloads SET configuration=$2 WHERE id=$1`, id, raw)
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", recipe, 200)
	response := f.call("root", "default", "GET", "/api/projects/p/preview-recipe", nil, 200)
	var view struct {
		Plan previewPlan `json:"plan"`
	}
	if json.Unmarshal(response.Body.Bytes(), &view) != nil {
		t.Fatal("invalid plan")
	}
	decisions := map[string]bool{}
	for _, resource := range view.Plan.Resources {
		if resource.ID == id {
			for _, blocker := range resource.Blockers {
				if blocker.Code == "variable_decision" {
					decisions[blocker.Key] = true
				}
			}
		}
	}
	if !decisions["OPAQUE"] || !decisions["SHARED"] {
		t.Fatal("external/shared reference copied without explicit decision")
	}
}

func TestInfisicalHiddenAndConnectMissingFieldsFail(t *testing.T) {
	for _, test := range []struct{ kind, ref, body string }{
		{"infisical", "/PASSWORD", `{"secret":{"secretValue":"hidden-secret","secretValueHidden":true}}`},
		{"infisical", "/PASSWORD", `{"secret":{"secretValue":null,"secretValueHidden":false}}`},
		{"onepassword-connect", "vault/item/password", `{"fields":[{"id":"other","label":"password","value":"wrong-field"}]}`},
	} {
		s := &Server{outboundState: &outboundProxyState{}, OAuthClient: &http.Client{Transport: oauthTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(test.body))}, nil
		})}}
		proxy := outbound.Config{}
		s.outboundState.value.Store(&proxy)
		source := httpVariableSource{server: s, config: secretProviderConfig{Kind: test.kind, URL: "https://provider.example", Token: "fixture-token"}}
		if _, err := source.Resolve(context.Background(), test.ref); err == nil {
			t.Fatal("hidden or absent field accepted")
		}
	}
}
