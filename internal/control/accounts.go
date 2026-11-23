package control

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"cove/internal/outbound"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

func normalizeEmail(raw string) (string, bool) {
	email := strings.ToLower(strings.TrimSpace(raw))
	a, err := mail.ParseAddress(email)
	return email, err == nil && a.Address == email && len(email) <= 254 && !strings.ContainsAny(email, "\r\n")
}
func passwordHash(password string) ([]byte, bool) {
	if len(password) < 16 || len(password) > 72 {
		return nil, false
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	return hash, err == nil
}
func setSession(w http.ResponseWriter, token string, expiry time.Time) {
	cookies := w.Header().Values("Set-Cookie")
	w.Header().Del("Set-Cookie")
	defer func() {
		for _, cookie := range cookies {
			w.Header().Add("Set-Cookie", cookie)
		}
	}()
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove", Value: token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, Expires: expiry})
}
func (s *Server) completeSignIn(w http.ResponseWriter, r *http.Request, user, token string, expiry time.Time) {
	s.githubTeamSignIn(r.Context(), user)
	setSession(w, token, expiry)
}
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, user string) {
	token := randomToken()
	expiry := time.Now().Add(12 * time.Hour)
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var id string
	err = tx.QueryRow(r.Context(), "SELECT id FROM users WHERE id=$1 AND NOT disabled AND email_verified FOR SHARE", user).Scan(&id)
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO sessions(token_hash,expires_at,user_id,user_agent) VALUES($1,$2,$3,$4)", tokenHash(token), expiry, id, sessionUserAgent(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	s.completeSignIn(w, r, user, token, expiry)
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) authLimit(w http.ResponseWriter, r *http.Request) bool {
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		http.Error(w, "invalid client address", 400)
		return false
	}
	return s.limitAttempts(w, r, "accounts:"+address, 60)
}
func (s *Server) limitAttempts(w http.ResponseWriter, r *http.Request, key string, maximum int) bool {
	var attempts int
	err := s.DB.QueryRow(r.Context(), `INSERT INTO login_attempts(address,attempts,window_start) VALUES($1,1,now()) ON CONFLICT(address) DO UPDATE SET attempts=CASE WHEN login_attempts.window_start<now()-interval '1 minute' THEN 1 ELSE login_attempts.attempts+1 END,window_start=CASE WHEN login_attempts.window_start<now()-interval '1 minute' THEN now() ELSE login_attempts.window_start END RETURNING attempts`, key).Scan(&attempts)
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return false
	}
	if attempts > maximum {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many attempts", 429)
		return false
	}
	return true
}
func emailRateKey(purpose, email string) string {
	return purpose + ":" + hex.EncodeToString(tokenHash(email))
}

func (s *Server) publicAccount(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sameOrigin(r) {
			http.Error(w, "origin rejected", 403)
			return
		}
		if !s.authLimit(w, r) {
			return
		}
		handler := next
		if r.URL.Path == "/auth/second-factor" || r.URL.Path == "/auth/forgot-password" || r.URL.Path == "/auth/reset-password" || r.URL.Path == "/auth/passkeys/finish" || strings.HasPrefix(r.URL.Path, "/auth/ldap/") {
			handler = s.loginAttempt(next)
		}
		s.authenticated(handler, w, r, "account authentication")
	}
}

const setupRequiredSQL = "NOT setup_completed AND NOT EXISTS(SELECT 1 FROM users WHERE id<>'bootstrap' OR password_hash IS NOT NULL)"

