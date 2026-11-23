package control

import (
	"cove/internal/worker"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
)

func (s *Server) workloadRuntime(w http.ResponseWriter, r *http.Request) {
	s.readRuntime(w, r, "containers")
}
func (s *Server) workloadLogs(w http.ResponseWriter, r *http.Request) {
	s.readRuntime(w, r, "container_logs")
}
func (s *Server) workloadBackups(w http.ResponseWriter, r *http.Request) {
	s.readRuntime(w, r, "backups")
}
func (s *Server) workloadReleases(w http.ResponseWriter, r *http.Request) {
	s.readRuntime(w, r, "releases")
}
func (s *Server) readRuntime(w http.ResponseWriter, r *http.Request, method string) {
	var node string
	err := s.DB.QueryRow(r.Context(), "SELECT node_id FROM workloads WHERE id=$1", r.PathValue("id")).Scan(&node)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	response, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: method, ID: r.PathValue("id")})
	if err != nil {
		http.Error(w, "runtime information is unavailable; check node connectivity and deployment state", 502)
		return
	}
	if method == "releases" {
		if response.Releases == nil {
			response.Releases = []worker.Release{}
		}
		respond(w, 200, map[string]any{"releases": response.Releases})
		return
	}
	if method == "backups" {
		if response.Backups == nil {
			response.Backups = []worker.BackupRecord{}
		}
		respond(w, 200, map[string]any{"backups": response.Backups})
		return
	}
	if method == "containers" {
		if err := s.observePorts(r.Context(), r.PathValue("id"), response.Containers); err != nil {
			http.Error(w, "runtime security observation unavailable", 503)
			return
		}
		if response.Containers == nil {
			response.Containers = []worker.Container{}
		}
		respond(w, 200, map[string]any{"containers": response.Containers})
		return
	}
	respond(w, 200, map[string]string{"logs": response.Logs})
}
