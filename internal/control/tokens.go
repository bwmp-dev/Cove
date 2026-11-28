package control

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strings"
	"time"
)

func (s *Server) scopedToken(next http.Handler, w http.ResponseWriter, r *http.Request) {
	if route, ok := r.Context().Value(apiRouteKey{}).(publicRoute); ok {
		s.authenticatePublicToken(next, w, r, route)
		return
	}
	var id, workload, permission, userID, teamID string
	err := s.DB.QueryRow(r.Context(), "SELECT id,workload_id,permission,user_id,team_id FROM api_tokens WHERE token_hash=$1 AND expires_at>now()", tokenHash(bearer(r))).Scan(&id, &workload, &permission, &userID, &teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "unauthorized", 401)
		return
	}
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	target := ""
	switch r.Pattern {
	case "GET /api/workloads/{id}", "GET /api/workloads/{id}/runtime", "GET /api/workloads/{id}/logs", "GET /api/workloads/{id}/releases", "GET /api/workloads/{id}/backups":
		target = r.PathValue("id")
	case "POST /api/workloads/{id}/operations":
		if permission == "operate" {
			fields, ok := bodyFields(w, r)
			if !ok {
				return
			}
			if field(fields, "action") == "restore" || field(fields, "action") == "drill" {
				http.Error(w, "restore requires an editor session", 403)
				return
			}
			target = r.PathValue("id")
		}
	case "GET /api/operations":
		target = r.URL.Query().Get("workload_id")
	case "GET /api/operations/{id}", "GET /api/operations/{id}/logs", "POST /api/operations/{id}/cancel":
		if r.Method == "GET" || permission == "operate" {
			err = s.DB.QueryRow(r.Context(), "SELECT workload_id FROM operations WHERE id=$1", r.PathValue("id")).Scan(&target)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "authorization unavailable", 503)
				return
			}
		}
	}
	if target == "" || target != workload {
		http.Error(w, "token does not permit this action", 403)
		return
	}
	r = r.Clone(r.Context())
	r.Header.Set("X-Cove-Team", teamID)
	p, err := s.identity(r, userID)
	if err != nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), identityKey{}, p))
	s.serveAuthorized(next, w, r, "API token "+id)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string `json:"name"`
		Workload   string `json:"workload_id"`
		Permission string `json:"permission"`
		Days       int    `json:"days"`
	}
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 || (in.Permission != "read" && in.Permission != "operate") || in.Days < 1 || in.Days > 365 {
		http.Error(w, "name, workload, read/operate permission and expiry of 1-365 days are required", 400)
		return
	}
	var exists bool
	if err := s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM workloads WHERE id=$1)", in.Workload).Scan(&exists); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if !exists {
		http.Error(w, "workload not found", 404)
		return
	}
	id, token := resourceID(), randomToken()
	expiry := time.Now().Add(time.Duration(in.Days) * 24 * time.Hour)
	if _, err := s.DB.Exec(r.Context(), "INSERT INTO api_tokens(id,name,token_hash,workload_id,permission,expires_at,user_id,team_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", id, in.Name, tokenHash(token), in.Workload, in.Permission, expiry, actor(r).ID, actor(r).Team); err != nil {
		http.Error(w, "could not create token", 500)
		return
	}
	respond(w, 201, map[string]any{"id": id, "token": token, "expires_at": expiry})
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), "SELECT id,name,workload_id,permission,expires_at FROM api_tokens WHERE team_id=$1 AND (user_id=$2 OR $3) AND ($4 OR EXISTS(SELECT 1 FROM workloads w LEFT JOIN environments e ON e.id=w.environment_id WHERE w.id=api_tokens.workload_id AND cove_access($2,w.team_id,w.project_id,COALESCE(e.id,''))>0)) ORDER BY created_at DESC", actor(r).Team, actor(r).ID, s.teamAdmin(r, actor(r).Team), actor(r).Admin)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	type token struct {
		ID         string    `json:"id"`
		Name       string    `json:"name"`
		Workload   string    `json:"workload_id"`
		Permission string    `json:"permission"`
		Expires    time.Time `json:"expires_at"`
	}
	tokens := []token{}
	for rows.Next() {
		var v token
		if err = rows.Scan(&v.ID, &v.Name, &v.Workload, &v.Permission, &v.Expires); err != nil {
			http.Error(w, "could not read tokens", 500)
			return
		}
		tokens = append(tokens, v)
	}
	if rows.Err() != nil {
		http.Error(w, "could not read tokens", 500)
		return
	}
	respond(w, 200, tokens)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	result, err := s.DB.Exec(r.Context(), "DELETE FROM api_tokens WHERE id=$1", r.PathValue("id"))
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if result.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