func (s *Server) authConfig(w http.ResponseWriter, r *http.Request) {
	var enabled, setup bool
	if s.DB.QueryRow(r.Context(), "SELECT registration_enabled,"+setupRequiredSQL+" FROM control_settings WHERE id=1").Scan(&enabled, &setup) != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	config, _, err := s.integrationConfig(r.Context(), s.DB)
	if err != nil {
		http.Error(w, "settings unavailable", 503)
		return
	}
	sendMail, err := s.mailer(r.Context())
	if err != nil {
		http.Error(w, "email settings unavailable", 503)
		return
	}
	respond(w, 200, map[string]bool{"setup_required": setup, "registration_enabled": setup || enabled && sendMail != nil, "github_enabled": config.Github.ClientID != "" && config.Github.ClientSecret != "", "email_enabled": sendMail != nil})
}
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		s.authConfig(w, r)
		return
	}
	var in struct {
		Enabled bool `json:"registration_enabled"`
	}
	if !decode(w, r, &in) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "settings unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var id int
	if err = tx.QueryRow(r.Context(), "SELECT id FROM control_settings WHERE id=1 FOR UPDATE").Scan(&id); err != nil {
		http.Error(w, "settings unavailable", 503)
		return
	}
	config, saved, err := s.integrationConfig(r.Context(), tx)
	if err != nil {
		http.Error(w, "settings unavailable", 503)
		return
	}
	if in.Enabled && config.SMTP.Host == "" && (saved || s.SendMail == nil) {
		http.Error(w, "configure email delivery before enabling registration", 409)
		return
	}
	_, err = tx.Exec(r.Context(), "UPDATE control_settings SET registration_enabled=$1 WHERE id=1", in.Enabled)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not update settings", 500)
		return
	}
	s.authConfig(w, r)
}
func (s *Server) registerAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	email, valid := normalizeEmail(in.Email)
	if !valid || strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 || len(in.Password) < 16 || len(in.Password) > 72 {
		http.Error(w, "valid email, name and password of 16 to 72 bytes required", 400)
		return
	}
	if !s.limitAttempts(w, r, emailRateKey("registration", email), 5) {
		return
	}
	sendMail, err := s.mailer(r.Context())
	if err != nil {
		http.Error(w, "email settings unavailable", 503)
		return
	}
	hash, valid := passwordHash(in.Password)
	if !valid {
		http.Error(w, "could not protect password", 500)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "registration unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var enabled, setup bool
	if err = tx.QueryRow(r.Context(), "SELECT registration_enabled,"+setupRequiredSQL+" FROM control_settings WHERE id=1 FOR UPDATE").Scan(&enabled, &setup); err != nil {
		http.Error(w, "registration unavailable", 503)
		return
	}
	if setup {
		token := randomToken()
		expiry := time.Now().Add(12 * time.Hour)
		_, err = tx.Exec(r.Context(), "UPDATE users SET email=$1,name=$2,password_hash=$3,email_verified=true,is_admin=true WHERE id='bootstrap'", email, strings.TrimSpace(in.Name), hash)
		if err == nil {
			_, err = tx.Exec(r.Context(), "INSERT INTO team_members(team_id,user_id,role) VALUES('default','bootstrap','owner') ON CONFLICT DO NOTHING")
		}
		if err == nil {
			_, err = tx.Exec(r.Context(), "UPDATE control_settings SET setup_completed=true,registration_enabled=false WHERE id=1")
		}
		if err == nil {
			_, err = tx.Exec(r.Context(), "INSERT INTO sessions(token_hash,expires_at,user_id,user_agent) VALUES($1,$2,'bootstrap',$3)", tokenHash(token), expiry, sessionUserAgent(r))
		}
		if err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			http.Error(w, "setup could not be completed", 503)
			return
		}
		s.completeSignIn(w, r, "bootstrap", token, expiry)
		respond(w, 200, map[string]bool{"ok": true, "setup_complete": true})
		return
	}
	if !enabled || sendMail == nil {
		http.Error(w, "registration is closed", 403)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "registration unavailable", 503)
		return
	}
	token := randomToken()
	res, err := s.DB.Exec(r.Context(), "INSERT INTO account_actions(token_hash,purpose,email,name,password_hash,expires_at) SELECT $1,'register',$2,$3,$4,now()+interval '1 hour' WHERE NOT EXISTS(SELECT 1 FROM users WHERE email=$2)", tokenHash(token), email, strings.TrimSpace(in.Name), hash)
	if err != nil {
		http.Error(w, "registration unavailable", 503)
		return
	}
	if res.RowsAffected() > 0 {
		if err = sendMail(r.Context(), email, "Verify your Cove account", "Verify your email within one hour:\n\n"+s.PublicURL+"/verify#"+token); err != nil {
			_, _ = s.DB.Exec(r.Context(), "DELETE FROM account_actions WHERE token_hash=$1", tokenHash(token))
			slog.Error("account verification email delivery failed")
		}
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) verifyAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &in) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var email, name, purpose string
	var user *string
	var hash []byte
	err = tx.QueryRow(r.Context(), "DELETE FROM account_actions WHERE token_hash=$1 AND purpose IN ('register','email') AND expires_at>now() RETURNING purpose,user_id,email,name,password_hash", tokenHash(in.Token)).Scan(&purpose, &user, &email, &name, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "invalid or expired verification", 400)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if purpose == "register" {
		var enabled bool
		err = tx.QueryRow(r.Context(), "SELECT registration_enabled FROM control_settings WHERE id=1 FOR SHARE").Scan(&enabled)
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		if !enabled {
			http.Error(w, "registration is closed", 403)
			return
		}
		id := resourceID()
		user = &id
		_, err = tx.Exec(r.Context(), "INSERT INTO users(id,email,name,password_hash,email_verified) VALUES($1,$2,$3,$4,true)", id, email, name, hash)
	} else {
		var res int
		err = tx.QueryRow(r.Context(), "UPDATE users SET email=$1,email_verified=true,auth_version=auth_version+1 WHERE id=$2 AND NOT disabled RETURNING 1", email, user).Scan(&res)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM account_actions WHERE email=$1 OR user_id=$2", email, user)
	}
	if err == nil && purpose == "email" {
		_, err = tx.Exec(r.Context(), "DELETE FROM sessions WHERE user_id=$1", user)
	}
	if err == nil && purpose == "email" {
		_, err = tx.Exec(r.Context(), "DELETE FROM oauth_states WHERE link_user_id=$1", user)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "verification could not be completed", 409)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) createReset(r *http.Request, user string) (string, time.Time, error) {
	token := randomToken()
	expiry := time.Now().Add(time.Hour)
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		return "", expiry, err
	}
	defer tx.Rollback(r.Context())
	var email string
	err = tx.QueryRow(r.Context(), "SELECT COALESCE(email,'') FROM users WHERE id=$1 AND NOT disabled FOR UPDATE", user).Scan(&email)
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM account_actions WHERE user_id=$1 AND purpose='reset'", user)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO account_actions(token_hash,purpose,user_id,email,expires_at) VALUES($1,'reset',$2,$3,$4)", tokenHash(token), user, email, expiry)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	return s.PublicURL + "/reset-password#" + token, expiry, err
}
func (s *Server) forgotPassword(w http.ResponseWriter, r *http.Request) {
	sendMail, mailErr := s.mailer(r.Context())
	if mailErr != nil {
		http.Error(w, "email settings unavailable", 503)
		return
	}
	var in struct {
		Email string `json:"email"`
	}
	if !decode(w, r, &in) {
		return
	}
	email, valid := normalizeEmail(in.Email)
	if valid && !s.limitAttempts(w, r, emailRateKey("reset", email), 5) {
		return
	}
	if valid && sendMail != nil {
		var id string
		err := s.DB.QueryRow(r.Context(), "SELECT id FROM users WHERE email=$1 AND email_verified AND NOT disabled", email).Scan(&id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "authentication unavailable", 503)
			return
		}
		if err == nil {
			url, _, e := s.createReset(r, id)
			if e != nil {
				http.Error(w, "authentication unavailable", 503)
				return
			}
			if e = sendMail(r.Context(), email, "Reset your Cove password", "Reset your password within one hour:\n\n"+url); e != nil {
				slog.Error("password reset email delivery failed")
			}
		}
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	hash, ok := passwordHash(in.Password)
	if !ok {
		http.Error(w, "password must contain 16 to 72 bytes", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var user string
	err = tx.QueryRow(r.Context(), "SELECT u.id FROM users u JOIN account_actions a ON a.user_id=u.id WHERE a.token_hash=$1 AND a.purpose='reset' AND a.expires_at>now() AND NOT u.disabled FOR UPDATE OF u", tokenHash(in.Token)).Scan(&user)
	if err == nil {
		err = tx.QueryRow(r.Context(), "DELETE FROM account_actions WHERE token_hash=$1 AND purpose='reset' AND expires_at>now() RETURNING user_id", tokenHash(in.Token)).Scan(&user)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "invalid or expired reset", 400)
		return
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "UPDATE users SET password_hash=$1,auth_version=auth_version+1 WHERE id=$2", hash, user)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM sessions WHERE user_id=$1", user)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM account_actions WHERE user_id=$1", user)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM oauth_states WHERE link_user_id=$1", user)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not reset password", 500)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) updateAccount(w http.ResponseWriter, r *http.Request) {
	sendMail, mailErr := s.mailer(r.Context())
	if mailErr != nil {
		http.Error(w, "email settings unavailable", 503)
		return
	}
	if !s.authLimit(w, r) {
		return
	}
	var in struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"current_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	email, ok := normalizeEmail(in.Email)
	if email == "" && actor(r).Email == "" {
		ok = true
	}
	if !ok || strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 || len(in.Password) > 72 {
		http.Error(w, "valid name and email required", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "account unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var hash []byte
	var currentEmail string
	if tx.QueryRow(r.Context(), "SELECT password_hash,COALESCE(email,'') FROM users WHERE id=$1 AND NOT disabled FOR UPDATE", actor(r).ID).Scan(&hash, &currentEmail) != nil {
		http.Error(w, "account unavailable", 503)
		return
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(in.Password)) != nil {
		http.Error(w, "current password is incorrect", 400)
		return
	}
	if email != currentEmail && sendMail == nil {
		http.Error(w, "configure email delivery to verify an email change", 409)
		return
	}
	if _, err := tx.Exec(r.Context(), "UPDATE users SET name=$1 WHERE id=$2", strings.TrimSpace(in.Name), actor(r).ID); err != nil {
		http.Error(w, "could not update account", 500)
		return
	}
	token := randomToken()
	if email != currentEmail {
		if _, err = tx.Exec(r.Context(), "DELETE FROM account_actions WHERE user_id=$1 AND purpose='email'", actor(r).ID); err != nil {
			http.Error(w, "account unavailable", 503)
			return
		}
		_, err := tx.Exec(r.Context(), "INSERT INTO account_actions(token_hash,purpose,user_id,email,expires_at) VALUES($1,'email',$2,$3,now()+interval '1 hour')", tokenHash(token), actor(r).ID, email)
		if err != nil {
			http.Error(w, "could not verify email", 500)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not update account", 500)
		return
	}
	if email != currentEmail {
		if err = sendMail(r.Context(), email, "Verify your Cove email", "Verify your new email within one hour:\n\n"+s.PublicURL+"/verify#"+token); err != nil {
			http.Error(w, "email delivery unavailable", 503)
			return
		}
	}
	respond(w, 200, map[string]bool{"ok": true, "verification_required": email != currentEmail})
}
func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), "SELECT id,COALESCE(email,''),name,is_admin,disabled FROM users ORDER BY created_at")
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		defer rows.Close()
		type user struct {
			ID       string `json:"id"`
			Email    string `json:"email"`
			Name     string `json:"name"`
			Admin    bool   `json:"is_admin"`
			Disabled bool   `json:"disabled"`
		}
		out := []user{}
		for rows.Next() {
			var u user
			if rows.Scan(&u.ID, &u.Email, &u.Name, &u.Admin, &u.Disabled) != nil {
				http.Error(w, "could not read users", 500)
				return
			}
			out = append(out, u)
		}
		if rows.Err() != nil {
			http.Error(w, "could not read users", 500)
			return
		}
		respond(w, 200, out)
		return
	}
	if r.Method == "POST" {
		url, expiry, err := s.createReset(r, r.PathValue("id"))
		if err != nil {
			http.Error(w, "could not generate reset", 409)
			return
		}
		respond(w, 201, map[string]any{"url": url, "expires_at": expiry})
		return
	}
	var in struct {
		Disabled bool `json:"disabled"`
		Admin    bool `json:"is_admin"`
	}
	if !decode(w, r, &in) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(1129272902)")
	var wasAdmin, wasDisabled bool
	if err == nil {
		err = tx.QueryRow(r.Context(), "SELECT is_admin,disabled FROM users WHERE id=$1 FOR UPDATE", r.PathValue("id")).Scan(&wasAdmin, &wasDisabled)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err == nil && wasAdmin && !wasDisabled && (!in.Admin || in.Disabled) {
		var count int
		err = tx.QueryRow(r.Context(), "SELECT count(*) FROM users WHERE is_admin AND NOT disabled").Scan(&count)
		if err == nil && count < 2 {
			http.Error(w, "instance must keep an active administrator", 409)
			return
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "UPDATE users SET is_admin=$1,disabled=$2,auth_version=auth_version+1 WHERE id=$3", in.Admin, in.Disabled, r.PathValue("id"))
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "UPDATE scim_users SET disabled_by_scim=false WHERE id=$1", r.PathValue("id"))
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM sessions WHERE user_id=$1", r.PathValue("id"))
	}
	if err == nil && in.Disabled {
		err = revokeSCIMAccess(r.Context(), tx, r.PathValue("id"))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not update user", 500)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func SMTPMailer(host, port, user, password, from string, selected ...outbound.Config) (func(context.Context, string, string, string) error, error) {
	if host == "" && port == "" && user == "" && password == "" && from == "" {
		return nil, nil
	}
	sender, ok := normalizeEmail(from)
	if !ok || host == "" || strings.ContainsAny(host, "\r\n") || port == "" || (user == "") != (password == "") {
		return nil, errors.New("complete SMTP host, port, sender, and optional credentials required")
	}
	return func(ctx context.Context, to, subject, body string) error {
		if _, valid := normalizeEmail(to); !valid || strings.ContainsAny(subject, "\r\n") {
			return errors.New("invalid email headers")
		}
		proxy := outbound.Environment()
		if len(selected) > 0 {
			proxy = selected[0]
		}
		address := net.JoinHostPort(host, port)
		via, err := proxy.For("https", address)
		if err != nil {
			return err
		}
		conn, err := outbound.Tunnel(ctx, via, "tcp", address)
		if err != nil {
			return err
		}
		defer conn.Close()
		deadline := time.Now().Add(20 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if err = conn.SetDeadline(deadline); err != nil {
			return err
		}
		config := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
		if port == "465" {
			secure := tls.Client(conn, config)
			if err = secure.HandshakeContext(ctx); err != nil {
				return err
			}
			conn = secure
		}
		client, err := smtp.NewClient(conn, host)
		if err != nil {
			return err
		}
		defer client.Close()
		if port != "465" {
			if ok, _ := client.Extension("STARTTLS"); !ok {
				return errors.New("SMTP requires TLS")
			}
			if err = client.StartTLS(config); err != nil {
				return err
			}
		}
		if user != "" {
			if err = client.Auth(smtp.PlainAuth("", user, password, host)); err != nil {
				return err
			}
		}
		if err = client.Mail(sender); err != nil {
			return err
		}
		if err = client.Rcpt(to); err != nil {
			return err
		}
		data, err := client.Data()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(data, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s", sender, to, subject, strings.ReplaceAll(body, "\n", "\r\n"))
		if err != nil {
			_ = data.Close()
			return err
		}
		if err = data.Close(); err != nil {
			return err
		}
		return client.Quit()
	}, nil
}
