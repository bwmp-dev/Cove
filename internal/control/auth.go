package control

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
	"net"
	"net/http"
	"strings"
	"time"
)

func (s *Server) InitAdministrator(ctx context.Context, password string) error {
	var hash []byte
	err := s.DB.QueryRow(ctx, "SELECT password_hash FROM users WHERE id='bootstrap'").Scan(&hash)
	if err != nil {
		return err
	}
	if len(hash) > 0 {
		return nil
	}
	if password == "" {
		return nil
	}
	if len(password) < 16 || len(password) > 72 {
		return errors.New("initial COVE_ADMIN_PASSWORD must contain 16 to 72 bytes")
	}
	hash, err = bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var completed bool
	if err = tx.QueryRow(ctx, "SELECT setup_completed FROM control_settings WHERE id=1 FOR UPDATE").Scan(&completed); err != nil {
		return err
	}
	if completed {
		return nil
	}
	_, err = tx.Exec(ctx, "UPDATE users SET password_hash=$1 WHERE id='bootstrap' AND password_hash IS NULL", hash)
	if err == nil {
		_, err = tx.Exec(ctx, "UPDATE control_settings SET setup_completed=true WHERE id=1")
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	return err
}

func (s *Server) sameOrigin(r *http.Request) bool { return r.Header.Get("Origin") == s.PublicURL }

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "origin rejected", 403)
		return
	}
	var in struct {
		Password   string `json:"password"`
		Email      string `json:"email"`
		BreakGlass bool   `json:"break_glass"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Password) > 72 {
		http.Error(w, "invalid credentials", 401)
		return
	}
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		http.Error(w, "invalid client address", 400)
		return
	}
	if !s.limitAttempts(w, r, "login-burst:"+address, 100) || !s.limitAttempts(w, r, emailRateKey("login", strings.ToLower(strings.TrimSpace(in.Email))), 10) {
		return
	}
	var hash []byte
	var userID string
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	// Rotation waits for in-flight logins, then revokes every session they created.
	err = tx.QueryRow(r.Context(), "SELECT id,password_hash FROM users WHERE ((email=$1 AND email_verified) OR ($1='' AND id='bootstrap' AND email IS NULL)) AND NOT disabled FOR SHARE", strings.ToLower(strings.TrimSpace(in.Email))).Scan(&userID, &hash)
	hasPassword := len(hash) > 0
	if !hasPassword {
		hash = s.dummyPasswordHash
	}
	matches := bcrypt.CompareHashAndPassword(hash, []byte(in.Password)) == nil
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (!hasPassword || !matches) {
		http.Error(w, "invalid credentials", 401)
		return
	}
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	var disabled, admin bool
	if err = tx.QueryRow(r.Context(), "SELECT c.password_login_disabled,u.is_admin FROM control_settings c CROSS JOIN users u WHERE c.id=1 AND u.id=$1", userID).Scan(&disabled, &admin); err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	if disabled && !(in.BreakGlass && admin) {
		http.Error(w, "password login disabled", 403)
		return
	}
	token := randomToken()
	expiry := time.Now().Add(12 * time.Hour)
	_, err = tx.Exec(r.Context(), "INSERT INTO sessions(token_hash,expires_at,user_id,user_agent) VALUES($1,$2,$3,$4)", tokenHash(token), expiry, userID, sessionUserAgent(r))
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	s.completeSignIn(w, r, userID, token, expiry)
	var required bool
	if s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM account_totp WHERE user_id=$1 AND confirmed) OR EXISTS(SELECT 1 FROM passkeys WHERE user_id=$1)", userID).Scan(&required) != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"user": userID, "second_factor_required": required})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "origin rejected", 403)
		return
	}
	if c, err := r.Cookie("__Host-cove"); err == nil {
		if _, err = s.DB.Exec(r.Context(), "DELETE FROM sessions WHERE token_hash=$1", tokenHash(c.Value)); err != nil {
			http.Error(w, "logout failed", 503)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	respond(w, 200, map[string]bool{"ok": true})
}
