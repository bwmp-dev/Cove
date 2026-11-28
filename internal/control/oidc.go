package control

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"
)

type oidcProviderConfig struct {
	Name     string   `json:"display_name"`
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"client_id"`
	Secret   string   `json:"client_secret,omitempty"`
	Scopes   []string `json:"scopes"`
}

func (s *Server) loadOIDC(ctx context.Context, id string) (oidcProviderConfig, int64, error) {
	var encrypted []byte
	var version int64
	var c oidcProviderConfig
	err := s.DB.QueryRow(ctx, "SELECT configuration,version FROM oidc_providers WHERE id=$1", id).Scan(&encrypted, &version)
	if err != nil {
		return c, 0, err
	}
	plain, err := unseal(s.Key, encrypted)
	if err == nil {
		err = json.Unmarshal(plain, &c)
	}
	return c, version, err
}
func (s *Server) oidcProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), "SELECT id,configuration FROM oidc_providers ORDER BY id")
		if err != nil {
			http.Error(w, "providers unavailable", 503)
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id string
			var encrypted []byte
			if rows.Scan(&id, &encrypted) != nil {
				http.Error(w, "providers unavailable", 503)
				return
			}
			plain, e := unseal(s.Key, encrypted)
			var c oidcProviderConfig
			if e != nil || json.Unmarshal(plain, &c) != nil {
				http.Error(w, "providers unavailable", 503)
				return
			}
			c.Secret = ""
			out = append(out, map[string]any{"id": id, "configuration": c, "redirect_uri": s.oidcRedirectURI(id)})
		}
		if rows.Err() != nil {
			http.Error(w, "providers unavailable", 503)
			return
		}
		respond(w, 200, out)
		return
	}
	if r.Method == "DELETE" {
		tx, err := s.identityTransaction(r.Context())
		if err != nil {
			http.Error(w, "providers unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		state, err := loadSCIMState(r.Context(), tx, s.Key)
		var managed bool
		if err == nil {
			err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM scim_users WHERE oidc_provider_id=$1)", r.PathValue("provider")).Scan(&managed)
		}
		if err != nil {
			http.Error(w, "provisioning policy unavailable", 503)
			return
		}
		if managed || state.Config.OIDCProvider == r.PathValue("provider") {
			http.Error(w, "detach SCIM accounts and change its OIDC binding before deleting this provider", 409)
			return
		}
		result, err := tx.Exec(r.Context(), "DELETE FROM oidc_providers WHERE id=$1", r.PathValue("provider"))
		if err != nil {
			http.Error(w, "providers unavailable", 503)
			return
		}
		if result.RowsAffected() == 0 {
			http.NotFound(w, r)
			return
		}
		if tx.Commit(r.Context()) != nil {
			http.Error(w, "providers unavailable", 503)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	var c oidcProviderConfig
	if !decode(w, r, &c) {
		return
	}
	issuer, err := url.Parse(c.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" || len(c.Issuer) > 2048 || strings.TrimSpace(c.Name) == "" || len(c.Name) > 120 || c.ClientID == "" || len(c.ClientID) > 512 || len(c.Secret) > 4096 || len(c.Scopes) > 20 {
		http.Error(w, "valid HTTPS issuer, display name and client id required", 400)
		return
	}
	c.Scopes = append([]string{"openid", "email", "profile"}, c.Scopes...)
	for _, scope := range c.Scopes {
		if strings.ContainsAny(scope, " \t\r\n") || scope == "" || len(scope) > 200 {
			http.Error(w, "invalid scopes", 400)
			return
		}
	}
	id := r.PathValue("provider")
	if id == "" {
		id = resourceID()
	}
	if c.Secret == "" && r.Method == "PUT" {
		saved, _, e := s.loadOIDC(r.Context(), id)
		if e != nil {
			http.Error(w, "provider unavailable", 404)
			return
		}
		c.Secret = saved.Secret
	}
	raw, err := json.Marshal(c)
	if err == nil {
		raw, err = seal(s.Key, raw)
	}
	if err != nil {
		http.Error(w, "providers unavailable", 503)
		return
	}
	if r.Method == "PUT" {
		_, err = s.DB.Exec(r.Context(), "UPDATE oidc_providers SET configuration=$2,version=version+1 WHERE id=$1", id, raw)
	} else {
		_, err = s.DB.Exec(r.Context(), "INSERT INTO oidc_providers(id,configuration) VALUES($1,$2)", id, raw)
	}
	if err != nil {
		http.Error(w, "providers unavailable", 503)
		return
	}
	respond(w, 200, map[string]string{"id": id, "redirect_uri": s.oidcRedirectURI(id)})
}
func (s *Server) oidcClient(ctx context.Context, c oidcProviderConfig, id string) (context.Context, *oidc.Provider, *oauth2.Config, error) {
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if s.OAuthClient != nil {
		client.Transport = s.OAuthClient.Transport
	}
	ctx = oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(ctx, c.Issuer)
	if err != nil {
		return ctx, nil, nil, err
	}
	endpoint := provider.Endpoint()
	var discovery struct {
		Keys string `json:"jwks_uri"`
	}
	if err = provider.Claims(&discovery); err != nil {
		return ctx, nil, nil, err
	}
	for _, raw := range []string{endpoint.AuthURL, endpoint.TokenURL, discovery.Keys} {
		u, e := url.Parse(raw)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return ctx, nil, nil, errors.New("invalid provider endpoint")
		}
	}
	return ctx, provider, &oauth2.Config{ClientID: c.ClientID, ClientSecret: c.Secret, Endpoint: endpoint, RedirectURL: s.oidcRedirectURI(id), Scopes: c.Scopes}, nil
}
func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Link     bool   `json:"link"`
		Password string `json:"current_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	id := r.PathValue("provider")
	c, version, err := s.loadOIDC(r.Context(), id)
	if err != nil {
		http.Error(w, "provider unavailable", 404)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	_, _, client, err := s.oidcClient(ctx, c, id)
	if err != nil {
		http.Error(w, "provider unavailable", 502)
		return
	}
	var link *string
	var linkVersion *int64
	var session []byte
	if in.Link {
		p, e := s.sessionIdentity(r)
		if e != nil {
			http.Error(w, "sign in before linking OIDC", 401)
			return
		}
		var hash []byte
		if s.DB.QueryRow(r.Context(), "SELECT password_hash FROM users WHERE id=$1", p.ID).Scan(&hash) != nil {
			http.Error(w, "authentication unavailable", 503)
			return
		}
		if len(hash) > 0 {
			if len(in.Password) > 72 || bcrypt.CompareHashAndPassword(hash, []byte(in.Password)) != nil {
				http.Error(w, "current password is incorrect", 400)
				return
			}
		} else {
			cookie, _ := r.Cookie("__Host-cove")
			var fresh bool
			if s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM sessions WHERE token_hash=$1 AND expires_at>now()+interval '11 hours 50 minutes')", tokenHash(cookie.Value)).Scan(&fresh) != nil || !fresh {
				http.Error(w, "sign in again before linking OIDC", 401)
				return
			}
		}
		var v int64
		if s.DB.QueryRow(r.Context(), "SELECT auth_version FROM users WHERE id=$1", p.ID).Scan(&v) != nil {
			http.Error(w, "authentication unavailable", 503)
			return
		}
		cookie, _ := r.Cookie("__Host-cove")
		link = &p.ID
		linkVersion = &v
		session = tokenHash(cookie.Value)
	}
	state, browser, nonce, verifier := randomToken(), randomToken(), randomToken(), oauth2.GenerateVerifier()
	encrypted, err := seal(s.Key, []byte(verifier))
	if err == nil {
		_, err = s.DB.Exec(r.Context(), "INSERT INTO oidc_states(token_hash,browser_hash,provider_id,provider_version,nonce_hash,verifier,link_user_id,link_version,session_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)", tokenHash(state), tokenHash(browser), id, version, tokenHash(nonce), encrypted, link, linkVersion, session)
	}
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove-oidc", Value: browser, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	respond(w, 200, map[string]string{"url": client.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))})
}
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	ensureLoginDevice(w, r)
	status := 401
	userForEvent := ""
	defer func() {
		s.recordAudit(r, "OIDC callback", status)
		address, _, _ := net.SplitHostPort(r.RemoteAddr)
		s.recordLogin(r, userForEvent, status == 200, "oidc", address)
	}()
	fail := func(code string) { http.Redirect(w, r, "/login?error="+code, http.StatusSeeOther) }
	cookie, err := r.Cookie("__Host-cove-oidc")
	if err != nil {
		fail("oidc_failed")
		return
	}
	var browser, nonce, encrypted, session []byte
	var link *string
	var version int64
	var linkVersion *int64
	id := r.PathValue("provider")
	err = s.DB.QueryRow(r.Context(), "DELETE FROM oidc_states WHERE token_hash=$1 AND provider_id=$2 AND expires_at>now() RETURNING browser_hash,nonce_hash,verifier,provider_version,link_user_id,link_version,session_hash", tokenHash(r.URL.Query().Get("state")), id).Scan(&browser, &nonce, &encrypted, &version, &link, &linkVersion, &session)
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove-oidc", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	if err != nil || subtle.ConstantTimeCompare(browser, tokenHash(cookie.Value)) != 1 {
		fail("oidc_failed")
		return
	}
	c, current, err := s.loadOIDC(r.Context(), id)
	if err != nil || current != version {
		fail("oidc_failed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	ctx, provider, client, err := s.oidcClient(ctx, c, id)
	if err != nil {
		fail("oidc_failed")
		return
	}
	verifier, err := unseal(s.Key, encrypted)
	if err != nil {
		fail("oidc_failed")
		return
	}
	tokens, err := client.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(string(verifier)))
	if err != nil {
		fail("oidc_failed")
		return
	}
	raw, ok := tokens.Extra("id_token").(string)
	if !ok {
		fail("oidc_failed")
		return
	}
	token, err := provider.Verifier(&oidc.Config{ClientID: c.ClientID}).Verify(ctx, raw)
	if err != nil || token.Subject == "" || subtle.ConstantTimeCompare(nonce, tokenHash(token.Nonce)) != 1 {
		fail("oidc_failed")
		return
	}
	var claims struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
		Name     string `json:"name"`
	}
	if token.Claims(&claims) != nil || !claims.Verified {
		fail("verified_email_required")
		return
	}
	email, ok := normalizeEmail(claims.Email)
	if !ok {
		fail("verified_email_required")
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		fail("oidc_failed")
		return
	}
	defer tx.Rollback(r.Context())
	var user string
	err = tx.QueryRow(r.Context(), "SELECT u.id FROM oidc_identities i JOIN users u ON u.id=i.user_id WHERE i.provider_id=$1 AND i.subject=$2 AND NOT u.disabled AND u.email_verified FOR UPDATE OF u", id, token.Subject).Scan(&user)
	if link != nil {
		if err == nil && user != *link {
			fail("account_exists")
			return
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			fail("oidc_failed")
			return
		}
		var localEmail string
		var v int64
		var valid bool
		err = tx.QueryRow(r.Context(), "SELECT email,auth_version,EXISTS(SELECT 1 FROM sessions WHERE token_hash=$2 AND user_id=$1 AND expires_at>now()) FROM users WHERE id=$1 AND NOT disabled FOR UPDATE", *link, session).Scan(&localEmail, &v, &valid)
		if err != nil || linkVersion == nil || v != *linkVersion || !valid || email != localEmail {
			fail("oidc_failed")
			return
		}
		user = *link
		_, err = tx.Exec(r.Context(), "INSERT INTO oidc_identities(provider_id,subject,user_id) VALUES($1,$2,$3) ON CONFLICT(provider_id,subject) DO NOTHING", id, token.Subject, user)
	} else if errors.Is(err, pgx.ErrNoRows) {
		var enabled, exists bool
		err = tx.QueryRow(r.Context(), "SELECT oidc_registration_enabled,EXISTS(SELECT 1 FROM users WHERE email=$1) FROM control_settings WHERE id=1 FOR SHARE", email).Scan(&enabled, &exists)
		if err != nil {
			fail("oidc_failed")
			return
		}
		if exists {
			fail("account_exists")
			return
		}
		if !enabled {
			fail("registration_closed")
			return
		}
		user = resourceID()
		name := strings.TrimSpace(claims.Name)
		if name == "" || len(name) > 120 {
			name = "OIDC user"
		}
		_, err = tx.Exec(r.Context(), "INSERT INTO users(id,email,name,email_verified) VALUES($1,$2,$3,true)", user, email, name)
		if err == nil {
			_, err = tx.Exec(r.Context(), "INSERT INTO oidc_identities(provider_id,subject,user_id) VALUES($1,$2,$3)", id, token.Subject, user)
		}
	}
	if err != nil {
		fail("oidc_failed")
		return
	}
	sessionToken := randomToken()
	expiry := time.Now().Add(12 * time.Hour)
	_, err = tx.Exec(r.Context(), "INSERT INTO sessions(token_hash,user_id,expires_at,user_agent) VALUES($1,$2,$3,$4)", tokenHash(sessionToken), user, expiry, sessionUserAgent(r))
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail("oidc_failed")
		return
	}
	status = 200
	userForEvent = user
	s.completeSignIn(w, r, user, sessionToken, expiry)
	http.Redirect(w, r, "/login?authenticated=1", http.StatusSeeOther)
}
func (s *Server) signInPolicy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Disabled     bool   `json:"password_login_disabled"`
		Registration bool   `json:"oidc_registration_enabled"`
		Redirect     string `json:"oidc_auto_redirect"`
	}
	if r.Method == "GET" {
		if s.DB.QueryRow(r.Context(), "SELECT password_login_disabled,oidc_registration_enabled,oidc_auto_redirect FROM control_settings WHERE id=1").Scan(&in.Disabled, &in.Registration, &in.Redirect) != nil {
			http.Error(w, "settings unavailable", 503)
			return
		}
	} else {
		if !decode(w, r, &in) {
			return
		}
		var valid bool
		if s.DB.QueryRow(r.Context(), "SELECT $1='' OR EXISTS(SELECT 1 FROM oidc_providers WHERE id=$1)", in.Redirect).Scan(&valid) != nil {
			http.Error(w, "settings unavailable", 503)
			return
		}
		if !valid {
			http.Error(w, "unknown redirect provider", 400)
			return
		}
		if _, err := s.DB.Exec(r.Context(), "UPDATE control_settings SET password_login_disabled=$1,oidc_registration_enabled=$2,oidc_auto_redirect=$3 WHERE id=1", in.Disabled, in.Registration, in.Redirect); err != nil {
			http.Error(w, "settings unavailable", 503)
			return
		}
	}
	respond(w, 200, in)
}

func (s *Server) oidcRedirectURI(id string) string {
	return s.PublicURL + "/auth/oidc/" + id + "/callback"
}
