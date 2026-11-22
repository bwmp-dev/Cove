package control

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

type githubAppCredentials struct {
	ClientID      string `json:"client_id,omitempty"`
	ClientSecret  string `json:"client_secret,omitempty"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
}

func (s *Server) appAPI(ctx context.Context, method, path, token string, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Cove")
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if s.OAuthClient != nil {
		client.Transport = s.OAuthClient.Transport
	}
	res, err := client.Do(req)
	if err != nil {
		return errors.New("GitHub is unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &githubStatusError{Status: res.StatusCode}
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 8*1024*1024)).Decode(output)
}

type githubStatusError struct{ Status int }

func (e *githubStatusError) Error() string { return fmt.Sprintf("GitHub returned HTTP %d", e.Status) }

func appJWT(id int64, keyPEM string, now time.Time) (string, error) {
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return "", errors.New("invalid GitHub key")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, e := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e != nil {
			return "", e
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("GitHub requires an RSA key")
		}
	}
	encode := base64.RawURLEncoding.EncodeToString
	claims, err := json.Marshal(map[string]any{"iss": id, "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix()})
	if err != nil {
		return "", err
	}
	unsigned := encode([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + encode(claims)
	hash := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + encode(signature), nil
}

func (s *Server) appCredentials(ctx context.Context) (int64, githubAppCredentials, error) {
	var id int64
	var encrypted []byte
	var credentials githubAppCredentials
	err := s.DB.QueryRow(ctx, "SELECT app_id,credentials FROM github_app WHERE id=1").Scan(&id, &encrypted)
	if err != nil {
		return id, credentials, err
	}
	plain, err := unseal(s.Key, encrypted)
	if err == nil {
		err = json.Unmarshal(plain, &credentials)
	}
	return id, credentials, err
}
func (s *Server) appToken(ctx context.Context) (string, error) {
	id, c, err := s.appCredentials(ctx)
	if err != nil {
		return "", err
	}
	return appJWT(id, c.PEM, time.Now())
}

func (s *Server) githubAppSetup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Organization string `json:"organization"`
	}
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 34 || len(in.Description) > 1000 || (in.Organization != "" && !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,38}$`).MatchString(in.Organization)) {
		http.Error(w, "invalid app name, description or organization", 400)
		return
	}
	var exists bool
	if err := s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM github_app)").Scan(&exists); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if exists {
		http.Error(w, "a GitHub App is already connected", 409)
		return
	}
	state, browser := randomToken(), randomToken()
	var version int64
	if err := s.DB.QueryRow(r.Context(), "SELECT auth_version FROM users WHERE id=$1", actor(r).ID).Scan(&version); err != nil {
		http.Error(w, "account unavailable", 503)
		return
	}
	if _, err := s.DB.Exec(r.Context(), "INSERT INTO github_app_setup(state_hash,browser_hash,user_id,user_version,expires_at) VALUES($1,$2,$3,$4,now()+interval '30 minutes')", tokenHash(state), tokenHash(browser), actor(r).ID, version); err != nil {
		http.Error(w, "could not start setup", 503)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove-app-setup", Value: browser, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 1800})
	action := "https://github.com/settings/apps/new"
	if in.Organization != "" {
		action = "https://github.com/organizations/" + in.Organization + "/settings/apps/new"
	}
	manifest := map[string]any{"name": in.Name, "description": in.Description, "url": s.PublicURL, "public": true, "redirect_url": s.PublicURL + "/api/github/app/callback", "setup_url": s.PublicURL + "/integrations", "hook_attributes": map[string]any{"url": s.PublicURL + "/hooks/github-app", "active": true}, "default_permissions": map[string]string{"contents": "read", "pull_requests": "write", "statuses": "write", "deployments": "write", "members": "read"}, "default_events": []string{"push", "pull_request", "release"}}
	manifest["callback_urls"] = []string{s.PublicURL + "/api/github/app/connect/callback"}
	b, err := json.Marshal(manifest)
	if err != nil {
		http.Error(w, "could not create manifest", 500)
		return
	}
	respond(w, 200, map[string]string{"action": action + "?state=" + url.QueryEscape(state), "manifest": string(b)})
}

