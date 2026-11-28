package control

import (
	"context"
	"cove/internal/worker"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

func (s *Server) gpuInventory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	node := r.PathValue("id")
	if r.Pattern == "GET /api/workloads/{id}/gpus" {
		if !s.requirePermission(w, r, "workload", r.PathValue("id"), "edit_configuration") {
			return
		}
		if err := s.DB.QueryRow(r.Context(), `SELECT node_id FROM workloads WHERE id=$1`, node).Scan(&node); err != nil {
			http.Error(w, "resource GPU placement unavailable", 503)
			return
		}
	}
	var ready, capable bool
	err := s.DB.QueryRow(r.Context(), `SELECT state='ready',COALESCE((capabilities->>'gpu_assignments')::boolean,false) FROM nodes WHERE id=$1`, node).Scan(&ready, &capable)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "GPU inventory unavailable", 503)
		return
	}
	if !ready || !capable {
		http.Error(w, "verify an upgraded Linux worker before inspecting GPUs", 409)
		return
	}
	response, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "gpus"})
	if err != nil || response.Error != "" || response.GPUs == nil {
		http.Error(w, "GPU inspection unavailable; check the node's native drivers and worker privileges", 502)
		return
	}
	respond(w, 200, response.GPUs)
}

func (s *Server) prepareGPUAssignments(ctx context.Context, request *worker.Request) error {
	if request.Action != "deploy" {
		return nil
	}
	configured := false
	for _, limit := range request.Limits {
		configured = configured || limit.GPUs != nil
	}
	if !configured {
		return nil
	}
	var capable bool
	if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'gpu_assignments')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, request.Workload).Scan(&capable); err != nil {
		return err
	}
	if !capable {
		return errors.New("upgrade and verify the worker before deploying GPU assignments")
	}
	return nil
}
