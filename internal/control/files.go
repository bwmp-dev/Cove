package control

import (
	"cove/internal/worker"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *Server) workloadFiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		response := &statusResponse{ResponseWriter: w}
		w = response
		defer func() {
			status := response.status
			if status == 0 {
				status = 200
			}
			s.recordAudit(r, actor(r).ID, status)
		}()
	}
	if !actor(r).Admin {
		http.Error(w, "file browsing requires an instance administrator", 403)
		return
	}
	var in worker.FileRequest
	if r.Method == http.MethodGet {
		in.Kind = r.URL.Query().Get("kind")
		in.Target = r.URL.Query().Get("target")
		in.Path = r.URL.Query().Get("path")
		if in.Path == "" {
			in.Path = "/"
		}
	} else if !decode(w, r, &in) {
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/targets"):
		in.Action = "targets"
	case r.Method == http.MethodPut || r.Method == http.MethodPost:
		in.Action = "write"
	case r.Method == http.MethodDelete:
		in.Action = "delete"
	case strings.HasSuffix(r.URL.Path, "/content") || strings.HasSuffix(r.URL.Path, "/download"):
		in.Action = "read"
	default:
		in.Action = "list"
	}
	if err := worker.ValidateFileRequest(in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var node, state string
	var supported bool
	err = tx.QueryRow(r.Context(), `SELECT w.node_id,n.state,COALESCE((n.capabilities->>'file_browser')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 AND w.team_id=$2 FOR UPDATE OF w`, r.PathValue("id"), actor(r).Team).Scan(&node, &state, &supported)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	if state != "ready" || !supported {
		http.Error(w, "select a ready worker with file browser support; upgrade and verify it first", 409)
		return
	}
	var busy bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))`, r.PathValue("id")).Scan(&busy); err != nil {
		http.Error(w, "operation state unavailable", 503)
		return
	}
	if busy {
		http.Error(w, "wait for active operations before file access", 409)
		return
	}
	response, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "files", ID: r.PathValue("id"), Files: &in})
	if err != nil || response.Files == nil || response.Error != "" {
		http.Error(w, "file access failed; check target ownership, path, revision, permissions and required worker tools", 502)
		return
	}
	if len(response.Files.Data) > worker.MaxFileBytes || len(response.Files.Entries) > 200 || len(response.Files.Targets) > 160 {
		http.Error(w, "worker file response exceeds limits", 502)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "file operation completed but database confirmation failed; refresh before retrying", 503)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/download") {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="cove-file.bin"`)
		_, _ = w.Write(response.Files.Data)
		return
	}
	respond(w, 200, response.Files)
}
