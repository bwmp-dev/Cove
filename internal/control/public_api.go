package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"cove/internal/worker"
)

type publicRoute struct {
	Method, Path, Scope, Summary string
	Input, Output                any
	Status                       int
	Handler                      http.HandlerFunc
}
type apiNameInput struct {
	Name string `json:"name"`
}
type apiOperationInput struct {
	Action   string `json:"action"`
	Release  string `json:"release,omitempty"`
	BackupID string `json:"backup_id,omitempty"`
}
type apiID struct {
	ID string `json:"id"`
}
type apiOK struct {
	OK bool `json:"ok"`
}
type apiEnvironmentFile struct {
	Dotenv string `json:"dotenv"`
}
type apiVariableInput struct {
	Value string `json:"value"`
}
type apiVariableView struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
type apiTunnelInput struct {
	Service   string `json:"service"`
	Port      int    `json:"port"`
	PublicKey string `json:"public_key"`
}
type apiTunnelView struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
	Host      string    `json:"host"`
	SSHPort   int       `json:"ssh_port"`
	HostKey   string    `json:"host_key"`
}

func (s *Server) publicRoutes() []publicRoute {
	return []publicRoute{
		{"GET", "/session", "", "Read the token creator and selected team", nil, identity{}, 200, s.sessionInfo},
		{"GET", "/nodes", "workloads:read", "List accessible worker metadata", nil, []map[string]any{}, 200, s.listNodes},
		{"GET", "/projects", "projects:read", "List accessible projects and environments", nil, []project{}, 200, s.listProjects},
		{"POST", "/projects", "projects:write", "Create a project", apiNameInput{}, apiID{}, 201, s.createProject},
		{"GET", "/projects/{id}", "projects:read", "Read a project", nil, map[string]string{}, 200, s.publicGroup},
		{"PUT", "/projects/{id}", "projects:write", "Rename a project", apiNameInput{}, apiID{}, 200, s.publicGroup},
		{"DELETE", "/projects/{id}", "projects:write", "Delete an empty project", nil, apiOK{}, 200, s.deleteProject},
		{"POST", "/projects/{id}/environments", "projects:write", "Create an environment", apiNameInput{}, apiID{}, 201, s.createEnvironment},
		{"GET", "/environments/{id}", "projects:read", "Read an environment", nil, map[string]string{}, 200, s.publicGroup},
		{"PUT", "/environments/{id}", "projects:write", "Rename an environment", apiNameInput{}, apiID{}, 200, s.publicGroup},
		{"DELETE", "/environments/{id}", "projects:write", "Delete an empty environment", nil, apiOK{}, 200, s.deleteEnvironment},
		{"GET", "/workloads", "workloads:read", "List accessible resources", nil, []map[string]any{}, 200, s.listWorkloads},
		{"POST", "/workloads", "workloads:write", "Create a resource without deploying", workloadInput{}, apiID{}, 201, s.createWorkload},
		{"GET", "/workloads/{id}", "workloads:read", "Read resource configuration with secret values omitted", nil, workloadView{}, 200, s.getWorkload},
		{"PUT", "/workloads/{id}", "workloads:write", "Update saved configuration without deploying", workloadInput{}, apiID{}, 200, s.updateWorkload},
		{"DELETE", "/workloads/{id}", "workloads:write", "Delete a stopped resource", nil, apiOK{}, 200, s.deleteWorkload},
		{"PUT", "/workloads/{id}/routes", "workloads:write", "Replace public routes using normal ingress reconciliation", struct {
			Routes []worker.Route `json:"routes"`
		}{}, map[string]any{}, 200, s.updateRoutes},
		{"POST", "/workloads/{id}/operations", "workloads:operate", "Queue an operation; restore/drill additionally require workloads:restore", apiOperationInput{}, apiID{}, 202, s.queueOperation},
		{"GET", "/workloads/{id}/runtime", "workloads:read", "Read current container metadata", nil, struct {
			Containers []worker.Container `json:"containers"`
		}{}, 200, s.workloadRuntime},
		{"GET", "/workloads/{id}/releases", "workloads:read", "Read retained releases", nil, struct {
			Releases []worker.Release `json:"releases"`
		}{}, 200, s.workloadReleases},
		{"GET", "/workloads/{id}/logs", "logs:read", "Read a bounded container log snapshot; poll to follow", nil, struct {
			Logs string `json:"logs"`
		}{}, 200, s.workloadLogs},
		{"GET", "/operations", "workloads:read", "List up to 100 accessible operations; optional workload_id query filter", nil, []map[string]any{}, 200, s.listOperations},
		{"GET", "/operations/{id}", "workloads:read", "Read an operation", nil, operationView{}, 200, s.getOperation},
		{"GET", "/operations/{id}/logs", "logs:read", "Read bounded operation logs", nil, struct {
			Logs string `json:"logs"`
		}{}, 200, s.operationLogs},
		{"POST", "/operations/{id}/cancel", "workloads:operate", "Request operation cancellation", nil, map[string]string{}, 202, s.cancelOperation},
		{"POST", "/workloads/{id}/variables/import", "secrets:write", "Merge saved dotenv variables", apiEnvironmentFile{}, struct {
			Keys []string `json:"keys"`
		}{}, 200, s.environmentFile},
		{"POST", "/workloads/{id}/variables/resolve", "secrets:read", "Export resolved saved variables and known built-in metadata; audited before disclosure", nil, struct {
			Environment map[string]string `json:"environment"`
		}{}, 200, s.resolvedEnvironment},
		{"POST", "/workloads/{id}/variables/export", "secrets:read", "Export saved raw variables; audited before disclosure", nil, apiEnvironmentFile{}, 200, s.environmentFile},
		{"GET", "/workloads/{id}/variables/{variable}", "secrets:read", "Read existence and opaque revision, never a variable value", nil, apiVariableView{}, 200, s.publicVariable},
		{"PUT", "/workloads/{id}/variables/{variable}", "secrets:write", "Set one saved variable", apiVariableInput{}, apiVariableView{}, 200, s.publicVariable},
		{"DELETE", "/workloads/{id}/variables/{variable}", "secrets:write", "Remove one saved variable", nil, apiOK{}, 200, s.publicVariable},
		{"POST", "/workloads/{id}/exec", "exec:run", "Run a command with a 1-30 second wait and 64 KiB combined output; timeout does not confirm process termination", worker.ExecRequest{}, worker.ExecResult{}, 200, s.workloadExec},
		{"POST", "/workloads/{id}/database-access", "tunnels:write", "Open a 15 minute pinned SSH service tunnel lease", apiTunnelInput{}, apiTunnelView{}, 201, s.databaseAccess},
		{"DELETE", "/workloads/{id}/database-access", "tunnels:write", "Revoke a service tunnel lease", apiID{}, apiOK{}, 200, s.databaseAccess},
	}
}

