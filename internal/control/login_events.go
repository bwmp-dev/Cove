package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

func (s *Server) loginAttempt(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sameOrigin(r) {
			http.Error(w, "origin rejected", 403)
			return
		}
		address, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "invalid client address", 400)
			return
		}
		ensureLoginDevice(w, r)
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
		if err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var fields struct {
			Email string `json:"email"`
			Token string `json:"token"`
		}
		_ = json.Unmarshal(body, &fields)
		email := strings.ToLower(strings.TrimSpace(fields.Email))
		user := ""
		if fields.Token != "" {
			err = s.DB.QueryRow(r.Context(), "SELECT user_id FROM account_actions WHERE token_hash=$1 AND purpose='reset'", tokenHash(fields.Token)).Scan(&user)
		} else if strings.HasPrefix(r.URL.Path, "/auth/ldap/") {
			err = s.DB.QueryRow(r.Context(), "SELECT id FROM users WHERE email=$1 UNION SELECT user_id FROM ldap_identities WHERE provider_id=$2 AND username=$1 ORDER BY id LIMIT 1", email, r.PathValue("provider")).Scan(&user)
		} else if email != "" || r.URL.Path == "/auth/login" {
			err = s.DB.QueryRow(r.Context(), "SELECT id FROM users WHERE email=$1 OR ($1='' AND id='bootstrap')", email).Scan(&user)
		} else if cookie, e := r.Cookie("__Host-cove"); e == nil {
			err = s.DB.QueryRow(r.Context(), "SELECT user_id FROM sessions WHERE token_hash=$1 AND expires_at>now()", tokenHash(cookie.Value)).Scan(&user)
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "authentication unavailable", 503)
			return
		}
		key := emailRateKey("failure", email)
		if user != "" {
			key = "account:" + user
		}
		keys := []string{"address:" + address}
		if user != "" || email != "" {
			keys = append(keys, key)
		}
		for _, k := range keys {
			var locked bool
			if s.DB.QueryRow(r.Context(), "SELECT COALESCE((SELECT locked_until>now() FROM authentication_failures WHERE key=$1),false)", k).Scan(&locked) != nil {
				http.Error(w, "authentication unavailable", 503)
				return
			}
			if locked {
				w.Header().Set("Retry-After", "900")
				http.Error(w, "too many attempts; try again later", 429)
				return
			}
		}
		response := &statusResponse{ResponseWriter: w}
		next(response, r)
		if response.status == 400 || response.status == 401 {
			for i, k := range keys {
				threshold := 10
				if i == 0 {
					threshold = 50
				}
				if _, e := s.DB.Exec(r.Context(), `INSERT INTO authentication_failures(key,failures) VALUES($1,1) ON CONFLICT(key) DO UPDATE SET failures=CASE WHEN authentication_failures.window_start<now()-interval '15 minutes' THEN 1 ELSE authentication_failures.failures+1 END,window_start=CASE WHEN authentication_failures.window_start<now()-interval '15 minutes' THEN now() ELSE authentication_failures.window_start END,locked_until=CASE WHEN authentication_failures.window_start>=now()-interval '15 minutes' AND authentication_failures.failures+1 >= $2 THEN now()+interval '15 minutes' ELSE authentication_failures.locked_until END`, k, threshold); e != nil {
					slog.Error("authentication failure could not be persisted")
				}
			}
		}
		success := response.status >= 200 && response.status < 300
		for _, cookie := range response.Header().Values("Set-Cookie") {
			if strings.HasPrefix(cookie, "__Host-cove=") {
				v := strings.SplitN(strings.TrimPrefix(cookie, "__Host-cove="), ";", 2)[0]
				err = s.DB.QueryRow(r.Context(), "SELECT user_id FROM sessions WHERE token_hash=$1", tokenHash(v)).Scan(&user)
			}
		}
		method := strings.TrimPrefix(r.URL.Path, "/auth/")
		if strings.HasPrefix(method, "ldap/") {
			method = "ldap"
		}
		s.recordLogin(r, user, success, method, address)
	}
}
func (s *Server) recordLogin(r *http.Request, user string, success bool, method, address string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 8*time.Second)
	defer cancel()
	var account *string
	if user != "" {
		account = &user
	}
	if _, err := s.DB.Exec(ctx, "INSERT INTO login_events(user_id,success,method,address,user_agent) VALUES($1,$2,$3,$4,$5)", account, success, method, address, sessionUserAgent(r)); err != nil {
		slog.Error("login event could not be persisted")
	}
	if !success || user == "" || strings.Contains(method, "password") || method == "register" {
		return
	}
	var needsFactor bool
	if s.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM account_totp WHERE user_id=$1 AND confirmed) OR EXISTS(SELECT 1 FROM passkeys WHERE user_id=$1)", user).Scan(&needsFactor) != nil {
		return
	}
	if needsFactor && (method == "login" || method == "oidc" || method == "github" || method == "ldap") {
		return
	}
	result, err := s.DB.Exec(ctx, "INSERT INTO account_devices(user_id,device_hash) VALUES($1,$2) ON CONFLICT(user_id,device_hash) DO UPDATE SET last_seen=now() WHERE account_devices.last_seen<now()-interval '90 days'", user, tokenHash(loginDevice(r)))
	if err != nil {
		slog.Error("sign-in device could not be persisted")
		return
	}
	if result.RowsAffected() == 0 {
		return
	}
	var email string
	if s.DB.QueryRow(ctx, "SELECT COALESCE(email,'') FROM users WHERE id=$1 AND email_verified", user).Scan(&email) != nil || email == "" {
		return
	}
	if _, err = s.DB.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status) VALUES($1,'account new device','',200)`, user); err != nil {
		slog.Error("sign-in notification event could not be persisted")
	}
	mailer, err := s.mailer(ctx)
	if err != nil {
		slog.Error("sign-in email settings unavailable")
		return
	}
	if mailer == nil {
		return
	}
	if err = s.sendNotification(ctx, &notificationConfig{Provider: "email", Email: email}, map[string]string{"event": "security.new_device", "subject": "New sign-in to Cove", "message": "Your account signed in from a new browser.\nAddress: " + address + "\nBrowser: " + sessionUserAgent(r) + "\nIf this was not you, reset your password and revoke your sessions."}); err != nil {
		slog.Error("sign-in email delivery failed")
	}

}
func (s *Server) loginHistory(w http.ResponseWriter, r *http.Request) {
	user := actor(r).ID
	if r.PathValue("id") != "" {
		user = r.PathValue("id")
	}
	rows, err := s.DB.Query(r.Context(), "SELECT id,success,method,address,user_agent,created_at FROM login_events WHERE user_id=$1 ORDER BY id DESC LIMIT 100", user)
	if err != nil {
		http.Error(w, "history unavailable", 503)
		return
	}
	defer rows.Close()
	type event struct {
		ID      int64     `json:"id"`
		Success bool      `json:"success"`
		Method  string    `json:"method"`
		Address string    `json:"address"`
		Agent   string    `json:"user_agent"`
		Created time.Time `json:"created_at"`
	}
	out := []event{}
	for rows.Next() {
		var e event
		if rows.Scan(&e.ID, &e.Success, &e.Method, &e.Address, &e.Agent, &e.Created) != nil {
			http.Error(w, "history unavailable", 503)
			return
		}
		out = append(out, e)
	}
	if rows.Err() != nil {
		http.Error(w, "history unavailable", 503)
		return
	}
	respond(w, 200, out)
}
func (s *Server) publicSignInMethods(w http.ResponseWriter, r *http.Request) {
	var disabled, registration bool
	var redirect string
	if s.DB.QueryRow(r.Context(), "SELECT password_login_disabled,oidc_registration_enabled,oidc_auto_redirect FROM control_settings WHERE id=1").Scan(&disabled, &registration, &redirect) != nil {
		http.Error(w, "settings unavailable", 503)
		return
	}
	rows, err := s.DB.Query(r.Context(), "SELECT id,configuration FROM oidc_providers ORDER BY id")
	if err != nil {
		http.Error(w, "settings unavailable", 503)
		return
	}
	defer rows.Close()
	providers := []map[string]string{}
	for rows.Next() {
		var id string
		var encrypted []byte
		if rows.Scan(&id, &encrypted) != nil {
			http.Error(w, "settings unavailable", 503)
			return
		}
		plain, e := unseal(s.Key, encrypted)
		var c oidcProviderConfig
		if e != nil || json.Unmarshal(plain, &c) != nil {
			http.Error(w, "settings unavailable", 503)
			return
		}
		providers = append(providers, map[string]string{"id": id, "display_name": c.Name})
	}
	if rows.Err() != nil {
		http.Error(w, "settings unavailable", 503)
		return
	}
	pending := false
	factors := []string{}
	if cookie, e := r.Cookie("__Host-cove"); e == nil {
		var totp, recovery, passkey bool
		err := s.DB.QueryRow(r.Context(), `SELECT
 EXISTS(SELECT 1 FROM account_totp WHERE user_id=s.user_id AND confirmed),
 EXISTS(SELECT 1 FROM recovery_codes WHERE user_id=s.user_id),
 EXISTS(SELECT 1 FROM passkeys WHERE user_id=s.user_id)
 FROM sessions s JOIN users u ON u.id=s.user_id WHERE token_hash=$1 AND s.expires_at>now()+interval '11 hours 50 minutes' AND NOT second_factor_verified AND NOT u.disabled AND u.email_verified`, tokenHash(cookie.Value)).Scan(&totp, &recovery, &passkey)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "authentication unavailable", 503)
			return
		}
		if err == nil {
			pending = totp || passkey
			if pending {
				if totp {
					factors = append(factors, "totp")
				}
				if recovery {
					factors = append(factors, "recovery_code")
				}
				if passkey {
					factors = append(factors, "passkey")
				}
			}
		}
	}
	ldapProviders, err := s.enabledLDAPProviders(r.Context())
	if err != nil {
		http.Error(w, "directory providers unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"password_enabled": !disabled, "passkeys_enabled": true, "oidc_registration_enabled": registration, "oidc_auto_redirect": redirect, "oidc_providers": providers, "ldap_enabled": len(ldapProviders) > 0, "ldap_providers": ldapProviders, "second_factor_pending": pending, "second_factor_methods": factors})
}

func loginDevice(r *http.Request) string {
	if c, err := r.Cookie("__Host-cove-device"); err == nil && len(c.Value) == 43 {
		return c.Value
	}
	return randomToken()
}
func ensureLoginDevice(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("__Host-cove-device"); err == nil && len(c.Value) == 43 {
		return
	}
	cookie := &http.Cookie{Name: "__Host-cove-device", Value: randomToken(), Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 90 * 24 * 60 * 60}
	http.SetCookie(w, cookie)
	r.AddCookie(cookie)
}
