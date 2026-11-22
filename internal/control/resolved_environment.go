package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

func (s *Server) resolvedEnvironment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAccess(w, r, "workload", id, 3) || !s.requirePersistentResource(w, r, id) {
		return
	}
	var raw []byte
	var config workloadInput
	if s.DB.QueryRow(r.Context(), `SELECT configuration FROM workloads WHERE id=$1`, id).Scan(&raw) != nil || s.unsealJSON(raw, &config) != nil {
		http.Error(w, "variables unavailable", 503)
		return
	}
	var release, revision string
	err := s.DB.QueryRow(r.Context(), `SELECT id,COALESCE(result->>'revision','') FROM operations WHERE workload_id=$1 AND state='succeeded' AND action IN ('deploy','rollback') ORDER BY created_at DESC LIMIT 1`, id).Scan(&release, &revision)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "release metadata unavailable", 503)
		return
	}
	request := worker.Request{ID: release, Workload: id, Action: "deploy", Environment: config.Environment, Source: config.Source, Routes: config.Routes}
	if request.Source != nil {
		request.Source.Revision = revision
	}
	if err = s.resolveDeploymentVariables(r.Context(), &request); err != nil {
		var resolution *variableResolutionError
		if errors.As(err, &resolution) {
			http.Error(w, resolution.Error(), 409)
		} else {
			http.Error(w, "variable resolution failed; check shared references and providers", 409)
		}
		return
	}
	values := map[string]string{}
	for key, value := range request.BuiltinDefaults {
		values[key] = value
	}
	for key, value := range request.Environment {
		values[key] = value
	}
	if _, err = s.DB.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'workload resolved variables exported',$2,200,$3)`, "user "+actor(r).ID, id, actor(r).Team); err != nil {
		http.Error(w, "could not audit variable disclosure", 503)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, map[string]any{"environment": values})
}

type apiCapture struct {
	headers http.Header
	status  int
	body    strings.Builder
	failed  bool
}

func (w *apiCapture) Header() http.Header { return w.headers }
func (w *apiCapture) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *apiCapture) Write(raw []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if w.body.Len()+len(raw) > 4*1024*1024 {
		w.failed = true
		return 0, errors.New("response exceeds limit")
	}
	return w.body.Write(raw)
}
func (s *Server) invokePublic(ctx context.Context, original *http.Request, method, path string, params map[string]string, input any) (json.RawMessage, error) {
	for _, route := range s.publicRoutes() {
		if route.Method != method || route.Path != path {
			continue
		}
		request := original.Clone(ctx)
		request.Method = method
		request.URL.Path = "/api/v1" + path
		request.URL.RawQuery = ""
		for name, value := range params {
			request.SetPathValue(name, value)
			request.URL.Path = strings.ReplaceAll(request.URL.Path, "{"+name+"}", value)
		}
		raw, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		request.Body = http.NoBody
		if input != nil {
			request.Body = io.NopCloser(bytes.NewReader(raw))
		}
		response := &apiCapture{headers: http.Header{}}
		s.publicHandler(route).ServeHTTP(response, request)
		if response.failed {
			return nil, errors.New("Cove response exceeds limit")
		}
		if response.status >= 400 {
			return nil, errors.New(strings.TrimSpace(response.body.String()))
		}
		return json.RawMessage(response.body.String()), nil
	}
	return nil, errors.New("unsupported public API operation")
}