func (s *Server) registerPublicAPI(mux *http.ServeMux) {
	for _, route := range s.publicRoutes() {
		mux.Handle(route.Method+" /api/v1"+route.Path, s.publicHandler(route))
	}
	mux.HandleFunc("GET /api/v1/openapi.json", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, s.openAPIDocument()) })
}
func (s *Server) publicHandler(route publicRoute) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.Clone(context.WithValue(r.Context(), apiRouteKey{}, route))
		r.URL.Path = strings.Replace(r.URL.Path, "/api/v1/", "/api/", 1)
		r.Pattern = route.Method + " /api" + route.Path
		s.admin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(route.Path, "/operations") {
				fields, ok := bodyFields(w, r)
				if !ok {
					return
				}
				if field(fields, "action") == "restore" || field(fields, "action") == "drill" {
					if token, ok := r.Context().Value(apiCredentialKey{}).(apiCredential); ok && !containsScope(token.Scopes, "workloads:restore") {
						http.Error(w, "token requires workloads:restore", 403)
						return
					}
				}
			}
			route.Handler(w, r)
		})).ServeHTTP(w, r)
	})
}

func containsScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}

func apiSchema(t reflect.Type) map[string]any {
	if t == reflect.TypeFor[json.RawMessage]() {
		return map[string]any{}
	}
	if t.Kind() == reflect.Pointer {
		return map[string]any{"anyOf": []any{apiSchema(t.Elem()), map[string]any{"type": "null"}}}
	}
	if t == reflect.TypeFor[time.Time]() {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": []string{"array", "null"}, "items": apiSchema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": []string{"object", "null"}, "additionalProperties": apiSchema(t.Elem())}
	case reflect.Struct:
		props := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")[0]
			if !field.IsExported() || tag == "-" {
				continue
			}
			if tag == "" {
				tag = field.Name
			}
			props[tag] = apiSchema(field.Type)
		}
		return map[string]any{"type": "object", "properties": props}
	default:
		return map[string]any{}
	}
}
func (s *Server) openAPIDocument() map[string]any {
	paths := map[string]any{}
	for _, route := range s.publicRoutes() {
		entry, ok := paths[route.Path].(map[string]any)
		if !ok {
			entry = map[string]any{}
			paths[route.Path] = entry
		}
		responses := map[string]any{fmt.Sprint(route.Status): map[string]any{"description": "Success", "content": map[string]any{"application/json": map[string]any{"schema": apiSchema(reflect.TypeOf(route.Output))}}}}
		if route.Method == "PUT" && route.Path == "/workloads/{id}/routes" {
			responses["202"] = responses["200"]
		}
		for _, code := range []int{400, 401, 403, 404, 409, 429, 500, 502, 503} {
			responses[fmt.Sprint(code)] = map[string]any{"description": http.StatusText(code), "content": map[string]any{"text/plain": map[string]any{"schema": map[string]any{"type": "string"}}}}
		}
		operation := map[string]any{"summary": route.Summary, "operationId": strings.ToLower(route.Method) + strings.NewReplacer("/", "_", "{", "", "}", "").Replace(route.Path), "x-cove-scope": route.Scope, "security": []any{map[string]any{"bearer": []any{}}}, "responses": responses}
		params := []any{map[string]any{"name": "X-Cove-Team", "in": "header", "required": false, "description": "Selected team for a user token; team tokens are fixed to their team.", "schema": map[string]any{"type": "string"}}}
		for _, part := range strings.Split(route.Path, "/") {
			if strings.HasPrefix(part, "{") {
				params = append(params, map[string]any{"name": strings.Trim(part, "{}"), "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
			}
		}
		operation["parameters"] = params
		if route.Input != nil {
			operation["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": apiSchema(reflect.TypeOf(route.Input))}}}
		}
		entry[strings.ToLower(route.Method)] = operation
	}
	return map[string]any{"openapi": "3.1.0", "info": map[string]string{"title": "Cove public API", "version": "1.0.0", "description": "Scopes limit tokens; every request also revalidates its creator's current grants. Errors are plain text. Deployment is explicit and asynchronous. Secret values appear only in audited exports."}, "servers": []any{map[string]string{"url": "/api/v1"}}, "paths": paths, "components": map[string]any{"securitySchemes": map[string]any{"bearer": map[string]string{"type": "http", "scheme": "bearer"}}}}
}