func (s *Server) githubAppCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	code, state := r.URL.Query().Get("code"), r.URL.Query().Get("state")
	if !regexp.MustCompile(`^[a-zA-Z0-9]{1,200}$`).MatchString(code) || len(state) > 200 {
		http.Error(w, "invalid GitHub callback", 400)
		return
	}
	cookie, err := r.Cookie("__Host-cove-app-setup")
	if err != nil {
		http.Error(w, "setup browser could not be verified; start again", 400)
		return
	}
	result, err := s.DB.Exec(r.Context(), "DELETE FROM github_app_setup st USING users u WHERE st.state_hash=$1 AND st.browser_hash=$2 AND st.expires_at>now() AND u.id=st.user_id AND u.auth_version=st.user_version AND u.is_admin AND NOT u.disabled", tokenHash(state), tokenHash(cookie.Value))
	if err != nil || result.RowsAffected() != 1 {
		http.Error(w, "setup expired or belongs to another user; start again", 400)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-cove-app-setup", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	var app struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
		githubAppCredentials
	}
	if err = s.appAPI(r.Context(), "POST", "/app-manifests/"+code+"/conversions", "", nil, &app); err != nil {
		http.Error(w, "GitHub app creation failed; check the app in GitHub before retrying", 502)
		return
	}
	if app.ID <= 0 || !regexp.MustCompile(`^[a-zA-Z0-9-]+$`).MatchString(app.Slug) || app.WebhookSecret == "" {
		http.Error(w, "GitHub returned invalid app credentials", 502)
		return
	}
	if _, err = appJWT(app.ID, app.PEM, time.Now()); err != nil {
		http.Error(w, "GitHub returned an invalid private key", 502)
		return
	}
	raw, err := json.Marshal(app.githubAppCredentials)
	if err != nil {
		http.Error(w, "could not encode credentials", 500)
		return
	}
	encrypted, err := seal(s.Key, raw)
	if err != nil {
		http.Error(w, "could not protect credentials", 500)
		return
	}
	if _, err = s.DB.Exec(r.Context(), "INSERT INTO github_app(id,app_id,slug,credentials) VALUES(1,$1,$2,$3)", app.ID, app.Slug, encrypted); err != nil {
		http.Error(w, "could not save app; another setup may have completed", 409)
		return
	}
	http.Redirect(w, r, "/integrations", http.StatusSeeOther)
}

type appInstallation struct {
	ID      int64 `json:"id"`
	Account struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	} `json:"account"`
	SuspendedAt *time.Time `json:"suspended_at"`
}

