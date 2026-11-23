package control

import (
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strings"
	"time"
)

func setupCommand(origin, token string) string {
	return fmt.Sprintf("(setup=$(mktemp) && trap 'rm -f \"$setup\"' EXIT && curl --fail --silent --show-error --connect-timeout 10 --max-time 120 --proto '=https' -H 'Authorization: Bearer %s' '%s/install.sh' -o \"$setup\" && sudo sh \"$setup\")", token, origin)
}
func (s *Server) renewEnrollment(w http.ResponseWriter, r *http.Request) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var id string
	err = tx.QueryRow(r.Context(), "SELECT id FROM nodes WHERE id=$1 AND mode='ssh' FOR UPDATE", r.PathValue("id")).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	var rotating bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM node_identity_rotations WHERE node_id=$1 AND phase NOT IN ('succeeded','cancelled'))`, id).Scan(&rotating); err != nil {
		http.Error(w, "identity state unavailable", 503)
		return
	}
	if rotating {
		http.Error(w, "finish or cancel SSH identity rotation before renewing enrollment", 409)
		return
	}
	_, err = tx.Exec(r.Context(), "UPDATE enrollments SET consumed_at=now() WHERE node_id=$1 AND consumed_at IS NULL", id)
	token := randomToken()
	expiry := time.Now().Add(15 * time.Minute)
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO enrollments(token_hash,node_id,expires_at) VALUES($1,$2,$3)", tokenHash(token), id, expiry)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not renew enrollment", 500)
		return
	}
	respond(w, 201, map[string]any{"id": id, "expires_at": expiry, "setup_command": setupCommand(s.PublicURL, token)})
}
func (s *Server) updateNode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Address string `json:"address"`
		Port    int    `json:"port"`
	}
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 || in.Address == "" || len(in.Address) > 253 || strings.ContainsAny(in.Address, " \t\r\n/\\\"'") || in.Port < 1 || in.Port > 65535 {
		http.Error(w, "name, SSH address, and valid port required", 400)
		return
	}
	tag, err := s.DB.Exec(r.Context(), "UPDATE nodes SET name=$1,address=$2,port=$3,state=CASE WHEN host_key IS NULL THEN 'pending' ELSE 'enrolled' END WHERE id=$4 AND mode='ssh'", in.Name, in.Address, in.Port, r.PathValue("id"))
	if err != nil {
		http.Error(w, "could not update node", 500)
		return
	}
	if tag.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	respond(w, 200, map[string]string{"id": r.PathValue("id")})
}
