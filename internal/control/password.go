package control

import (
	"golang.org/x/crypto/bcrypt"
	"net/http"
)

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Current) > 72 || len(in.New) < 16 || len(in.New) > 72 {
		http.Error(w, "new password must contain 16 to 72 bytes", 400)
		return
	}
	if !s.limitAttempts(w, r, "password-change:"+actor(r).ID, 5) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var hash []byte
	if err = tx.QueryRow(r.Context(), "SELECT password_hash FROM users WHERE id=$1 AND NOT disabled FOR UPDATE", actor(r).ID).Scan(&hash); err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(in.Current)) != nil {
		http.Error(w, "current password is incorrect", 400)
		return
	}
	hash, err = bcrypt.GenerateFromPassword([]byte(in.New), 12)
	if err != nil {
		http.Error(w, "could not protect password", 500)
		return
	}
	if _, err = tx.Exec(r.Context(), "UPDATE users SET password_hash=$1,auth_version=auth_version+1 WHERE id=$2", hash, actor(r).ID); err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM sessions WHERE user_id=$1", actor(r).ID)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM account_actions WHERE user_id=$1", actor(r).ID)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM oauth_states WHERE link_user_id=$1", actor(r).ID)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not change password", 500)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	respond(w, 200, map[string]bool{"ok": true})
}
