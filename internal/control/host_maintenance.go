package control

import (
	"cove/internal/worker"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

func (s *Server) nodeHostMaintenance(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	options := worker.HostMaintenanceRequest{Action: "inspect"}
	if r.Method == http.MethodPut {
		var in struct {
			Enabled *bool `json:"enabled"`
			Confirm bool  `json:"confirm"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Enabled == nil || !in.Confirm {
			http.Error(w, "confirm the security-update schedule change", 400)
			return
		}
		options.Action, options.Enabled = "security_policy", *in.Enabled
	}
	var ready, capable, busy bool
	err := s.DB.QueryRow(r.Context(), `SELECT state='ready',COALESCE((capabilities->>'host_maintenance')::boolean,false),EXISTS(SELECT 1 FROM workloads w JOIN operations o ON o.workload_id=w.id WHERE w.node_id=n.id AND o.state IN ('queued','running')) FROM nodes n WHERE id=$1`, r.PathValue("id")).Scan(&ready, &capable, &busy)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "node maintenance unavailable", 503)
		return
	}
	if !ready || !capable || r.Method == http.MethodPut && busy {
		http.Error(w, "verify an upgraded Linux worker and wait for active operations before changing its schedule", 409)
		return
	}
	response, err := s.nodeRPC(r.Context(), r.PathValue("id"), worker.RPCRequest{Method: "host_maintenance", HostMaintenance: &options})
	if err != nil || response.Error != "" || response.HostPatches == nil {
		http.Error(w, "host maintenance request could not be confirmed; refresh before retrying", 502)
		return
	}
	respond(w, 200, response.HostPatches)
}
