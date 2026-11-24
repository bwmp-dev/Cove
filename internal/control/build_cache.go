package control

import (
	"cove/internal/worker"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

func (s *Server) buildCache(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("id")
	var supported bool
	err := s.DB.QueryRow(r.Context(), `SELECT COALESCE((capabilities->>'build_cache_limits')::boolean,false) FROM nodes WHERE id=$1`, node).Scan(&supported)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "worker capabilities unavailable", 503)
		return
	}
	if !supported {
		http.Error(w, "upgrade and verify the worker before configuring hard build-cache limits", 409)
		return
	}
	var active string
	if err = s.DB.QueryRow(r.Context(), `SELECT COALESCE((SELECT o.id FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE COALESCE(o.execution_node_id,w.node_id)=$1 AND o.state IN ('queued','running') ORDER BY o.created_at LIMIT 1),'')`, node).Scan(&active); err != nil {
		http.Error(w, "node operations unavailable", 503)
		return
	}
	request := worker.RPCRequest{Method: "build_cache"}
	if r.Method == "PUT" {
		var settings worker.BuildCacheSettings
		if !decode(w, r, &settings) {
			return
		}
		if active != "" {
			http.Error(w, "wait for active node operations before changing build-cache settings", 409)
			return
		}
		request.Method, request.BuildCache = "build_cache_configure", &settings
	}
	response, err := s.nodeRPC(r.Context(), node, request)
	var remote *RemoteError
	if errors.As(err, &remote) {
		if remote.Message == "unsupported method" {
			http.Error(w, "upgrade and verify the worker before configuring hard build-cache limits", 409)
		} else {
			http.Error(w, remote.Message, 409)
		}
		return
	}
	if err != nil {
		http.Error(w, "worker unavailable; build-cache verification could not be read or completed", 502)
		return
	}
	if response.BuildCache == nil {
		http.Error(w, "worker did not return build-cache verification; upgrade and verify it", 409)
		return
	}
	response.BuildCache.CanManage, response.BuildCache.ActiveOperationID = actor(r).Admin, active
	respond(w, 200, response.BuildCache)
}
