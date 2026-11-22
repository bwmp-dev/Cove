package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const cloudflareOAuthScopes = "zone.read dns.write offline_access"

type cloudflareOAuthConfig struct {
	ClientID string `json:"client_id"`
	Secret   string `json:"client_secret"`
}

func (s *Server) cloudflareOAuthClient(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (cloudflareOAuthConfig, error) {
	var raw []byte
	var c cloudflareOAuthConfig
	err := db.QueryRow(ctx, "SELECT credentials FROM cloudflare_oauth_client WHERE id=1").Scan(&raw)
	if err == nil {
		raw, err = unseal(s.Key, raw)
	}
	if err == nil {
		err = json.Unmarshal(raw, &c)
	}
	if err != nil || c.ClientID == "" || c.Secret == "" {
		return c, errors.New("Cloudflare OAuth is not configured")
	}
	return c, nil
}

func (s *Server) cloudflareOAuthSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		c, err := s.cloudflareOAuthClient(r.Context(), s.DB)
		out := map[string]any{"configured": err == nil}
		if actor(r).Admin {
			out["client_id"] = c.ClientID
			out["redirect_uri"] = s.PublicURL + "/api/cloudflare/oauth/callback"
			out["scopes"] = cloudflareOAuthScopes
		}
		respond(w, 200, out)
		return
	}
	if !actor(r).Admin {
		http.Error(w, "instance administrator access required", 403)
		return
	}
	var c cloudflareOAuthConfig
	if !decode(w, r, &c) {
		return
	}
	if len(c.ClientID) < 3 || len(c.ClientID) > 256 || strings.ContainsAny(c.ClientID, "\r\n\t ") || len(c.Secret) < 20 || len(c.Secret) > 4096 || strings.ContainsAny(c.Secret, "\r\n") {
		http.Error(w, "a valid Cloudflare OAuth client ID and secret are required", 400)
		return
	}
	raw, err := json.Marshal(c)
	if err == nil {
		raw, err = seal(s.Key, raw)
	}
	if err != nil {
		http.Error(w, "could not protect credentials", 503)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var previous []byte
	// Changing clients would invalidate every existing refresh token.
	if e := tx.QueryRow(r.Context(), "SELECT credentials FROM cloudflare_oauth_client WHERE id=1 FOR UPDATE").Scan(&previous); e == nil {
		plain, e := unseal(s.Key, previous)
		var old cloudflareOAuthConfig
		if e != nil || json.Unmarshal(plain, &old) != nil {
			http.Error(w, "OAuth configuration unavailable", 503)
			return
		}
		if old.ClientID != c.ClientID {
			var used bool
			if e = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM cloudflare_accounts WHERE kind='oauth')").Scan(&used); e != nil {
				http.Error(w, "database unavailable", 503)
				return
			}
			if used {
				http.Error(w, "existing OAuth connections require the same client ID", 409)
				return
			}
		}
	}
	_, err = tx.Exec(r.Context(), "INSERT INTO cloudflare_oauth_client(id,credentials) VALUES(1,$1) ON CONFLICT(id) DO UPDATE SET credentials=EXCLUDED.credentials", raw)
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM cloudflare_oauth_states")
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save OAuth configuration", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) exchangeCloudflareToken(ctx context.Context, c cloudflareOAuthConfig, form url.Values) (cloudflareCredentials, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", "https://dash.cloudflare.com/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return cloudflareCredentials{}, err
	}
	req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(c.Secret))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second, Transport: s.CloudflareTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return cloudflareCredentials{}, errors.New("Cloudflare authorization unavailable")
	}
	defer response.Body.Close()
	var access struct {
		Token   string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Type    string `json:"token_type"`
		Expires int64  `json:"expires_in"`
		Scope   string `json:"scope"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 128<<10)).Decode(&access)
	if err != nil || response.StatusCode != 200 || !validCloudflareToken(access.Token) || !strings.EqualFold(access.Type, "bearer") || access.Expires < 60 || access.Expires > 86400*365 {
		return cloudflareCredentials{}, errors.New("Cloudflare returned invalid authorization")
	}
	if access.Scope != "" {
		scopes := map[string]bool{}
		for _, scope := range strings.Fields(access.Scope) {
			scopes[scope] = true
		}
		if !scopes["zone.read"] || !scopes["dns.write"] {
			return cloudflareCredentials{}, errors.New("Cloudflare DNS authorization was not granted")
		}
	}
	if access.Refresh != "" && !validCloudflareToken(access.Refresh) {
		return cloudflareCredentials{}, errors.New("Cloudflare returned invalid refresh authorization")
	}
	return cloudflareCredentials{Token: access.Token, Refresh: access.Refresh, Expires: time.Now().Add(time.Duration(access.Expires) * time.Second), ClientID: c.ClientID}, nil
}

func (s *Server) cloudflareOAuthStart(w http.ResponseWriter, r *http.Request) {
	cookie, e := r.Cookie("__Host-cove")
	person, err := s.sessionIdentity(r)
	if e != nil || err != nil || person.ID != actor(r).ID {
		http.Error(w, "a browser session is required", 401)
		return
	}
	c, err := s.cloudflareOAuthClient(r.Context(), s.DB)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	state, browser, verifier := randomToken(), randomToken(), randomToken()
	encrypted, err := seal(s.Key, []byte(verifier))
	if err != nil {
		http.Error(w, "could not start authorization", 503)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var version int64
	err = tx.QueryRow(r.Context(), "SELECT auth_version FROM users WHERE id=$1 FOR UPDATE", person.ID).Scan(&version)
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM cloudflare_oauth_states WHERE user_id=$1 OR expires_at<=now()", person.ID)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO cloudflare_oauth_states(state_hash,browser_hash,verifier,user_id,user_version,session_hash,client_id,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,now()+interval '10 minutes')", tokenHash(state), tokenHash(browser), encrypted, person.ID, version, tokenHash(cookie.Value), c.ClientID)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not start authorization", 503)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove-cloudflare-connect", Value: browser, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	hash := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {c.ClientID}, "redirect_uri": {s.PublicURL + "/api/cloudflare/oauth/callback"}, "response_type": {"code"}, "scope": {cloudflareOAuthScopes}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}}
	respond(w, 200, map[string]string{"url": "https://dash.cloudflare.com/oauth2/auth?" + q.Encode()})
}

func (s *Server) cloudflareOAuthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	finish := func(result string) { http.Redirect(w, r, "/integrations?cloudflare="+result, http.StatusSeeOther) }
	cookie, err := r.Cookie("__Host-cove-cloudflare-connect")
	if err != nil || len(r.URL.Query().Get("state")) > 200 || len(r.URL.Query().Get("code")) > 4096 {
		finish("expired")
		return
	}
	var user, clientID string
	var version int64
	var verifier, session []byte
	err = s.DB.QueryRow(r.Context(), "DELETE FROM cloudflare_oauth_states WHERE state_hash=$1 AND browser_hash=$2 AND expires_at>now() RETURNING user_id,user_version,verifier,session_hash,client_id", tokenHash(r.URL.Query().Get("state")), tokenHash(cookie.Value)).Scan(&user, &version, &verifier, &session, &clientID)
	if err != nil {
		finish("expired")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove-cloudflare-connect", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	if r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" {
		finish("denied")
		return
	}
	if current, e := r.Cookie("__Host-cove"); e == nil && !bytes.Equal(tokenHash(current.Value), session) {
		finish("expired")
		return
	}
	c, err := s.cloudflareOAuthClient(r.Context(), s.DB)
	if err != nil || c.ClientID != clientID {
		finish("unavailable")
		return
	}
	verifier, err = unseal(s.Key, verifier)
	if err != nil {
		finish("unavailable")
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		finish("unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var allowed bool
	err = tx.QueryRow(r.Context(), "SELECT NOT disabled AND email_verified AND auth_version=$2 AND EXISTS(SELECT 1 FROM sessions WHERE token_hash=$3 AND user_id=users.id AND expires_at>now()) FROM users WHERE id=$1 FOR SHARE", user, version, session).Scan(&allowed)
	if err != nil || !allowed {
		finish("expired")
		return
	}
	access, err := s.exchangeCloudflareToken(r.Context(), c, url.Values{"grant_type": {"authorization_code"}, "code": {r.URL.Query().Get("code")}, "redirect_uri": {s.PublicURL + "/api/cloudflare/oauth/callback"}, "code_verifier": {string(verifier)}})
	if err != nil || access.Refresh == "" {
		finish("unavailable")
		return
	}
	if _, err = s.discoverCloudflareZones(r.Context(), access.Token); err != nil {
		finish("unavailable")
		return
	}
	raw, err := json.Marshal(access)
	if err == nil {
		raw, err = seal(s.Key, raw)
	}
	id := randomToken()
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO cloudflare_accounts(id,owner_id,name,kind,credentials) VALUES($1,$2,'Cloudflare authorization','oauth',$3)", id, user, raw)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		finish("unavailable")
		return
	}
	finish("connected")
}
