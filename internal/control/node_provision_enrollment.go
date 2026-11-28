package control

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

func (s *Server) provisionVerifierMissing(w http.ResponseWriter, r *http.Request) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "enrollment report unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var node string
	err = tx.QueryRow(r.Context(), `UPDATE enrollments e SET consumed_at=now() WHERE token_hash=$1 AND consumed_at IS NULL AND expires_at>now() AND EXISTS(SELECT 1 FROM node_provisions p WHERE p.node_id=e.node_id AND p.phase IN ('creating','server_pending','enrolling')) RETURNING node_id`, tokenHash(bearer(r))).Scan(&node)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "invalid or expired cloud enrollment", 401)
		return
	}
	if err != nil {
		http.Error(w, "enrollment report unavailable", 503)
		return
	}
	tag, err := tx.Exec(r.Context(), `UPDATE node_provisions SET phase='uncertain',last_error='Trusted Cosign v3+ is missing from the provider image. Worker enrollment was refused and its token revoked. The paid server was retained; install a trusted verifier and renew enrollment, or delete the server in its provider console. Creation will not be repeated.',updated_at=now() WHERE node_id=$1 AND phase IN ('creating','server_pending','enrolling')`, node)
	if err != nil || tag.RowsAffected() != 1 {
		http.Error(w, "cloud enrollment changed; inspect provisioning history", 409)
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id) SELECT 'node enrollment','cloud enrollment verifier missing',$1,409,team_id FROM node_provisions WHERE node_id=$1`, node)
	if err != nil || tx.Commit(r.Context()) != nil {
		http.Error(w, "could not record enrollment prerequisite failure", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
