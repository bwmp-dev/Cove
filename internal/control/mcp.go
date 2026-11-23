package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) mcpSettings(w http.ResponseWriter, r *http.Request) {
	if !actor(r).Admin {
		http.Error(w, "permission denied", 403)
		return
	}
	if r.Method == "PUT" {
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if !decode(w, r, &in) {
			return
		}
		if _, err := s.DB.Exec(r.Context(), `UPDATE control_settings SET mcp_enabled=$1 WHERE id=1`, in.Enabled); err != nil {
			http.Error(w, "could not save MCP setting", 503)
			return
		}
	}
	var enabled bool
	if err := s.DB.QueryRow(r.Context(), `SELECT mcp_enabled FROM control_settings WHERE id=1`).Scan(&enabled); err != nil {
		http.Error(w, "MCP setting unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"enabled": enabled, "url": s.PublicURL + "/mcp"})
}

func (s *Server) mcpHTTP(w http.ResponseWriter, r *http.Request) {
	response := &statusResponse{ResponseWriter: w}
	w = response
	defer func() {
		if response.status == 401 || response.status == 403 {
			s.recordAudit(r, "rejected MCP authentication or authorization", response.status)
		}
	}()
	var enabled bool
	if err := s.DB.QueryRow(r.Context(), `SELECT mcp_enabled FROM control_settings WHERE id=1`).Scan(&enabled); err != nil {
		http.Error(w, "MCP unavailable", 503)
		return
	}
	if !enabled {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Origin") != "" && !s.sameOrigin(r) {
		http.Error(w, "origin not permitted", 403)
		return
	}
	if bearer(r) == "" {
		http.Error(w, "scoped API token required", 401)
		return
	}
	original := r.Clone(r.Context())
	original.Pattern = r.Method + " /mcp"
	original.URL.Path = "/api/search"
	s.authenticatePublicToken(http.HandlerFunc(func(w http.ResponseWriter, authorized *http.Request) {
		authorized.URL.Path = "/mcp"
		authorized.Pattern = r.Method + " /mcp"
		handler := mcp.NewStreamableHTTPHandler(func(request *http.Request) *mcp.Server { return s.mcpServer(request) }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 1024 * 1024, PropagateRequestCancellation: true})
		handler.ServeHTTP(w, authorized)
	}), w, original, publicRoute{})
}

type mcpWorkloadInput struct {
	Reason   string `json:"reason,omitempty" jsonschema:"Optional audited deployment reason"`
	Workload string `json:"workload" jsonschema:"The Cove resource ID"`
}

func (s *Server) mcpServer(r *http.Request) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "cove", Version: "1.0.0"}, nil)
	credential, _ := r.Context().Value(apiCredentialKey{}).(apiCredential)
	for _, item := range []struct{ uri, name, path, scope string }{{"cove://projects", "Projects", "/projects", "projects:read"}, {"cove://workloads", "Resources", "/workloads", "workloads:read"}} {
		if !containsScope(credential.Scopes, item.scope) {
			continue
		}
		server.AddResource(&mcp.Resource{URI: item.uri, Name: item.name, MIMEType: "application/json"}, func(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			raw, err := s.invokePublic(ctx, r, "GET", item.path, nil, nil)
			if err != nil {
				return nil, err
			}
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: item.uri, MIMEType: "application/json", Text: string(raw)}}}, nil
		})
	}
	for _, item := range []struct{ uri, name, path string }{{"cove://workloads/{id}", "Resource", "/workloads/{id}"}, {"cove://operations/{id}", "Operation", "/operations/{id}"}} {
		if !containsScope(credential.Scopes, "workloads:read") {
			continue
		}
		server.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: item.uri, Name: item.name, MIMEType: "application/json"}, func(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			id := strings.TrimPrefix(request.Params.URI, strings.TrimSuffix(item.uri, "{id}"))
			if id == "" || strings.ContainsAny(id, "/\\?#%") {
				return nil, errors.New("invalid resource URI")
			}
			raw, err := s.invokePublic(ctx, r, "GET", item.path, map[string]string{"id": id}, nil)
			if err != nil {
				return nil, err
			}
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: request.Params.URI, MIMEType: "application/json", Text: string(raw)}}}, nil
		})
	}
	for _, tool := range []struct{ name, description, scope, method, path, action string }{
		{"deploy", "Queue a new deployment. Poll the returned operation ID for its outcome.", "workloads:operate", "POST", "/workloads/{id}/operations", "deploy"},
		{"pull_and_deploy", "Pull referenced images and queue a deployment through normal admission.", "workloads:operate", "POST", "/workloads/{id}/operations", "deploy"},
		{"rollout_promote", "Promote a staged rollout through normal deployment admission.", "workloads:operate", "POST", "/workloads/{id}/operations", "rollout_promote"},
		{"rollout_abort", "Abort a staged rollout and recover the retained generation.", "workloads:operate", "POST", "/workloads/{id}/operations", "rollout_abort"},
		{"image_check", "Queue a registry digest check without deploying.", "workloads:operate", "POST", "/workloads/{id}/operations", "image_check"},
		{"restart", "Queue a resource restart.", "workloads:operate", "POST", "/workloads/{id}/operations", "restart"},
		{"logs", "Read a bounded container log snapshot.", "logs:read", "GET", "/workloads/{id}/logs", ""},
	} {
		if !containsScope(credential.Scopes, tool.scope) {
			continue
		}
		mcp.AddTool(server, &mcp.Tool{Name: tool.name, Description: tool.description}, func(ctx context.Context, request *mcp.CallToolRequest, input mcpWorkloadInput) (*mcp.CallToolResult, any, error) {
			if input.Workload == "" {
				return nil, nil, errors.New("resource ID required")
			}
			var body any
			if tool.action != "" {
				body = apiOperationInput{Action: tool.action, Reason: input.Reason, PullLatest: tool.name == "pull_and_deploy"}
			}
			raw, err := s.invokePublic(ctx, r, tool.method, tool.path, map[string]string{"id": input.Workload}, body)
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
			}
			var value any
			if json.Unmarshal(raw, &value) != nil {
				return nil, nil, errors.New("invalid API response")
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, value, nil
		})
	}
	return server
}
