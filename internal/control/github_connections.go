package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

var githubAccountName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,38}$`)

func (s *Server) githubAppUserAuth(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClientID string `json:"client_id"`
		Secret   string `json:"client_secret"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`).MatchString(in.ClientID) || len(in.Secret) < 20 || len(in.Secret) > 256 || strings.ContainsAny(in.Secret, "\r\n") {
		http.Error(w, "valid GitHub App client ID and client secret required", 400)
		return
	}
	token, err := s.appToken(r.Context())
	var app struct {
		ClientID string `json:"client_id"`
	}
	if err != nil || s.appAPI(r.Context(), "GET", "/app", token, nil, &app) != nil {
		http.Error(w, "GitHub App could not be verified", 502)
		return
	}
	if app.ClientID != in.ClientID {
		http.Error(w, "client ID must belong to this instance's GitHub App", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var raw []byte
	var credentials githubAppCredentials
	if err = tx.QueryRow(r.Context(), "SELECT credentials FROM github_app WHERE id=1 FOR UPDATE").Scan(&raw); err == nil {
		raw, err = unseal(s.Key, raw)
	}
	if err != nil || json.Unmarshal(raw, &credentials) != nil {
		http.Error(w, "GitHub configuration unavailable", 503)
		return
	}
	credentials.ClientID, credentials.ClientSecret = in.ClientID, in.Secret
	raw, err = json.Marshal(credentials)
	if err == nil {
		raw, err = seal(s.Key, raw)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "UPDATE github_app SET credentials=$1 WHERE id=1", raw)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM github_installation_states")
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save GitHub credentials", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) githubInstallationStart(w http.ResponseWriter, r *http.Request) {
	sessionCookie, cookieErr := r.Cookie("__Host-cove")
	person, sessionErr := s.sessionIdentity(r)
	if cookieErr != nil || sessionErr != nil || person.ID != actor(r).ID {
		http.Error(w, "a browser session is required", 401)
		return
	}

	var in struct {
		Account string `json:"account"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !githubAccountName.MatchString(in.Account) {
		http.Error(w, "enter a GitHub username or organization name", 400)
		return
	}
	_, c, err := s.appCredentials(r.Context())
	if err != nil || c.ClientID == "" || c.ClientSecret == "" {
		http.Error(w, "an instance administrator must enable GitHub App user authorization first", 409)
		return
	}
	state, browser, verifier := randomToken(), randomToken(), randomToken()
	encrypted, err := seal(s.Key, []byte(verifier))
	if err != nil {
		http.Error(w, "could not start GitHub authorization", 503)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var version int64
	if err = tx.QueryRow(r.Context(), "SELECT auth_version FROM users WHERE id=$1 FOR UPDATE", actor(r).ID).Scan(&version); err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM github_installation_states WHERE user_id=$1 OR expires_at<=now()", actor(r).ID)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO github_installation_states(state_hash,browser_hash,verifier,user_id,user_version,team_id,account,session_hash,expires_at) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,now()+interval '10 minutes')", tokenHash(state), tokenHash(browser), encrypted, actor(r).ID, version, actor(r).Team, strings.ToLower(in.Account), tokenHash(sessionCookie.Value))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not start GitHub authorization", 503)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove-github-connect", Value: browser, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	challenge := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {c.ClientID}, "redirect_uri": {s.PublicURL + "/api/github/app/connect/callback"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	respond(w, 200, map[string]string{"url": "https://github.com/login/oauth/authorize?" + query.Encode()})
}

type githubAccount struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// Managing an installation permits sharing its selected repositories, so ordinary
// repository access is insufficient: require the personal owner or an organization admin.
func (s *Server) ownedGithubInstallation(ctx context.Context, token, account string) (appInstallation, error) {
	var user githubAccount
	if err := s.appAPI(ctx, "GET", "/user", token, nil, &user); err != nil {
		return appInstallation{}, err
	}
	ownerID, kind := user.ID, "users"
	if !strings.EqualFold(user.Login, account) {
		ownerID, kind = 0, "orgs"
		for page := 1; ; page++ {
			var memberships []struct {
				State        string        `json:"state"`
				Role         string        `json:"role"`
				Organization githubAccount `json:"organization"`
			}
			if err := s.appAPI(ctx, "GET", fmt.Sprintf("/user/memberships/orgs?state=active&per_page=100&page=%d", page), token, nil, &memberships); err != nil {
				return appInstallation{}, err
			}
			for _, m := range memberships {
				if m.State == "active" && m.Role == "admin" && strings.EqualFold(m.Organization.Login, account) {
					ownerID = m.Organization.ID
				}
			}
			if ownerID > 0 || len(memberships) < 100 {
				break
			}
		}
	}
	if ownerID <= 0 {
		return appInstallation{}, errors.New("GitHub account ownership could not be verified")
	}
	jwt, err := s.appToken(ctx)
	if err != nil {
		return appInstallation{}, err
	}
	var installation appInstallation
	if err = s.appAPI(ctx, "GET", "/"+kind+"/"+url.PathEscape(account)+"/installation", jwt, nil, &installation); err != nil {
		return installation, err
	}
	if installation.ID <= 0 || installation.Account.ID != ownerID || !strings.EqualFold(installation.Account.Login, account) || installation.SuspendedAt != nil {
		return installation, errors.New("GitHub installation is not available")
	}
	for page := 1; ; page++ {
		var result struct {
			Installations []appInstallation `json:"installations"`
		}
		if err = s.appAPI(ctx, "GET", fmt.Sprintf("/user/installations?per_page=100&page=%d", page), token, nil, &result); err != nil {
			return installation, err
		}
		for _, i := range result.Installations {
			if i.ID == installation.ID && i.Account.ID == ownerID && i.SuspendedAt == nil {
				return installation, nil
			}
		}
		if len(result.Installations) < 100 {
			return installation, errors.New("authorize access to the installation on GitHub")
		}
	}
}

