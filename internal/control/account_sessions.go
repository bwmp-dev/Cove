package control

import (
	"net/http"
	"strings"
	"time"
)

func sessionUserAgent(r *http.Request) string {
	agent := []rune(strings.Map(func(c rune) rune {
		if c < 32 || c == 127 {
			return -1
		}
		return c
	}, r.UserAgent()))
	if len(agent) > 256 {
		agent = agent[:256]
	}
	return string(agent)
}

func (s *Server) listAccountSessions(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("__Host-cove")
	if err != nil {
		http.Error(w, "sign in to manage sessions", 401)
		return
	}
	rows, err := s.DB.Query(r.Context(), `SELECT id,expires_at,user_agent,token_hash=$2 FROM sessions WHERE user_id=$1 AND expires_at>now() ORDER BY expires_at DESC`, actor(r).ID, tokenHash(cookie.Value))
	if err != nil {
		http.Error(w, "sessions unavailable", 503)
		return
	}
	defer rows.Close()
	type session struct {
		ID        string    `json:"id"`
		ExpiresAt time.Time `json:"expires_at"`
		UserAgent string    `json:"user_agent"`
		Current   bool      `json:"current"`
	}
	sessions := []session{}
	for rows.Next() {
		var v session
		if err = rows.Scan(&v.ID, &v.ExpiresAt, &v.UserAgent, &v.Current); err != nil {
			http.Error(w, "sessions unavailable", 503)
			return
		}
		sessions = append(sessions, v)
	}
	if rows.Err() != nil {
		http.Error(w, "sessions unavailable", 503)
		return
	}
	respond(w, 200, sessions)
}

func (s *Server) revokeAccountSession(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("__Host-cove")
	if err != nil {
		http.Error(w, "sign in to manage sessions", 401)
		return
	}
	// Keep this browser signed in; its existing Sign out action handles its cookie.
	result, err := s.DB.Exec(r.Context(), `DELETE FROM sessions WHERE id=$1 AND user_id=$2 AND token_hash<>$3`, r.PathValue("id"), actor(r).ID, tokenHash(cookie.Value))
	if err != nil {
		http.Error(w, "could not revoke session", 503)
		return
	}
	if result.RowsAffected() == 0 {
		http.Error(w, "session not found or is the current session", 404)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