func (s *Server) githubAppInfo(w http.ResponseWriter, r *http.Request) {
	var id int64
	var slug string
	err := s.DB.QueryRow(r.Context(), "SELECT app_id,slug FROM github_app WHERE id=1").Scan(&id, &slug)
	if errors.Is(err, pgx.ErrNoRows) {
		respond(w, 200, map[string]any{"configured": false})
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	rows, err := s.DB.Query(r.Context(), `SELECT i.id,i.account,($1 OR EXISTS(SELECT 1 FROM github_installation_managers m WHERE m.installation_id=i.id AND m.user_id=$3)),
 ARRAY(SELECT t.team_id FROM github_installation_teams t WHERE t.installation_id=i.id AND ($1 OR cove_access($3,t.team_id,'','')>=3) ORDER BY t.team_id),
 EXISTS(SELECT 1 FROM github_installation_teams t WHERE t.installation_id=i.id AND t.team_id=$2)
 FROM github_installations i WHERE $1 OR EXISTS(SELECT 1 FROM github_installation_managers m WHERE m.installation_id=i.id AND m.user_id=$3) OR EXISTS(SELECT 1 FROM github_installation_teams t WHERE t.installation_id=i.id AND t.team_id=$2 AND cove_access($3,t.team_id,'','')>=3) ORDER BY i.account`, actor(r).Admin, actor(r).Team, actor(r).ID)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	type installation struct {
		ID      int64    `json:"id"`
		Account string   `json:"account"`
		Teams   []string `json:"team_ids"`
		Manage  bool     `json:"can_manage"`
		Shared  bool     `json:"shared_with_current_team"`
	}
	list := []installation{}
	for rows.Next() {
		var i installation
		if err = rows.Scan(&i.ID, &i.Account, &i.Manage, &i.Teams, &i.Shared); err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		list = append(list, i)
	}
	if rows.Err() != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	_, credentials, err := s.appCredentials(r.Context())
	if err != nil {
		http.Error(w, "GitHub configuration unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"configured": true, "id": id, "slug": slug, "install_url": "https://github.com/apps/" + slug + "/installations/new", "installations": list, "user_auth_configured": credentials.ClientID != "" && credentials.ClientSecret != "", "callback_url": s.PublicURL + "/api/github/app/connect/callback"})
}
func (s *Server) syncGithubInstallations(w http.ResponseWriter, r *http.Request) {
	token, err := s.appToken(r.Context())
	if err != nil {
		http.Error(w, "GitHub app is not configured", 409)
		return
	}
	all := []appInstallation{}
	for page := 1; ; page++ {
		var list []appInstallation
		if err = s.appAPI(r.Context(), "GET", fmt.Sprintf("/app/installations?per_page=100&page=%d", page), token, nil, &list); err != nil {
			http.Error(w, "could not refresh GitHub installations", 502)
			return
		}
		all = append(all, list...)
		if len(list) < 100 {
			break
		}
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	for _, i := range all {
		if _, err = tx.Exec(r.Context(), "INSERT INTO github_installations(id,account) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET account=EXCLUDED.account", i.ID, i.Account.Login); err != nil {
			http.Error(w, "could not save installations", 500)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save installations", 500)
		return
	}
	s.githubAppInfo(w, r)
}
func (s *Server) installationToken(ctx context.Context, id, repo int64) (string, error) {
	return s.installationAccessToken(ctx, id, repo, map[string]string{"contents": "read"})
}

func (s *Server) installationAccessToken(ctx context.Context, id, repo int64, permissions map[string]string) (string, error) {
	jwt, err := s.appToken(ctx)
	if err != nil {
		return "", err
	}
	input := map[string]any{"permissions": permissions}
	if repo > 0 {
		input["repository_ids"] = []int64{repo}
	}
	var result struct {
		Token string `json:"token"`
	}
	err = s.appAPI(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", id), jwt, input, &result)
	if err == nil && result.Token == "" {
		err = errors.New("GitHub did not return an installation token")
	}
	return result.Token, err
}

type appRepository struct {
	ID             int64  `json:"id"`
	FullName       string `json:"full_name"`
	CloneURL       string `json:"clone_url"`
	DefaultBranch  string `json:"default_branch"`
	InstallationID int64  `json:"installation_id"`
}

func (s *Server) githubRepositories(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), "SELECT installation_id FROM github_installation_teams WHERE team_id=$1", actor(r).Team)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			http.Error(w, "database unavailable", 503)
			return
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	all := []appRepository{}
	provider := s.githubProvider()
	for _, id := range ids {
		repositories, err := provider.Repositories(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		all = append(all, repositories...)
	}
	respond(w, 200, all)
}

func (s *Server) githubSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method != "GET" && !s.requireAccess(w, r, "team", "", 3) {
		return
	}
	if r.Method == "GET" {
		var installation, repo int64
		var automatic bool
		err := s.DB.QueryRow(r.Context(), "SELECT installation_id,repository_id,automatic FROM github_sources WHERE workload_id=$1", id).Scan(&installation, &repo, &automatic)
		if errors.Is(err, pgx.ErrNoRows) {
			respond(w, 200, nil)
			return
		}
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		respond(w, 200, map[string]any{"installation_id": installation, "repository_id": repo, "automatic": automatic})
		return
	}
	var in struct {
		Installation  int64  `json:"installation_id"`
		Repository    int64  `json:"repository_id"`
		Branch        string `json:"branch"`
		ComposePath   string `json:"compose_path"`
		InlineCompose bool   `json:"inline_compose"`
		Automatic     bool   `json:"automatic"`
	}
	if r.Method != "DELETE" && !decode(w, r, &in) {
		return
	}
	var repo appRepository
	if r.Method != "DELETE" {
		var allowed bool
		if err := s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM github_installation_teams WHERE installation_id=$1 AND team_id=$2)", in.Installation, actor(r).Team).Scan(&allowed); err != nil || !allowed || in.Repository <= 0 {
			http.Error(w, "installation is not assigned to this team", 403)
			return
		}
		token, err := s.installationToken(r.Context(), in.Installation, in.Repository)
		if err != nil {
			http.Error(w, "repository access could not be verified", 502)
			return
		}
		if err = s.appAPI(r.Context(), "GET", fmt.Sprintf("/repositories/%d", in.Repository), token, nil, &repo); err != nil {
			http.Error(w, "repository is unavailable", 502)
			return
		}
		if repo.ID != in.Repository || !strings.HasPrefix(repo.CloneURL, "https://github.com/") {
			http.Error(w, "invalid repository", 502)
			return
		}
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var encrypted []byte
	var team string
	var connected bool
	if err = tx.QueryRow(r.Context(), "SELECT configuration,team_id,EXISTS(SELECT 1 FROM git_sources WHERE workload_id=$1) FROM workloads WHERE id=$1 FOR UPDATE", id).Scan(&encrypted, &team, &connected); err != nil {
		http.NotFound(w, r)
		return
	}
	if connected && r.Method != "DELETE" {
		http.Error(w, "disconnect the existing Git provider first", 409)
		return
	}
	var active bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))", id).Scan(&active); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if active {
		http.Error(w, "wait for the active operation", 409)
		return
	}
	if r.Method == "DELETE" {
		if _, err = tx.Exec(r.Context(), "DELETE FROM github_sources WHERE workload_id=$1", id); err != nil {
			http.Error(w, "could not disconnect", 500)
			return
		}
	} else {
		var assigned string
		if err = tx.QueryRow(r.Context(), "SELECT team_id FROM github_installation_teams WHERE installation_id=$1 AND team_id=$2 FOR SHARE", in.Installation, team).Scan(&assigned); err != nil || assigned != team {
			http.Error(w, "installation is not assigned to the resource team", 403)
			return
		}
		raw, err := unseal(s.Key, encrypted)
		var config workloadInput
		if err != nil || json.Unmarshal(raw, &config) != nil {
			http.Error(w, "configuration unavailable", 500)
			return
		}
		var watchPaths []string
		if in.InlineCompose && (config.Source == nil || !config.Source.InlineCompose) {
			http.Error(w, "save Cove-managed Git configuration in Source before connecting it", 409)
			return
		}
		if config.Source != nil && config.Source.Repository == repo.CloneURL {
			watchPaths = config.Source.WatchPaths
		}
		config.Source = &worker.GitSource{Repository: repo.CloneURL, Branch: in.Branch, ComposePath: in.ComposePath, InlineCompose: in.InlineCompose, WatchPaths: watchPaths}
		if err = validateWorkload(config); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		raw, err = json.Marshal(config)
		if err != nil {
			http.Error(w, "could not encode configuration", 500)
			return
		}
		encrypted, err = seal(s.Key, raw)
		if err != nil {
			http.Error(w, "could not protect configuration", 500)
			return
		}
		if _, err = tx.Exec(r.Context(), "UPDATE workloads SET configuration=$2,webhook_secret=NULL WHERE id=$1", id, encrypted); err != nil {
			http.Error(w, "could not save configuration", 500)
			return
		}
		if _, err = tx.Exec(r.Context(), "INSERT INTO github_sources(workload_id,installation_id,repository_id,repository,automatic) VALUES($1,$2,$3,$4,$5) ON CONFLICT(workload_id) DO UPDATE SET installation_id=EXCLUDED.installation_id,repository_id=EXCLUDED.repository_id,repository=EXCLUDED.repository,automatic=EXCLUDED.automatic", id, in.Installation, in.Repository, repo.CloneURL, in.Automatic); err != nil {
			http.Error(w, "could not connect repository", 500)
			return
		}
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM webhook_deliveries WHERE workload_id=$1 AND operation_id IS NULL", id); err != nil {
		http.Error(w, "could not clear old deliveries", 500)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save GitHub connection", 500)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) prepareAppDeployment(ctx context.Context, request *worker.Request) error {
	if request.Action == "deploy" && request.Source != nil && strings.HasPrefix(request.Source.Repository, "ssh://") {
		return s.prepareGitDeployKey(ctx, request)
	}

	if request.Action == "deploy" && request.Source != nil {
		handled, err := s.prepareConnectedGitDeployment(ctx, request)
		if handled || err != nil {
			return err
		}
	}
	if request.Action != "deploy" || request.Source == nil || request.Source.Token != "" {
		return nil
	}
	var installation, repo int64
	var repository string
	err := s.DB.QueryRow(ctx, "SELECT g.installation_id,g.repository_id,g.repository FROM github_sources g JOIN workloads w ON w.id=g.workload_id JOIN github_installation_teams i ON i.installation_id=g.installation_id AND i.team_id=w.team_id WHERE g.workload_id=$1", request.Workload).Scan(&installation, &repo, &repository)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if repository != request.Source.Repository {
		return errors.New("GitHub source changed; reconnect the repository")
	}
	token, err := s.githubProvider().CloneToken(ctx, installation, repo)
	if err != nil {
		return err
	}
	var reporting bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM github_reporting r LEFT JOIN previews p ON p.workload_id=$1 WHERE r.workload_id=COALESCE(p.source_id,$1) AND r.enabled)`, request.Workload).Scan(&reporting); err != nil {
		return err
	}
	if reporting && request.Source.Revision == "" {
		revision, err := s.githubProvider().BranchHead(ctx, installation, repo, request.Source.Branch)
		if err != nil {
			return err
		}
		if revision == "" {
			return errors.New("GitHub branch is unavailable")
		}
		request.Source.Revision = revision
	}
	request.Source.Token = token
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	encrypted, err := seal(s.Key, raw)
	if err != nil {
		return err
	}
	// Persist once before submitting: the worker requires identical input for retries.
	_, err = s.DB.Exec(ctx, "UPDATE operations SET payload=$2 WHERE id=$1", request.ID, encrypted)
	return err
}

func (s *Server) githubAppWebhook(w http.ResponseWriter, r *http.Request) {
	_, credentials, err := s.appCredentials(r.Context())
	if err != nil {
		http.Error(w, "GitHub app unavailable", 503)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
	if err != nil {
		http.Error(w, "invalid webhook", 400)
		return
	}
	if !s.githubProvider().VerifyWebhook([]byte(credentials.WebhookSecret), body, r.Header) {
		http.Error(w, "invalid signature", 401)
		return
	}
	if r.Header.Get("X-GitHub-Event") == "pull_request" {
		if err = s.previewWebhook(r.Context(), body, r.Header.Get("X-GitHub-Delivery")); err != nil {
			http.Error(w, "could not record pull request; retry delivery after checking preview configuration and quotas", 503)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	if r.Header.Get("X-GitHub-Event") == "release" {
		s.githubReleaseWebhook(w, r, body)
		return
	}
	if r.Header.Get("X-GitHub-Event") != "push" {
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	s.githubAppPush(w, r, body, "push")
}

func (s *Server) githubAppPush(w http.ResponseWriter, r *http.Request, body []byte, eventKind string) {
	var err error
	if !regexp.MustCompile(`^[a-zA-Z0-9-]{1,100}$`).MatchString(r.Header.Get("X-GitHub-Delivery")) {
		http.Error(w, "invalid push", 400)
		return
	}
	if err = s.previewBranchWebhook(r.Context(), body, r.Header.Get("X-GitHub-Delivery")); err != nil {
		http.Error(w, "could not record branch change; retry delivery", 503)
		return
	}
	var event struct {
		Ref          string `json:"ref"`
		After        string `json:"after"`
		Deleted      bool   `json:"deleted"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Repository appRepository `json:"repository"`
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if json.Unmarshal(body, &event) != nil || !regexp.MustCompile(`^[a-zA-Z0-9-]{1,100}$`).MatchString(delivery) {
		http.Error(w, "invalid push", 400)
		return
	}
	if event.Deleted {
		respond(w, 200, map[string]bool{"ignored": true})
		return
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(event.After) || event.After == strings.Repeat("0", 40) {
		http.Error(w, "invalid commit", 400)
		return
	}
	if strings.HasPrefix(event.Ref, "refs/tags/") && eventKind == "push" {
		revision, err := s.githubProvider().TagHead(r.Context(), event.Installation.ID, event.Repository.ID, strings.TrimPrefix(event.Ref, "refs/tags/"))
		if err != nil {
			http.Error(w, "tag commit could not be resolved", 502)
			return
		}
		event.After = revision
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	rows, err := tx.Query(r.Context(), "SELECT w.id,w.configuration,g.repository,COALESCE(t.kind,'branch'),COALESCE(t.pattern,'') FROM github_sources g JOIN workloads w ON w.id=g.workload_id LEFT JOIN git_source_triggers t ON t.workload_id=w.id JOIN github_installation_teams i ON i.installation_id=g.installation_id AND i.team_id=w.team_id WHERE g.installation_id=$1 AND g.repository_id=$2 AND g.automatic AND NOT w.manually_stopped FOR UPDATE OF w", event.Installation.ID, event.Repository.ID)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	requests := []worker.Request{}
	matchedPaths := map[string]bool{}
	for rows.Next() {
		var id, repository string
		var trigger gitSourceTrigger
		var encrypted []byte
		if err = rows.Scan(&id, &encrypted, &repository, &trigger.Kind, &trigger.Pattern); err != nil {
			break
		}
		raw, e := unseal(s.Key, encrypted)
		var c workloadInput
		if e != nil || json.Unmarshal(raw, &c) != nil {
			err = errors.New("invalid configuration")
			break
		}
		if c.Source == nil || c.Source.Repository != repository || !triggerMatches(trigger, eventKind, event.Ref, c.Source.Branch) {
			continue
		}
		matchedPaths[id] = eventKind != "push" || strings.HasPrefix(event.Ref, "refs/tags/") || pushMatchesWatchPaths(body, c.Source.WatchPaths)
		if strings.HasPrefix(event.Ref, "refs/tags/") {
			c.Source.Branch = strings.TrimPrefix(event.Ref, "refs/tags/")
		}
		c.Source.Revision = event.After
		c.Source.Token = ""
		requests = append(requests, worker.Request{ID: resourceID(), Workload: id, Action: "deploy", Source: c.Source, Compose: c.Compose, Environment: c.Environment, Kind: c.Kind, JobService: c.JobService, Routes: c.Routes, Private: c.Private, Limits: c.Limits, Hooks: c.Hooks, FileSecrets: c.FileSecrets, GuardedDeploy: c.GuardedDeploy})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		http.Error(w, "configuration unavailable", 500)
		return
	}
	requests, predecessors, err := s.sequenceWebhookRequests(r.Context(), tx, requests)
	if err != nil {
		http.Error(w, "deployment sequences unavailable", 503)
		return
	}
	requests = filterWebhookSequencePaths(requests, predecessors, matchedPaths)
	for id, matched := range matchedPaths {
		if !matched {
			if _, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status) VALUES('GitHub webhook','push skipped: no watched paths changed',$1,200)", id); err != nil {
				http.Error(w, "could not record webhook", 503)
				return
			}
		}
	}
	for _, request := range requests {
		raw, e := json.Marshal(request)
		if e != nil {
			http.Error(w, "could not encode deployment", 500)
			return
		}
		encrypted, e := seal(s.Key, raw)
		if e != nil {
			http.Error(w, "could not protect deployment", 500)
			return
		}
		if _, err = tx.Exec(r.Context(), "INSERT INTO webhook_deliveries(workload_id,delivery,payload,prerequisite_workload,prerequisite_delivery) VALUES($1,$2,$3,NULLIF($4,''),CASE WHEN $4='' THEN NULL ELSE $2 END) ON CONFLICT DO NOTHING", request.Workload, delivery, encrypted, predecessors[request.Workload]); err != nil {
			http.Error(w, "could not queue deployment", 500)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not queue deployment", 500)
		return
	}
	respond(w, 202, map[string]bool{"accepted": true})
}
