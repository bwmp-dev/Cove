package control

import (
	"cove/internal/worker"
	"net/http"
	"time"
)

func (s *Server) workloadExec(w http.ResponseWriter, r *http.Request) {
	if !s.requirePermission(w, r, "workload", r.PathValue("id"), "read_secrets") {
		return
	}
	id := r.PathValue("id")
	if !s.requireAccess(w, r, "workload", id, 3) {
		return
	}
	var in worker.ExecRequest
	if !decode(w, r, &in) {
		return
	}
	if in.Workload != "" && in.Workload != id {
		http.Error(w, "resource does not match request", 400)
		return
	}
	in.Workload = id
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = 30
	}
	if err := worker.ValidateExec(in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var node string
	var supported, busy bool
	if err := s.DB.QueryRow(r.Context(), `SELECT n.id,COALESCE((n.capabilities->>'exec')::boolean,false),EXISTS(SELECT 1 FROM operations WHERE workload_id=w.id AND state IN ('queued','running')) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, id).Scan(&node, &supported, &busy); err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	if !supported || busy {
		http.Error(w, "upgrade and verify the worker, and wait for active operations before executing a command", 409)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(40 * time.Second))
	result, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "exec", Exec: &in})
	if err != nil || result.Exec == nil {
		http.Error(w, "command unavailable; check the selected service, container and worker", 502)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, result.Exec)
}
