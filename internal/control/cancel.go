package control

import "net/http"

func (s *Server) cancelOperation(w http.ResponseWriter, r *http.Request) {
	var maintenance bool
	if err := s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM database_maintenance WHERE id=$1)", r.PathValue("id")).Scan(&maintenance); err != nil {
		http.Error(w, "operation unavailable", 503)
		return
	}
	if maintenance {
		http.Error(w, "database maintenance must finish or be retried", 409)
		return
	}
	result, err := s.DB.Exec(r.Context(), "UPDATE operations SET cancel_requested=true,updated_at=now() WHERE id=$1 AND state IN ('queued','running')", r.PathValue("id"))
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if result.RowsAffected() == 0 {
		http.Error(w, "operation is no longer active or does not exist", 409)
		return
	}
	respond(w, 202, map[string]string{"status": "cancellation requested"})
}
