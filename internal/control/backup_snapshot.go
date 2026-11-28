package control

import (
	"cove/internal/worker"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
)

func (s *Server) backupSnapshotPreview(w http.ResponseWriter, r *http.Request) {
	var options worker.VolumeSnapshotConfig
	if value := r.URL.Query().Get("max_bytes"); value != "" {
		bytes, err := strconv.ParseInt(value, 10, 64)
		if err != nil || bytes < 64<<20 || bytes > worker.MaxTransferBytes {
			http.Error(w, "snapshot size must be 64 MiB–64 GiB", 400)
			return
		}
		options.MaxBytes = bytes
	}
	var node, state string
	var supported, busy bool
	err := s.DB.QueryRow(r.Context(), `SELECT n.id,n.state,COALESCE((n.capabilities->>'filesystem_snapshots')::boolean,false),EXISTS(SELECT 1 FROM operations WHERE workload_id=w.id AND state IN ('queued','running')) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.PathValue("id")).Scan(&node, &state, &supported, &busy)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if !supported || state != "ready" || busy {
		http.Error(w, "verify an upgraded worker and wait for active operations before inspecting snapshots", 409)
		return
	}
	response, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "snapshot_preview", ID: r.PathValue("id"), Snapshot: &options})
	if err != nil || response.Error != "" || response.Snapshot == nil {
		http.Error(w, "snapshot inspection unavailable; check worker tools, privileges and deployment state", 502)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, response.Snapshot)
}
