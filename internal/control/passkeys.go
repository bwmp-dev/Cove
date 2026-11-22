package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	wa "github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

type passkeyUser struct {
	ID, Email, Name string
	Credentials     []wa.Credential
}

func (u *passkeyUser) WebAuthnID() []byte                   { return []byte(u.ID) }
func (u *passkeyUser) WebAuthnName() string                 { return u.Email }
func (u *passkeyUser) WebAuthnDisplayName() string          { return u.Name }
func (u *passkeyUser) WebAuthnCredentials() []wa.Credential { return u.Credentials }
func (s *Server) webauthn() (*wa.WebAuthn, error) {
	origin, err := url.Parse(s.PublicURL)
	if err != nil {
		return nil, err
	}
	return wa.New(&wa.Config{RPID: origin.Hostname(), RPDisplayName: "Cove", RPOrigins: []string{s.PublicURL}, AuthenticatorSelection: protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementRequired, UserVerification: protocol.VerificationRequired}})
}
func loadPasskeyUser(ctx context.Context, tx pgx.Tx, id string) (*passkeyUser, int64, error) {
	u := &passkeyUser{Credentials: []wa.Credential{}}
	var version int64
	err := tx.QueryRow(ctx, "SELECT id,COALESCE(email,''),name,auth_version FROM users WHERE id=$1 AND NOT disabled AND email_verified FOR UPDATE", id).Scan(&u.ID, &u.Email, &u.Name, &version)
	if err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(ctx, "SELECT credential FROM passkeys WHERE user_id=$1 ORDER BY id FOR UPDATE", id)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var c wa.Credential
		if err = rows.Scan(&c); err != nil {
			return nil, 0, err
		}
		u.Credentials = append(u.Credentials, c)
	}
	return u, version, rows.Err()
}
func (s *Server) passkeyBegin(w http.ResponseWriter, r *http.Request) {
	register := strings.HasPrefix(r.URL.Path, "/api/")
	if !s.sameOrigin(r) {
		http.Error(w, "origin rejected", 403)
		return
	}
	if !s.authLimit(w, r) {
		return
	}
	web, err := s.webauthn()
	if err != nil {
		http.Error(w, "passkeys unavailable", 503)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "passkeys unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var options any
	var data *wa.SessionData
	var user *string
	var version *int64
	var session []byte
	ceremony := "login"
	if register {
		var in struct {
			Password string `json:"current_password"`
		}
		if !decode(w, r, &in) {
			return
		}
		var hash []byte
		if s.DB.QueryRow(r.Context(), "SELECT password_hash FROM users WHERE id=$1", actor(r).ID).Scan(&hash) != nil {
			http.Error(w, "authentication unavailable", 503)
			return
		}
		if len(hash) > 0 && (len(in.Password) > 72 || bcrypt.CompareHashAndPassword(hash, []byte(in.Password)) != nil) {
			http.Error(w, "current password is incorrect", 400)
			return
		}
		cookie, e := r.Cookie("__Host-cove")
		if e != nil {
			http.Error(w, "sign in to register a passkey", 401)
			return
		}
		u, v, e := loadPasskeyUser(r.Context(), tx, actor(r).ID)
		if e != nil {
			http.Error(w, "passkeys unavailable", 503)
			return
		}
		if len(u.Credentials) >= 20 {
			http.Error(w, "passkey limit reached", 409)
			return
		}
		var enrolled, verified bool
		e = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM account_totp WHERE user_id=$1 AND confirmed),second_factor_verified FROM sessions WHERE token_hash=$2 AND expires_at>now()", u.ID, tokenHash(cookie.Value)).Scan(&enrolled, &verified)
		if e != nil || ((enrolled || len(u.Credentials) > 0) && !verified) {
			http.Error(w, "second factor required", 403)
			return
		}
		options, data, err = web.BeginRegistration(u, wa.WithExclusions(wa.Credentials(u.Credentials).CredentialDescriptors()))
		user = &u.ID
		version = &v
		session = tokenHash(cookie.Value)
		ceremony = "register"
	} else {
		options, data, err = web.BeginDiscoverableLogin(wa.WithUserVerification(protocol.VerificationRequired))
	}
	if err != nil {
		http.Error(w, "passkeys unavailable", 503)
		return
	}
	browser := randomToken()
	_, err = tx.Exec(r.Context(), "INSERT INTO webauthn_challenges(browser_hash,user_id,user_version,session_hash,ceremony,data) VALUES($1,$2,$3,$4,$5,$6)", tokenHash(browser), user, version, session, ceremony, data)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "passkeys unavailable", 503)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove-webauthn", Value: browser, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 300})
	respond(w, 200, options)
}
func (s *Server) passkeyFinish(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "origin rejected", 403)
		return
	}
	if !s.authLimit(w, r) {
		return
	}
	cookie, err := r.Cookie("__Host-cove-webauthn")
	if err != nil {
		http.Error(w, "invalid or expired challenge", 401)
		return
	}
	var user *string
	var version *int64
	var session []byte
	var data wa.SessionData
	ceremony := "login"
	if strings.HasPrefix(r.URL.Path, "/api/") {
		ceremony = "register"
	}
	err = s.DB.QueryRow(r.Context(), "DELETE FROM webauthn_challenges WHERE browser_hash=$1 AND ceremony=$2 AND expires_at>now() RETURNING user_id,user_version,session_hash,data", tokenHash(cookie.Value), ceremony).Scan(&user, &version, &session, &data)
	if err != nil {
		http.Error(w, "invalid or expired challenge", 401)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove-webauthn", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	web, err := s.webauthn()
	if err != nil {
		http.Error(w, "passkeys unavailable", 503)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "passkeys unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	var credential *wa.Credential
	var u *passkeyUser
	if ceremony == "register" {
		current, e := r.Cookie("__Host-cove")
		if e != nil || user == nil || version == nil || *user != actor(r).ID || !bytes.Equal(session, tokenHash(current.Value)) {
			http.Error(w, "invalid registration session", 401)
			return
		}
		var v int64
		u, v, err = loadPasskeyUser(r.Context(), tx, *user)
		if err != nil || v != *version {
			http.Error(w, "invalid registration session", 401)
			return
		}
		credential, err = web.FinishRegistration(u, data, r)
		if err == nil {
			_, err = tx.Exec(r.Context(), "INSERT INTO passkeys(id,user_id,name,credential) VALUES($1,$2,'Passkey',$3)", credential.ID, u.ID, credential)
		}
	} else {
		var authenticated wa.User
		authenticated, credential, err = web.FinishPasskeyLogin(func(rawID, handle []byte) (wa.User, error) {
			var e error
			u, _, e = loadPasskeyUser(r.Context(), tx, string(handle))
			return u, e
		}, data, r)
		if err == nil {
			u = authenticated.(*passkeyUser)
			if credential.Authenticator.CloneWarning {
				err = errors.New("authenticator counter replay")
			}
		}
		if err == nil {
			_, err = tx.Exec(r.Context(), "UPDATE passkeys SET credential=$2 WHERE id=$1 AND user_id=$3", credential.ID, credential, u.ID)
		}
	}
	if err != nil {
		http.Error(w, "passkey verification failed", 401)
		return
	}
	token := randomToken()
	expiry := time.Now().Add(12 * time.Hour)
	if ceremony == "login" {
		_, err = tx.Exec(r.Context(), "INSERT INTO sessions(token_hash,user_id,expires_at,user_agent,second_factor_verified) VALUES($1,$2,$3,$4,true)", tokenHash(token), u.ID, expiry, sessionUserAgent(r))
	} else {
		_, err = tx.Exec(r.Context(), "DELETE FROM sessions WHERE user_id=$1 AND token_hash<>$2", u.ID, session)
		if err == nil {
			_, err = tx.Exec(r.Context(), "UPDATE sessions SET second_factor_verified=true WHERE token_hash=$1", session)
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "passkeys unavailable", 503)
		return
	}
	if ceremony == "login" {
		setSession(w, token, expiry)
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) accountPasskeys(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), "SELECT id,name,created_at FROM passkeys WHERE user_id=$1 ORDER BY created_at", actor(r).ID)
		if err != nil {
			http.Error(w, "passkeys unavailable", 503)
			return
		}
		defer rows.Close()
		type entry struct {
			ID      string    `json:"id"`
			Name    string    `json:"name"`
			Created time.Time `json:"created_at"`
		}
		out := []entry{}
		for rows.Next() {
			var id []byte
			var e entry
			if rows.Scan(&id, &e.Name, &e.Created) != nil {
				http.Error(w, "passkeys unavailable", 503)
				return
			}
			e.ID = base64.RawURLEncoding.EncodeToString(id)
			out = append(out, e)
		}
		if rows.Err() != nil {
			http.Error(w, "passkeys unavailable", 503)
			return
		}
		respond(w, 200, out)
		return
	}
	id, err := base64.RawURLEncoding.DecodeString(r.PathValue("key"))
	if err != nil {
		http.Error(w, "invalid passkey", 400)
		return
	}
	query := "DELETE FROM passkeys WHERE id=$1 AND user_id=$2"
	args := []any{id, actor(r).ID}
	if r.Method == "PATCH" {
		var in struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &in) {
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" || len(in.Name) > 120 {
			http.Error(w, "name must contain 1-120 characters", 400)
			return
		}
		query = "UPDATE passkeys SET name=$3 WHERE id=$1 AND user_id=$2"
		args = append(args, in.Name)
	}
	result, err := s.DB.Exec(r.Context(), query, args...)
	if err != nil {
		http.Error(w, "passkeys unavailable", 503)
		return
	}
	if result.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