func (s *Server) publicGroup(w http.ResponseWriter, r *http.Request) {
	environment := strings.Contains(r.URL.Path, "/environments/")
	kind, table := "project", "projects"
	if environment {
		kind, table = "environment", "environments"
	}
	level := 1
	if r.Method != "GET" {
		level = 3
	}
	if !s.requireAccess(w, r, kind, r.PathValue("id"), level) {
		return
	}
	if r.Method == "GET" {
		var name, parent string
		query := `SELECT name,'' FROM projects WHERE id=$1 AND team_id=$2`
		if environment {
			query = `SELECT name,project_id FROM environments WHERE id=$1 AND team_id=$2`
		}
		if s.DB.QueryRow(r.Context(), query, r.PathValue("id"), actor(r).Team).Scan(&name, &parent) != nil {
			http.Error(w, "group unavailable", 503)
			return
		}
		respond(w, 200, map[string]string{"id": r.PathValue("id"), "name": name, "project_id": parent})
		return
	}
	var in apiNameInput
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 120 {
		http.Error(w, "name must contain 1-120 characters", 400)
		return
	}
	if _, err := s.DB.Exec(r.Context(), `UPDATE `+table+` SET name=$2 WHERE id=$1 AND team_id=$3`, r.PathValue("id"), in.Name, actor(r).Team); err != nil {
		groupError(w, err)
		return
	}
	respond(w, 200, apiID{r.PathValue("id")})
}
