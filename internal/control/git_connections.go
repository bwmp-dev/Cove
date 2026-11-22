package control

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

func (s *Server) gitConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), `SELECT id,configuration FROM git_connections WHERE team_id=$1 ORDER BY id`, actor(r).Team)
		if err != nil {
			http.Error(w, "connections unavailable", 503)
			return
		}
		defer rows.Close()
		list := []map[string]any{}
		for rows.Next() {
			var id int64
			var encrypted []byte
			if rows.Scan(&id, &encrypted) != nil {
				http.Error(w, "connections unavailable", 503)
				return
			}
			var c gitConnection
			if err := s.unsealJSON(encrypted, &c); err != nil {
				http.Error(w, "connections unavailable", 503)
				return
			}
			list = append(list, map[string]any{"id": id, "name": c.Name, "kind": c.Kind, "url": c.URL, "workspace": c.Workspace, "webhook_url": fmt.Sprintf("%s/hooks/git/%d", s.PublicURL, id)})
		}
		if rows.Err() != nil {
			http.Error(w, "connections unavailable", 503)
			return
		}
		respond(w, 200, list)
		return
	}
	if !s.requireAccess(w, r, "team", "", 3) {
		return
	}
	if r.Method == "DELETE" {
		id, err := strconv.ParseInt(r.PathValue("connection"), 10, 64)
		if err != nil {
			http.Error(w, "invalid connection", 400)
			return
		}
		result, err := s.DB.Exec(r.Context(), `DELETE FROM git_connections c WHERE id=$1 AND team_id=$2 AND NOT EXISTS(SELECT 1 FROM git_sources WHERE connection_id=c.id)`, id, actor(r).Team)
		if err != nil || result.RowsAffected() == 0 {
			http.Error(w, "connection is unavailable or used by a workload", 409)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	var in gitConnection
	if !decode(w, r, &in) {
		return
	}
	if len(in.AuthUser) > 254 || strings.ContainsAny(in.AuthUser, "\r\n\x00") || (in.Kind != "bitbucket" && in.AuthUser != "") || in.ID != 0 || in.Secret != "" || strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 || len(in.Token) < 8 || len(in.Token) > 8192 || strings.ContainsAny(in.Token, "\r\n\x00") {
		http.Error(w, "name and provider access token required", 400)
		return
	}
	if in.Kind == "forgejo" || in.Kind == "codeberg" {
		in.Kind = "gitea"
	}
	if in.Kind != "gitlab" && in.Kind != "gitea" && in.Kind != "bitbucket" {
		http.Error(w, "unsupported Git provider", 400)
		return
	}
	parsed, err := url.Parse(in.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || len(in.URL) > 2048 {
		http.Error(w, "provider URL must be HTTPS without credentials", 400)
		return
	}
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && !publicWebhookIP(ip) {
		http.Error(w, "provider URL must resolve to a public address", 400)
		return
	}
	in.URL = strings.TrimSuffix(in.URL, "/")
	switch in.Kind {
	case "gitlab":
		in.URL += "/api/v4"
	case "gitea":
		in.URL += "/api/v1"
	case "bitbucket":
		if in.URL != "https://api.bitbucket.org" || in.Workspace == "" || len(in.Workspace) > 100 || strings.ContainsAny(in.Workspace, "/\\\r\n") {
			http.Error(w, "Bitbucket requires https://api.bitbucket.org and a workspace", 400)
			return
		}
		in.URL += "/2.0"
	}
	p := connectedGitProvider{server: s, connection: in}
	var account struct {
		Username string `json:"username"`
		Login    string `json:"login"`
		UUID     string `json:"uuid"`
	}
	if err = p.api(r.Context(), "GET", "/user", nil, &account); err != nil {
		http.Error(w, "provider account could not be verified; check its URL and token", 502)
		return
	}
	in.Account = account.Username
	if in.Kind == "gitea" {
		in.Account = account.Login
	}
	if in.Kind == "bitbucket" {
		in.Account = account.UUID
	}
	if in.Account == "" || len(in.Account) > 100 || strings.ContainsAny(in.Account, "\r\n\x00") {
		http.Error(w, "provider returned no account identity", 502)
		return
	}
	secret := randomToken()
	if in.Kind == "gitlab" {
		key, err := base64.RawURLEncoding.DecodeString(secret)
		if err != nil {
			http.Error(w, "could not create signing key", 500)
			return
		}
		secret = "whsec_" + base64.StdEncoding.EncodeToString(key)
	}
	in.Secret = secret
	encrypted, err := s.sealPreview(in)
	if err != nil {
		http.Error(w, "could not protect connection", 500)
		return
	}
	var id int64
	if err = s.DB.QueryRow(r.Context(), `INSERT INTO git_connections(team_id,configuration) VALUES($1,$2) RETURNING id`, actor(r).Team, encrypted).Scan(&id); err != nil {
		http.Error(w, "could not save connection", 503)
		return
	}
	respond(w, 201, map[string]any{"id": id, "name": in.Name, "kind": in.Kind, "url": in.URL, "workspace": in.Workspace, "webhook_secret": secret, "webhook_url": fmt.Sprintf("%s/hooks/git/%d", s.PublicURL, id)})
}

func (s *Server) gitProviderRepositories(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("connection"), 10, 64)
	if err != nil {
		http.Error(w, "invalid connection", 400)
		return
	}
	if !s.gitConnectionAccess(w, r, id) {
		return
	}
	p, err := s.connectedGitProvider(r.Context(), id)
	if err != nil {
		http.Error(w, "connection unavailable", 503)
		return
	}
	if r.PathValue("repository") != "" {
		repository, err := strconv.ParseInt(r.PathValue("repository"), 10, 64)
		if err != nil {
			http.Error(w, "invalid repository", 400)
			return
		}
		branches, err := p.Branches(r.Context(), id, repository, "")
		if err != nil {
			http.Error(w, "provider branches unavailable", 502)
			return
		}
		respond(w, 200, branches)
		return
	}
	repositories, err := p.Repositories(r.Context(), id)
	if err != nil {
		http.Error(w, "provider repositories unavailable", 502)
		return
	}
	respond(w, 200, repositories)
}

func (s *Server) gitConnectionAccess(w http.ResponseWriter, r *http.Request, id int64) bool {
	var allowed bool
	if err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM git_connections WHERE id=$1 AND team_id=$2)`, id, actor(r).Team).Scan(&allowed); err != nil {
		http.Error(w, "connection unavailable", 503)
		return false
	}
	if !allowed {
		http.NotFound(w, r)
		return false
	}
	return true
}

func (s *Server) gitProviderSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "GET" {
		var connection, repository int64
		var automatic, reporting bool
		err := s.DB.QueryRow(r.Context(), `SELECT connection_id,repository_id,automatic,reporting FROM git_sources WHERE workload_id=$1`, id).Scan(&connection, &repository, &automatic, &reporting)
		if errors.Is(err, pgx.ErrNoRows) {
			respond(w, 200, nil)
			return
		}
		if err != nil {
			http.Error(w, "source unavailable", 503)
			return
		}
		reports, err := s.providerFeedbackReports(r.Context(), id)
		if err != nil {
			http.Error(w, "provider reporting unavailable", 503)
			return
		}
		respond(w, 200, map[string]any{"connection_id": connection, "repository_id": repository, "automatic": automatic, "reporting": reporting, "reports": reports})
		return
	}
	if !s.requireAccess(w, r, "workload", id, 3) || !s.requireAccess(w, r, "team", "", 3) || !s.requirePersistentResource(w, r, id) {
		return
	}
	var in struct {
		Connection  int64  `json:"connection_id"`
		Repository  int64  `json:"repository_id"`
		Branch      string `json:"branch"`
		ComposePath string `json:"compose_path"`
		Inline      bool   `json:"inline_compose"`
		Automatic   bool   `json:"automatic"`
		Reporting   bool   `json:"reporting"`
	}
	var selected appRepository
	if r.Method != "DELETE" {
		if !decode(w, r, &in) || !s.gitConnectionAccess(w, r, in.Connection) {
			return
		}
		p, err := s.connectedGitProvider(r.Context(), in.Connection)
		if err != nil {
			http.Error(w, "connection unavailable", 503)
			return
		}
		repos, err := p.Repositories(r.Context(), in.Connection)
		if err != nil {
			http.Error(w, "repository access could not be verified", 502)
			return
		}
		for _, repo := range repos {
			if repo.ID == in.Repository {
				selected = repo
				break
			}
		}
		if selected.ID == 0 {
			http.Error(w, "repository is unavailable through this connection", 403)
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
	var active, github bool
	if err = tx.QueryRow(r.Context(), `SELECT configuration,team_id,EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running')),EXISTS(SELECT 1 FROM github_sources WHERE workload_id=$1) FROM workloads WHERE id=$1 FOR UPDATE`, id).Scan(&encrypted, &team, &active, &github); err != nil {
		http.Error(w, "workload unavailable", 503)
		return
	}
	if active {
		http.Error(w, "wait for the active operation", 409)
		return
	}
	if r.Method == "DELETE" {
		_, err = tx.Exec(r.Context(), `DELETE FROM git_sources WHERE workload_id=$1`, id)
	} else {
		if github {
			http.Error(w, "disconnect GitHub before changing Git providers", 409)
			return
		}
		var shared bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM git_connections WHERE id=$1 AND team_id=$2 FOR SHARE)`, in.Connection, team).Scan(&shared); err != nil || !shared {
			http.Error(w, "connection is not shared with this workload's team", 403)
			return
		}
		var config workloadInput
		plain, err := unseal(s.Key, encrypted)
		if err != nil || json.Unmarshal(plain, &config) != nil {
			http.Error(w, "configuration unavailable", 503)
			return
		}
		if in.Inline && (config.Source == nil || !config.Source.InlineCompose) {
			http.Error(w, "save Cove-managed Git configuration first", 409)
			return
		}
		var paths []string
		if config.Source != nil && config.Source.Repository == selected.CloneURL {
			paths = config.Source.WatchPaths
		}
		config.Source = &worker.GitSource{Repository: selected.CloneURL, Branch: in.Branch, ComposePath: in.ComposePath, InlineCompose: in.Inline, WatchPaths: paths}
		if err = validateWorkload(config); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		saved, err := s.sealPreview(config)
		if err != nil {
			http.Error(w, "could not protect source", 500)
			return
		}
		if _, err = tx.Exec(r.Context(), `UPDATE workloads SET configuration=$2,webhook_secret=NULL WHERE id=$1`, id, saved); err != nil {
			http.Error(w, "could not save source", 503)
			return
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO git_sources(workload_id,connection_id,repository_id,automatic,reporting) VALUES($1,$2,$3,$4,$5) ON CONFLICT(workload_id) DO UPDATE SET connection_id=EXCLUDED.connection_id,repository_id=EXCLUDED.repository_id,automatic=EXCLUDED.automatic,reporting=EXCLUDED.reporting,connected_at=now()`, id, in.Connection, in.Repository, in.Automatic, in.Reporting)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `DELETE FROM webhook_deliveries WHERE workload_id=$1 AND operation_id IS NULL`, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save provider source", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