func (s *Server) githubInstallationCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	redirectResult := func(result string) { http.Redirect(w, r, "/integrations?github="+result, http.StatusSeeOther) }
	cookie, err := r.Cookie("__Host-cove-github-connect")
	if err != nil || len(r.URL.Query().Get("state")) > 200 || r.URL.Query().Get("code") == "" {
		redirectResult("expired")
		return
	}
	var user, team, account string
	var version int64
	var encrypted, sessionHash []byte
	err = s.DB.QueryRow(r.Context(), "DELETE FROM github_installation_states WHERE state_hash=$1 AND browser_hash=$2 AND expires_at>now() RETURNING user_id,user_version,COALESCE(team_id,''),account,verifier,session_hash", tokenHash(r.URL.Query().Get("state")), tokenHash(cookie.Value)).Scan(&user, &version, &team, &account, &encrypted, &sessionHash)
	if err != nil {
		redirectResult("expired")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove-github-connect", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	// SameSite=Strict omits the session cookie on GitHub's redirect. The one-use
	// browser cookie binds this callback to the original, still-live session.
	if len(sessionHash) == 0 {
		redirectResult("expired")
		return
	}
	if current, e := r.Cookie("__Host-cove"); e == nil && !bytes.Equal(tokenHash(current.Value), sessionHash) {
		redirectResult("expired")
		return
	}
	verifier, err := unseal(s.Key, encrypted)
	if err != nil {
		redirectResult("unavailable")
		return
	}
	_, credentials, err := s.appCredentials(r.Context())
	if err != nil || credentials.ClientID == "" || credentials.ClientSecret == "" {
		redirectResult("unavailable")
		return
	}
	var access struct {
		Token string `json:"access_token"`
		Type  string `json:"token_type"`
	}
	err = s.githubRequest(r, "POST", "https://github.com/login/oauth/access_token", "", url.Values{"client_id": {credentials.ClientID}, "client_secret": {credentials.ClientSecret}, "code": {r.URL.Query().Get("code")}, "redirect_uri": {s.PublicURL + "/api/github/app/connect/callback"}, "code_verifier": {string(verifier)}}, &access)
	if err != nil || access.Token == "" || !strings.EqualFold(access.Type, "bearer") {
		redirectResult("unavailable")
		return
	}
	installation, err := s.ownedGithubInstallation(r.Context(), access.Token, account)
	if err != nil {
		redirectResult("access_denied")
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		redirectResult("unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var allowed bool
	err = tx.QueryRow(r.Context(), "SELECT NOT disabled AND email_verified AND auth_version=$2 AND EXISTS(SELECT 1 FROM sessions WHERE token_hash=$3 AND user_id=users.id AND expires_at>now()) FROM users WHERE id=$1 FOR SHARE", user, version, sessionHash).Scan(&allowed)
	if err != nil || !allowed {
		redirectResult("access_denied")
		return
	}
	_, err = tx.Exec(r.Context(), "INSERT INTO github_installations(id,account) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET account=EXCLUDED.account", installation.ID, installation.Account.Login)
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO github_installation_managers(installation_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING", installation.ID, user)
	}
	if err != nil {
		redirectResult("unavailable")
		return
	}
	_, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'github.connect',$2,200,NULLIF($3,''))", user, strconv.FormatInt(installation.ID, 10), team)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		redirectResult("unavailable")
		return
	}
	redirectResult("connected")
}

func (s *Server) disconnectGithubInstallation(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("installation"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid installation", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var found int64
	err = tx.QueryRow(r.Context(), "SELECT installation_id FROM github_installation_teams WHERE installation_id=$1 AND team_id=$2 FOR UPDATE", id, actor(r).Team).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	var used bool
	err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM github_sources g JOIN workloads w ON w.id=g.workload_id WHERE g.installation_id=$1 AND w.team_id=$2)", id, actor(r).Team).Scan(&used)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if used {
		http.Error(w, "disconnect this team's resources before removing the installation", 409)
		return
	}
	_, err = tx.Exec(r.Context(), "DELETE FROM github_installation_teams WHERE installation_id=$1 AND team_id=$2", id, actor(r).Team)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not disconnect installation", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
