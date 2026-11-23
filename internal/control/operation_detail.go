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
	var admitted bool
	var cancelRequested bool
	var approvalRequired bool
	var approvedAt *time.Time
	var reason, overrideReason string
	var overrideBy *string
	var overrideAt *time.Time
	err := s.DB.QueryRow(r.Context(), "SELECT id,workload_id,action,state,result,created_at,cancel_requested,approval_required,approved_at,reason,emergency_override_by,emergency_override_reason,emergency_override_at,admitted_at IS NOT NULL FROM operations WHERE id=$1", r.PathValue("id")).Scan(&id, &workload, &action, &state, &result, &created, &cancelRequested, &approvalRequired, &approvedAt, &reason, &overrideBy, &overrideReason, &overrideAt, &admitted)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	var maintenance bool
	if err = s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM database_maintenance WHERE id=$1)", id).Scan(&maintenance); err != nil {
		http.Error(w, "operation unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"can_cancel": !maintenance, "reason": reason, "emergency_override_by": overrideBy, "emergency_override_reason": overrideReason, "emergency_override_at": overrideAt, "can_override": s.teamAdmin(r, actor(r).Team) && state == "queued" && !admitted && !cancelRequested && overrideAt == nil && (action == "deploy" || action == "rollback"), "id": id, "workload_id": workload, "action": action, "state": state, "result": result, "created_at": created, "cancel_requested": cancelRequested, "approval_required": approvalRequired, "approved_at": approvedAt, "can_approve": s.teamAdmin(r, actor(r).Team)})
}