type providerPush struct {
	Event      string
	Repository string
	Ref        string
	Revision   string
	Deleted    bool
}

func (p connectedGitProvider) pushes(body []byte) ([]providerPush, error) {
	if p.connection.Kind != "bitbucket" {
		var event struct {
			Ref        string `json:"ref"`
			After      string `json:"after"`
			Deleted    bool   `json:"deleted"`
			Repository struct {
				ID json.Number `json:"id"`
			} `json:"repository"`
			Project struct {
				ID json.Number `json:"id"`
			} `json:"project"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return nil, err
		}
		repository := event.Repository.ID.String()
		if p.connection.Kind == "gitlab" {
			repository = event.Project.ID.String()
		}
		return []providerPush{{Repository: repository, Ref: event.Ref, Revision: event.After, Deleted: event.Deleted || event.After == strings.Repeat("0", 40)}}, nil
	}
	var event struct {
		Repository struct {
			UUID string `json:"uuid"`
		} `json:"repository"`
		Push struct {
			Changes []struct {
				New *struct {
					Type   string `json:"type"`
					Name   string `json:"name"`
					Target struct {
						Hash string `json:"hash"`
					} `json:"target"`
				} `json:"new"`
			} `json:"changes"`
		} `json:"push"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, err
	}
	out := []providerPush{}
	for _, change := range event.Push.Changes {
		if change.New == nil {
			continue
		}
		prefix := "refs/heads/"
		if change.New.Type == "tag" {
			prefix = "refs/tags/"
		} else if change.New.Type != "branch" {
			continue
		}
		out = append(out, providerPush{Repository: event.Repository.UUID, Ref: prefix + change.New.Name, Revision: change.New.Target.Hash})
	}
	return out, nil
}

func (s *Server) gitProviderWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("connection"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p, err := s.connectedGitProvider(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
	if err != nil {
		http.Error(w, "invalid webhook", 400)
		return
	}
	if !p.VerifyWebhook([]byte(p.connection.Secret), body, r.Header) {
		http.Error(w, "invalid signature", 401)
		return
	}
	event, delivery := r.Header.Get("X-Gitea-Event"), r.Header.Get("X-Gitea-Delivery")
	if p.connection.Kind == "gitea" && event == "" {
		event, delivery = r.Header.Get("X-Forgejo-Event"), r.Header.Get("X-Forgejo-Delivery")
	}
	switch p.connection.Kind {
	case "gitlab":
		event, delivery = r.Header.Get("X-Gitlab-Event"), r.Header.Get("webhook-id")
	case "bitbucket":
		event, delivery = r.Header.Get("X-Event-Key"), r.Header.Get("X-Request-UUID")
	}
	if delivery == "" || len(delivery) > 200 || strings.ContainsAny(delivery, "\r\n") {
		http.Error(w, "invalid delivery", 400)
		return
	}
	digest := sha256.Sum256([]byte(delivery))
	receipt := fmt.Sprintf("%s-%d-%s", p.connection.Kind, id, hex.EncodeToString(digest[:]))
	if event == "pull_request" || event == "pull_request_sync" || event == "Merge Request Hook" || strings.HasPrefix(event, "pullrequest:") {
		pr, err := p.PullRequest(r.Context(), body)
		if err != nil {
			http.Error(w, "invalid pull request", 400)
			return
		}
		if err = s.providerPreviewWebhook(r.Context(), p.connection.Kind, pr, receipt); err != nil {
			http.Error(w, "could not record preview event", 503)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	if event == "release" || event == "Release Hook" {
		var input struct {
			Action     string `json:"action"`
			Tag        string `json:"tag"`
			Repository struct {
				ID json.Number `json:"id"`
			} `json:"repository"`
			Project struct {
				ID json.Number `json:"id"`
			} `json:"project"`
			Release struct {
				Tag   string `json:"tag_name"`
				Draft bool   `json:"draft"`
			} `json:"release"`
		}
		if json.Unmarshal(body, &input) != nil {
			http.Error(w, "invalid release", 400)
			return
		}
		if input.Action != "published" && input.Action != "create" {
			respond(w, 200, map[string]bool{"ignored": true})
			return
		}
		tag, repository := input.Release.Tag, input.Repository.ID.String()
		if p.connection.Kind == "gitlab" {
			tag, repository = input.Tag, input.Project.ID.String()
		}
		if input.Release.Draft {
			respond(w, 200, map[string]bool{"ignored": true})
			return
		}
		if err = s.queueProviderPush(r.Context(), p, providerPush{Repository: repository, Ref: "refs/tags/" + tag, Event: "release"}, receipt, body); err != nil {
			http.Error(w, "could not queue release", 503)
			return
		}
		respond(w, 202, map[string]string{"status": "accepted"})
		return
	}
	if event != "push" && event != "Push Hook" && event != "Tag Push Hook" && event != "repo:push" {
		respond(w, 200, map[string]bool{"ignored": true})
		return
	}
	pushes, err := p.pushes(body)
	if err != nil {
		http.Error(w, "invalid push", 400)
		return
	}
	for _, push := range pushes {
		if err = s.queueProviderPush(r.Context(), p, push, receipt, body); err != nil {
			http.Error(w, "could not queue provider push", 503)
			return
		}
	}
	respond(w, 202, map[string]string{"status": "accepted"})
}

func (s *Server) queueProviderPush(ctx context.Context, p connectedGitProvider, push providerPush, delivery string, body []byte) error {
	if push.Deleted {
		return nil
	}
	if !strings.HasPrefix(push.Ref, "refs/heads/") && !strings.HasPrefix(push.Ref, "refs/tags/") {
		return errors.New("invalid push ref or revision")
	}
	if strings.HasPrefix(push.Ref, "refs/tags/") {
		var repository int64
		if err := s.DB.QueryRow(ctx, `SELECT id FROM git_repositories WHERE connection_id=$1 AND external_id=$2`, p.connection.ID, push.Repository).Scan(&repository); err != nil {
			return err
		}
		revision, err := p.TagHead(ctx, p.connection.ID, repository, strings.TrimPrefix(push.Ref, "refs/tags/"))
		if err != nil {
			return err
		}
		push.Revision = revision
	}
	if !gitCommit.MatchString(push.Revision) || push.Revision == strings.Repeat("0", 40) {
		return errors.New("invalid commit")
	}
	if push.Event == "" {
		push.Event = "push"
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT w.id,w.configuration,r.clone_url,COALESCE(t.kind,'branch'),COALESCE(t.pattern,'') FROM git_sources g JOIN workloads w ON w.id=g.workload_id LEFT JOIN git_source_triggers t ON t.workload_id=w.id JOIN git_repositories r ON r.id=g.repository_id JOIN git_connections c ON c.id=g.connection_id AND c.team_id=w.team_id WHERE g.connection_id=$1 AND r.external_id=$2 AND g.automatic AND NOT w.manually_stopped ORDER BY w.id FOR UPDATE OF w`, p.connection.ID, push.Repository)
	if err != nil {
		return err
	}
	requests := []worker.Request{}
	matched := map[string]bool{}
	for rows.Next() {
		var id, repository string
		var trigger gitSourceTrigger
		var encrypted []byte
		if err = rows.Scan(&id, &encrypted, &repository, &trigger.Kind, &trigger.Pattern); err != nil {
			break
		}
		var config workloadInput
		if err = s.unsealJSON(encrypted, &config); err != nil {
			break
		}
		if config.Source == nil || config.Source.Repository != repository || !triggerMatches(trigger, push.Event, push.Ref, config.Source.Branch) {
			continue
		}
		config.Source.Revision = push.Revision
		config.Source.Token = ""
		matched[id] = strings.HasPrefix(push.Ref, "refs/tags/") || pushMatchesWatchPaths(body, config.Source.WatchPaths)
		if strings.HasPrefix(push.Ref, "refs/tags/") {
			config.Source.Branch = strings.TrimPrefix(push.Ref, "refs/tags/")
		}
		requests = append(requests, worker.Request{ID: resourceID(), Workload: id, Action: "deploy", ComposeBundle: config.ComposeBundle, Compose: config.Compose, Source: config.Source, Environment: config.Environment, Kind: config.Kind, JobService: config.JobService, Routes: config.Routes, Private: config.Private, Limits: config.Limits, Hooks: config.Hooks, FileSecrets: config.FileSecrets, GuardedDeploy: config.GuardedDeploy})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	requests, predecessors, err := s.sequenceWebhookRequests(ctx, tx, requests)
	if err != nil {
		return err
	}
	requests = filterWebhookSequencePaths(requests, predecessors, matched)
	for _, request := range requests {
		if err = worker.ValidateRequest(request); err != nil {
			return err
		}
		payload, err := s.sealPreview(request)
		if err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries(workload_id,delivery,payload,prerequisite_workload,prerequisite_delivery) VALUES($1,$2,$3,NULLIF($4,''),CASE WHEN $4='' THEN NULL ELSE $2 END) ON CONFLICT DO NOTHING`, request.Workload, delivery, payload, predecessors[request.Workload])
		if err != nil {
			return err
		}
		if result.RowsAffected() > 0 {
			if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status,team_id) SELECT 'Git provider webhook','push',id,202,team_id FROM workloads WHERE id=$1`, request.Workload); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}
