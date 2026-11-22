package control

import "net/http"

func (s *Server) cancelOperation(w http.ResponseWriter, r *http.Request) {
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
