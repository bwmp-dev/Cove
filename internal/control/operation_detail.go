package control

import (
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"time"
)

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	var id, workload, action, state, executionNode string
	var result json.RawMessage
	var created time.Time
	var cancelRequested bool
	var approvalRequired bool
	var approvedAt *time.Time
	err := s.DB.QueryRow(r.Context(), "SELECT id,workload_id,action,state,result,created_at,cancel_requested,approval_required,approved_at,CASE WHEN action='build' OR (action='image_scan' AND execution_node_id IS NOT NULL) THEN COALESCE(execution_node_id,'') ELSE (SELECT node_id FROM workloads WHERE id=operations.workload_id) END FROM operations WHERE id=$1", r.PathValue("id")).Scan(&id, &workload, &action, &state, &result, &created, &cancelRequested, &approvalRequired, &approvedAt, &executionNode)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"execution_node_id": executionNode, "id": id, "workload_id": workload, "action": action, "state": state, "result": result, "created_at": created, "cancel_requested": cancelRequested, "approval_required": approvalRequired, "approved_at": approvedAt, "can_approve": s.teamAdmin(r, actor(r).Team)})
}
