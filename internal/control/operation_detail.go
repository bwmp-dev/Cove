package control

import (
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"time"
)

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	var id, workload, action, state string
	var result json.RawMessage
	var created time.Time
	var cancelRequested bool
	var approvalRequired bool
	var approvedAt *time.Time
	err := s.DB.QueryRow(r.Context(), "SELECT id,workload_id,action,state,result,created_at,cancel_requested,approval_required,approved_at FROM operations WHERE id=$1", r.PathValue("id")).Scan(&id, &workload, &action, &state, &result, &created, &cancelRequested, &approvalRequired, &approvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	respond(w, 200, operationView{ID: id, WorkloadID: workload, Action: action, State: state, Result: result, CreatedAt: created, CancelRequested: cancelRequested, ApprovalRequired: approvalRequired, ApprovedAt: approvedAt, CanApprove: s.teamAdmin(r, actor(r).Team)})
}
