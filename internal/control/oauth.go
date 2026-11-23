package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) githubStart(w http.ResponseWriter, r *http.Request) {
	config, _, configErr := s.integrationConfig(r.Context(), s.DB)
	if configErr != nil {
		http.Error(w, "GitHub settings unavailable", 503)
		return
	}
	if config.Github.ClientID == "" || config.Github.ClientSecret == "" {
		http.Error(w, "GitHub sign-in is not configured", 409)
		return
	}
	var in struct {
		Invitation string `json:"invitation_token"`
		Link       bool   `json:"link"`
		Password   string `json:"current_password"`
		Code       string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var link, invite *string
	var linkVersion *int64
	if in.Link {
		p, err := s.sessionIdentity(r)
		if err != nil {
			http.Error(w, "sign in before linking GitHub", 401)
			return
		}
		var hash []byte
		var version int64
		err = tx.QueryRow(r.Context(), "SELECT password_hash,auth_version FROM users WHERE id=$1 AND NOT disabled FOR SHARE", p.ID).Scan(&hash, &version)
		if err != nil {
			http.Error(w, "authentication unavailable", 503)
			return
		}
		if len(hash) == 0 {
			proof, _ := json.Marshal(map[string]string{"current_password": in.Password, "code": in.Code})
			request := r.Clone(context.WithValue(r.Context(), identityKey{}, p))
			request.Body = io.NopCloser(bytes.NewReader(proof))
			if !s.reauthenticate(w, request, tx) {
				return
			}
		} else if len(in.Password) > 72 || bcrypt.CompareHashAndPassword(hash, []byte(in.Password)) != nil {
			http.Error(w, "current password is incorrect", 400)
			return
		}
		link = &p.ID
		linkVersion = &version
	}
	if in.Invitation != "" {
		var id string
		err := s.DB.QueryRow(r.Context(), "SELECT id FROM invitations WHERE token_hash=$1 AND consumed_at IS NULL AND expires_at>now()", tokenHash(in.Invitation)).Scan(&id)
		if err != nil {
			http.Error(w, "invalid or expired invitation", 400)
			return
		}
		invite = &id
	}
	state, browser, verifier := randomToken(), randomToken(), randomToken()
	directoryUser := ""
	if link != nil {
		directoryUser = *link
	}
	directory, err := s.githubDirectoryNeedsOrgScope(r.Context(), directoryUser)
	if err != nil {
		http.Error(w, "GitHub settings unavailable", 503)
		return
	}
	var linkSession []byte
	if directory {
		cookie, _ := r.Cookie("__Host-cove")
		linkSession = tokenHash(cookie.Value)
	}
	encrypted, err := seal(s.Key, []byte(verifier))
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	_, err = tx.Exec(r.Context(), "INSERT INTO oauth_states(token_hash,browser_hash,verifier,link_user_id,invitation_id,link_version,expires_at,directory_sync,link_session_hash) VALUES($1,$2,$3,$4,$5,$6,now()+interval '10 minutes',$7,$8)", tokenHash(state), tokenHash(browser), encrypted, link, invite, linkVersion, directory, linkSession)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove-oauth", Value: browser, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {config.Github.ClientID}, "redirect_uri": {s.PublicURL + "/auth/github/callback"}, "scope": {"read:user user:email"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	if directory {
		q.Set("scope", "read:user user:email read:org")
	}
	respond(w, 200, map[string]string{"url": "https://github.com/login/oauth/authorize?" + q.Encode()})
}
func (s *Server) githubRequest(r *http.Request, method, endpoint, token string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(r.Context(), method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Cove")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	client := http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if s.OAuthClient != nil {
		client.Transport = s.OAuthClient.Transport
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New("provider request rejected")
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 1024*1024+1))
	if err != nil || len(b) > 1024*1024 {
		return errors.New("invalid provider response")
	}
	return json.Unmarshal(b, out)
}
func (s *Server) githubCallback(w http.ResponseWriter, r *http.Request) {
	ensureLoginDevice(w, r)
	status := 401
	userForEvent := ""
	directoryFlow := false
	defer func() {
		s.recordAudit(r, "GitHub callback", status)
		address, _, _ := net.SplitHostPort(r.RemoteAddr)
		if !directoryFlow {
			s.recordLogin(r, userForEvent, status == 200, "github", address)
		}
	}()
	fail := func(code string) {
		destination := "/login?error=" + code
		if directoryFlow {
			destination = "/admin?github_team_sync=error#github-team-sync"
		}
		http.Redirect(w, r, destination, http.StatusSeeOther)
	}
	cookie, err := r.Cookie("__Host-cove-oauth")
	if err != nil {
		fail("oauth_failed")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove-oauth", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	var browser, encrypted []byte
	var link, invite *string
	var linkVersion *int64
	var directory bool
	var linkSession []byte
	err = s.DB.QueryRow(r.Context(), "DELETE FROM oauth_states WHERE token_hash=$1 AND expires_at>now() RETURNING browser_hash,verifier,link_user_id,invitation_id,link_version,directory_sync,link_session_hash", tokenHash(r.URL.Query().Get("state"))).Scan(&browser, &encrypted, &link, &invite, &linkVersion, &directory, &linkSession)
	if err != nil || subtle.ConstantTimeCompare(browser, tokenHash(cookie.Value)) != 1 {
		fail("oauth_failed")
		return
	}
	directoryFlow = directory && link != nil
	config, _, configErr := s.integrationConfig(r.Context(), s.DB)
	if configErr != nil || config.Github.ClientID == "" || config.Github.ClientSecret == "" {
		fail("oauth_failed")
		return
	}
	verifier, err := unseal(s.Key, encrypted)
	if err != nil || r.URL.Query().Get("code") == "" {
		fail("oauth_failed")
		return
	}
	var access struct {
		Token string `json:"access_token"`
		Type  string `json:"token_type"`
		Scope string `json:"scope"`
	}
	err = s.githubRequest(r, "POST", "https://github.com/login/oauth/access_token", "", url.Values{"client_id": {config.Github.ClientID}, "client_secret": {config.Github.ClientSecret}, "code": {r.URL.Query().Get("code")}, "redirect_uri": {s.PublicURL + "/auth/github/callback"}, "code_verifier": {string(verifier)}}, &access)
	if err != nil || access.Token == "" || !strings.EqualFold(access.Type, "bearer") {
		fail("oauth_failed")
		return
	}
	var gh struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if s.githubRequest(r, "GET", "https://api.github.com/user", access.Token, nil, &gh) != nil || gh.ID <= 0 {
		fail("oauth_failed")
		return
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if s.githubRequest(r, "GET", "https://api.github.com/user/emails", access.Token, nil, &emails) != nil {
		fail("oauth_failed")
		return
	}
	verified := map[string]bool{}
	email := ""
	for _, e := range emails {
		normalized, valid := normalizeEmail(e.Email)
		if e.Verified && valid {
			verified[normalized] = true
			if email == "" || e.Primary {
				email = normalized
			}
		}
	}
	if email == "" {
		fail("oauth_failed")
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		fail("oauth_failed")
		return
	}
	defer tx.Rollback(r.Context())
	var team, inviteEmail, role string
	var grants json.RawMessage
	if invite != nil {
		err = tx.QueryRow(r.Context(), `SELECT i.team_id,i.email,i.role,i.grants FROM invitations i JOIN users creator ON creator.id=i.created_by WHERE i.id=$1 AND i.consumed_at IS NULL AND i.expires_at>now() AND NOT creator.disabled AND (creator.is_admin OR (cove_permission(i.created_by,i.team_id,'','','manage_members') AND (i.role<>'admin' OR EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=i.team_id AND m.user_id=i.created_by AND m.role IN ('owner','admin'))))) FOR UPDATE OF i`, invite).Scan(&team, &inviteEmail, &role, &grants)
		if err != nil || !verified[inviteEmail] {
			fail("oauth_failed")
			return
		}
		email = inviteEmail
	}
	var user string
	githubID := strconv.FormatInt(gh.ID, 10)
	err = tx.QueryRow(r.Context(), "SELECT id FROM users WHERE github_id=$1 AND NOT disabled FOR UPDATE", githubID).Scan(&user)
	if link != nil {
		if err == nil && user != *link {
			fail("account_exists")
			return
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			fail("oauth_failed")
			return
		}
		var localEmail string
		var currentGitHub *string
		var currentVersion int64
		err = tx.QueryRow(r.Context(), "SELECT COALESCE(email,''),github_id,auth_version FROM users WHERE id=$1 AND NOT disabled FOR UPDATE", link).Scan(&localEmail, &currentGitHub, &currentVersion)
		if err != nil || linkVersion == nil || currentVersion != *linkVersion || !verified[localEmail] || (currentGitHub != nil && *currentGitHub != githubID) {
			fail("oauth_failed")
			return
		}
		user = *link
		_, err = tx.Exec(r.Context(), "UPDATE users SET github_id=$1 WHERE id=$2", githubID, user)
	} else if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM users WHERE email=$1 OR github_id=$2)", email, githubID).Scan(&exists)
		if err != nil {
			fail("oauth_failed")
			return
		}
		if exists {
			fail("account_exists")
			return
		}
		var enabled bool
		err = tx.QueryRow(r.Context(), "SELECT registration_enabled FROM control_settings WHERE id=1 FOR SHARE").Scan(&enabled)
		if err != nil {
			fail("oauth_failed")
			return
		}
		if !enabled && invite == nil {
			fail("registration_closed")
			return
		}
		user = resourceID()
		name := gh.Login
		if name == "" || len(name) > 120 {
			name = "GitHub user"
		}
		_, err = tx.Exec(r.Context(), "INSERT INTO users(id,email,name,email_verified,github_id) VALUES($1,$2,$3,true,$4)", user, email, name, githubID)
	}
	if err != nil {
		fail("oauth_failed")
		return
	}
	if invite != nil {
		var localEmail string
		if tx.QueryRow(r.Context(), "SELECT email FROM users WHERE id=$1", user).Scan(&localEmail) != nil || localEmail != inviteEmail {
			fail("oauth_failed")
			return
		}
		_, err = tx.Exec(r.Context(), "INSERT INTO team_members(team_id,user_id,role,grants) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", team, user, role, grants)
		if err == nil {
			_, err = tx.Exec(r.Context(), "UPDATE invitations SET consumed_at=now() WHERE id=$1", invite)
		}
	}
	token := randomToken()
	expiry := time.Now().Add(12 * time.Hour)
	if err == nil && directoryFlow {
		var valid bool
		// The Strict account cookie is withheld on cross-site callbacks; the browser-bound state pins its live session.
		err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.user_id=$2 AND s.expires_at>now() AND u.is_admin AND NOT u.disabled AND (s.second_factor_verified OR NOT EXISTS(SELECT 1 FROM account_totp WHERE user_id=u.id AND confirmed) AND NOT EXISTS(SELECT 1 FROM passkeys WHERE user_id=u.id)))`, linkSession, user).Scan(&valid)
		if err != nil || !valid {
			fail("oauth_failed")
			return
		}
	}
	if err == nil && !directoryFlow {
		_, err = tx.Exec(r.Context(), "INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,$3)", tokenHash(token), user, expiry)
	}
	var pending bool
	if err == nil {
		err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM account_totp WHERE user_id=$1 AND confirmed) OR EXISTS(SELECT 1 FROM passkeys WHERE user_id=$1)", user).Scan(&pending)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail("oauth_failed")
		return
	}
	status = 200
	userForEvent = user
	granted := slices.Contains(strings.Fields(strings.ReplaceAll(access.Scope, ",", " ")), "read:org")
	retained := false
	if granted {
		if retained, err = s.storeGithubTeamOAuth(r.Context(), user, githubID, access.Token); err != nil {
			slog.Error("GitHub directory OAuth connection could not be retained")
			retained = false
		}
	}
	if directoryFlow {
		outcome := "connected"
		if !granted {
			outcome = "permission_required"
			status = 403
		} else if !retained {
			outcome = "error"
			status = 503
		}
		http.Redirect(w, r, "/admin?github_team_sync="+outcome+"#github-team-sync", http.StatusSeeOther)
		return
	}
	s.completeSignIn(w, r, user, token, expiry)
	destination := "/"
	if pending {
		destination = "/login?authenticated=1"
	}
	http.Redirect(w, r, destination, http.StatusSeeOther)
}
