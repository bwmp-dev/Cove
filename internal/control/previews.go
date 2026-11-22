package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type previewPolicy struct {
	Enabled     bool              `json:"enabled"`
	Hours       int               `json:"hours"`
	Domain      string            `json:"domain"`
	Service     string            `json:"service"`
	Port        int               `json:"port"`
	Environment map[string]string `json:"environment,omitempty"`
}

func (s *Server) requirePersistentResource(w http.ResponseWriter, r *http.Request, id string) bool {
	var preview bool
	if err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM previews WHERE workload_id=$1) OR EXISTS(SELECT 1 FROM environment_preview_resources WHERE resource_id=$1)`, id).Scan(&preview); err != nil {
		http.Error(w, "resource metadata unavailable", 503)
		return false
	}
	if preview {
		http.Error(w, "temporary previews cannot be used in persistent resource bindings or deployment plans", 409)
		return false
	}
	return true
}

func (s *Server) previews(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var project string
	if err := s.DB.QueryRow(r.Context(), `SELECT project_id FROM workloads WHERE id=$1`, id).Scan(&project); err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	needed := 1
	if r.Method != "GET" {
		needed = 3
	}
	if !s.requireAccess(w, r, "project", project, needed) {
		return
	}
	if r.Method == "GET" {
		policy := previewPolicy{Hours: 24}
		var encrypted []byte
		err := s.DB.QueryRow(r.Context(), `SELECT configuration FROM preview_policies WHERE workload_id=$1`, id).Scan(&encrypted)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "preview settings unavailable", 503)
			return
		}
		if err == nil {
			raw, e := unseal(s.Key, encrypted)
			if e != nil || json.Unmarshal(raw, &policy) != nil {
				http.Error(w, "preview settings unavailable", 503)
				return
			}
		}
		names := []string{}
		for name := range policy.Environment {
			names = append(names, name)
		}
		sort.Strings(names)
		policy.Environment = nil
		rows, err := s.DB.Query(r.Context(), `SELECT p.number,p.workload_id,p.environment_id,p.revision,p.expires_at,p.closing,w.manually_stopped FROM previews p JOIN workloads w ON w.id=p.workload_id WHERE p.source_id=$1 ORDER BY p.number`, id)
		if err != nil {
			http.Error(w, "previews unavailable", 503)
			return
		}
		defer rows.Close()
		list := []map[string]any{}
		for rows.Next() {
			var number int
			var resource, environment, revision string
			var expires time.Time
			var closing, stopped bool
			if err = rows.Scan(&number, &resource, &environment, &revision, &expires, &closing, &stopped); err != nil {
				break
			}
			list = append(list, map[string]any{"number": number, "resource_id": resource, "environment_id": environment, "revision": revision, "expires_at": expires, "closing": closing, "manually_stopped": stopped})
		}
		if err != nil || rows.Err() != nil {
			http.Error(w, "previews unavailable", 503)
			return
		}
		respond(w, 200, map[string]any{"policy": policy, "environment_keys": names, "previews": list})
		return
	}
	if r.Method == "POST" {
		var in struct {
			Number int `json:"number"`
		}
		if !decode(w, r, &in) {
			return
		}
		tx, err := s.DB.Begin(r.Context())
		if err != nil {
			http.Error(w, "previews unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		var target string
		if err = tx.QueryRow(r.Context(), `SELECT w.id FROM previews p JOIN workloads w ON w.id=p.workload_id WHERE p.source_id=$1 AND p.number=$2 AND NOT p.closing AND p.expires_at>now() FOR UPDATE OF w,p`, id, in.Number).Scan(&target); err != nil {
			http.Error(w, "preview is unavailable or closing", 409)
			return
		}
		var active bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))`, target).Scan(&active); err != nil {
			http.Error(w, "operations unavailable", 503)
			return
		}
		if active {
			http.Error(w, "wait for the active preview operation", 409)
			return
		}
		if _, err = tx.Exec(r.Context(), `UPDATE workloads SET manually_stopped=false WHERE id=$1`, target); err != nil {
			http.Error(w, "could not resume preview", 503)
			return
		}
		if _, err = tx.Exec(r.Context(), `UPDATE previews SET deployed_revision='',retry_after=now() WHERE workload_id=$1`, target); err != nil {
			http.Error(w, "could not deploy preview", 503)
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			http.Error(w, "could not deploy preview", 503)
			return
		}
		respond(w, 202, map[string]bool{"ok": true})
		return
	}
	if r.Method == "DELETE" {
		var in struct {
			Number int `json:"number"`
		}
		if !decode(w, r, &in) {
			return
		}
		result, err := s.DB.Exec(r.Context(), `UPDATE previews SET closing=true WHERE source_id=$1 AND number=$2`, id, in.Number)
		if err != nil {
			http.Error(w, "could not close preview", 503)
			return
		}
		if result.RowsAffected() == 0 {
			http.NotFound(w, r)
			return
		}
		respond(w, 202, map[string]bool{"ok": true})
		return
	}
	var in previewPolicy
	if !decode(w, r, &in) {
		return
	}
	if in.Hours < 1 || in.Hours > 168 {
		http.Error(w, "preview expiry must be 1-168 hours", 400)
		return
	}
	if in.Domain != "" {
		if err := worker.ValidateRoutes([]worker.Route{{Domain: "pr-1-test." + in.Domain, Service: in.Service, Port: in.Port}}, "service"); err != nil {
			http.Error(w, "select a valid preview domain suffix, service and port", 400)
			return
		}
	}
	for key, value := range in.Environment {
		if strings.Contains(value, "{{") {
			http.Error(w, "previews cannot reference shared or production variables", 400)
			return
		}
		if err := worker.ValidateRequest(worker.Request{ID: "validate", Workload: "validate", Action: "deploy", Compose: "services: {}", Environment: map[string]string{key: value}}); err != nil {
			http.Error(w, "invalid preview variables", 400)
			return
		}
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var connected, capable bool
	var sourceConfig []byte
	if err = tx.QueryRow(r.Context(), `SELECT (EXISTS(SELECT 1 FROM github_sources WHERE workload_id=w.id) OR EXISTS(SELECT 1 FROM git_sources WHERE workload_id=w.id)),COALESCE((n.capabilities->>'previews')::boolean,false),w.configuration FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w`, id).Scan(&connected, &capable, &sourceConfig); err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	if in.Enabled {
		raw, err := unseal(s.Key, sourceConfig)
		var config workloadInput
		if err != nil || json.Unmarshal(raw, &config) != nil {
			http.Error(w, "configuration unavailable", 503)
			return
		}
		if config.Source != nil && config.Source.InlineCompose {
			http.Error(w, "previews require a repository Compose file; Cove-managed configuration is not shared with pull requests", 409)
			return
		}
		if config.ComposeBundle != nil {
			http.Error(w, "restricted pull-request previews require one repository Compose file without profiles", 409)
			return
		}
	}
	if in.Enabled && (!connected || !capable) {
		http.Error(w, "connect a managed Git repository and upgrade and verify the worker before enabling previews", 409)
		return
	}
	if in.Environment == nil {
		var saved []byte
		err = tx.QueryRow(r.Context(), `SELECT configuration FROM preview_policies WHERE workload_id=$1`, id).Scan(&saved)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "settings unavailable", 503)
			return
		}
		if err == nil {
			raw, e := unseal(s.Key, saved)
			var old previewPolicy
			if e != nil || json.Unmarshal(raw, &old) != nil {
				http.Error(w, "settings unavailable", 503)
				return
			}
			in.Environment = old.Environment
		}
	}
	encrypted, err := s.sealPreview(in)
	if err != nil {
		http.Error(w, "could not protect settings", 500)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO preview_policies(workload_id,configuration) VALUES($1,$2) ON CONFLICT(workload_id) DO UPDATE SET configuration=EXCLUDED.configuration`, id, encrypted); err != nil {
		http.Error(w, "could not save settings", 503)
		return
	}
	if !in.Enabled {
		if _, err = tx.Exec(r.Context(), `UPDATE previews SET closing=true WHERE source_id=$1`, id); err != nil {
			http.Error(w, "could not close previews", 503)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save settings", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) sealPreview(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return seal(s.Key, raw)
}

type previewEvent struct {
	Action       string `json:"action"`
	Number       int    `json:"number"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		ID int64 `json:"id"`
	} `json:"repository"`
	PullRequest struct {
		UpdatedAt time.Time `json:"updated_at"`
		State     string    `json:"state"`
		Head      struct {
			SHA  string `json:"sha"`
			Ref  string `json:"ref"`
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref  string `json:"ref"`
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
}

func (s *Server) previewWebhook(ctx context.Context, body []byte, delivery string) error {
	event, parseErr := s.githubProvider().PullRequest(ctx, body)
	if parseErr != nil {
		return errors.New("invalid pull request")
	}
	return s.providerPreviewWebhook(ctx, "github", event, delivery)
}

func (s *Server) providerPreviewWebhook(ctx context.Context, kind string, event previewEvent, delivery string) error {
	if !regexp.MustCompile(`^[a-zA-Z0-9-]{1,150}$`).MatchString(delivery) || event.Number < 1 {
		return errors.New("invalid pull request")
	}
	if event.Action != "opened" && event.Action != "reopened" && event.Action != "synchronize" && event.Action != "closed" {
		return nil
	}
	if event.PullRequest.UpdatedAt.IsZero() {
		return errors.New("pull request timestamp required")
	}
	if event.Repository.ID == 0 || event.PullRequest.Head.Repo.ID != event.Repository.ID || event.PullRequest.Base.Repo.ID != event.Repository.ID {
		return nil
	}
	if event.Action != "closed" && !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(event.PullRequest.Head.SHA) {
		return errors.New("invalid preview revision")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	receipt, err := tx.Exec(ctx, `INSERT INTO preview_deliveries(delivery) VALUES($1) ON CONFLICT DO NOTHING`, delivery)
	if err != nil {
		return err
	}
	if receipt.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	rows, err := tx.Query(ctx, `SELECT w.id,w.node_id,w.project_id,w.team_id,w.configuration,p.configuration,g.repository,g.installation_id,g.repository_id FROM workloads w JOIN preview_policies p ON p.workload_id=w.id JOIN LATERAL (
 SELECT g.repository,g.installation_id,g.repository_id FROM github_sources g JOIN github_installation_teams i ON i.installation_id=g.installation_id AND i.team_id=w.team_id WHERE g.workload_id=w.id AND $3='github'
 UNION ALL
 SELECT r.clone_url,g.connection_id,g.repository_id FROM git_sources g JOIN git_repositories r ON r.id=g.repository_id JOIN git_connections c ON c.id=g.connection_id AND c.team_id=w.team_id WHERE g.workload_id=w.id AND $3<>'github'
 ) g ON true WHERE g.installation_id=$1 AND g.repository_id=$2 ORDER BY w.id FOR UPDATE OF w`, event.Installation.ID, event.Repository.ID, kind)
	if err != nil {
		return err
	}
	type source struct {
		id, node, project, team, repository string
		config, policy                      []byte
		installation, repo                  int64
	}
	sources := []source{}
	for rows.Next() {
		var item source
		if err = rows.Scan(&item.id, &item.node, &item.project, &item.team, &item.config, &item.policy, &item.repository, &item.installation, &item.repo); err != nil {
			break
		}
		sources = append(sources, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range sources {
		accepted, err := tx.Exec(ctx, `INSERT INTO preview_pull_requests(source_id,number,updated_at,closed) VALUES($1,$2,$3,$4) ON CONFLICT(source_id,number) DO UPDATE SET updated_at=EXCLUDED.updated_at,closed=EXCLUDED.closed WHERE EXCLUDED.updated_at>preview_pull_requests.updated_at OR (EXCLUDED.updated_at=preview_pull_requests.updated_at AND (NOT preview_pull_requests.closed OR EXCLUDED.closed))`, item.id, event.Number, event.PullRequest.UpdatedAt, event.Action == "closed")
		if err != nil {
			return err
		}
		if accepted.RowsAffected() == 0 {
			continue
		}
		if event.Action == "closed" {
			if _, err = tx.Exec(ctx, `UPDATE previews SET closing=true WHERE source_id=$1 AND number=$2`, item.id, event.Number); err != nil {
				return err
			}
			continue
		}
		var policy previewPolicy
		raw, err := unseal(s.Key, item.policy)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &policy); err != nil {
			return err
		}
		if !policy.Enabled {
			continue
		}
		var original workloadInput
		raw, err = unseal(s.Key, item.config)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &original); err != nil {
			return err
		}
		if original.Source == nil || original.Source.InlineCompose || original.Kind == "job" || original.Source.Repository != item.repository || original.Source.Branch != event.PullRequest.Base.Ref || event.PullRequest.State != "open" {
			continue
		}
		git := *original.Source
		git.Branch = event.PullRequest.Head.Ref
		git.Revision = event.PullRequest.Head.SHA
		git.Token = ""
		git.WatchPaths = nil
		target, environment := resourceID(), resourceID()
		var closing bool
		err = tx.QueryRow(ctx, `SELECT workload_id,environment_id,closing FROM previews WHERE source_id=$1 AND number=$2 FOR UPDATE`, item.id, event.Number).Scan(&target, &environment, &closing)
		fresh := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !fresh {
			return err
		}
		if closing {
			continue
		}
		config := workloadInput{Name: fmt.Sprintf("PR #%d", event.Number), NodeID: item.node, Kind: "service", Source: &git, Environment: policy.Environment}
		if policy.Domain != "" {
			config.Routes = []worker.Route{{Domain: fmt.Sprintf("pr-%d-%s.%s", event.Number, target[:8], policy.Domain), Service: policy.Service, Port: policy.Port}}
		}
		if err = validateWorkload(config); err != nil {
			return err
		}
		encrypted, err := s.sealPreview(config)
		if err != nil {
			return err
		}
		if fresh {
			var count int
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM previews WHERE source_id=$1`, item.id).Scan(&count); err != nil {
				return err
			}
			if count >= 5 {
				continue
			}
			conflict, err := s.routeConflict(ctx, tx, target, config.Routes)
			if err != nil {
				return err
			}
			if conflict != "" {
				return errors.New("preview hostname conflict")
			}
			if _, err = tx.Exec(ctx, `INSERT INTO environments(id,project_id,team_id,name) VALUES($1,$2,$3,$4)`, environment, item.project, item.team, fmt.Sprintf("PR %d · %s", event.Number, target[:8])); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO workloads(id,node_id,name,configuration,kind,environment_id,team_id,project_id,configured_cpu,configured_memory_mb) VALUES($1,$2,$3,$4,'service',$5,$6,$7,4,2048)`, target, item.node, config.Name, encrypted, environment, item.team, item.project); err != nil {
				return err
			}
			if kind == "github" {
				if _, err = tx.Exec(ctx, `INSERT INTO github_sources(workload_id,installation_id,repository_id,repository,automatic) VALUES($1,$2,$3,$4,false)`, target, item.installation, item.repo, item.repository); err != nil {
					return err
				}
			} else {
				if _, err = tx.Exec(ctx, `INSERT INTO git_sources(workload_id,connection_id,repository_id,automatic,reporting,connected_at) SELECT $1,connection_id,repository_id,false,reporting,connected_at FROM git_sources WHERE workload_id=$2`, target, item.id); err != nil {
					return err
				}
			}
			if _, err = tx.Exec(ctx, `INSERT INTO previews(source_id,number,workload_id,environment_id,revision,expires_at) VALUES($1,$2,$3,$4,$5,now()+$6*interval '1 hour')`, item.id, event.Number, target, environment, git.Revision, policy.Hours); err != nil {
				return err
			}
		} else {
			// Existing preview routes remain immutable until cleanup, including policy changes.
			var saved []byte
			if err = tx.QueryRow(ctx, `SELECT configuration FROM workloads WHERE id=$1 FOR UPDATE`, target).Scan(&saved); err != nil {
				return err
			}
			raw, err = unseal(s.Key, saved)
			if err != nil {
				return err
			}
			var old workloadInput
			if err = json.Unmarshal(raw, &old); err != nil {
				return err
			}
			config.Routes = old.Routes
			encrypted, err = s.sealPreview(config)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE workloads SET configuration=$2 WHERE id=$1`, target, encrypted); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE previews SET revision=$3,expires_at=now()+$4*interval '1 hour' WHERE source_id=$1 AND number=$2`, item.id, event.Number, git.Revision, policy.Hours); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func (s *Server) reconcilePreviews(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT workload_id FROM previews WHERE retry_after<=now() ORDER BY expires_at LIMIT 100`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.reconcilePreview(ctx, id); err != nil {
			slog.Error("preview reconciliation failed", "resource", id, "error", err)
			if _, updateErr := s.DB.Exec(ctx, `UPDATE previews SET retry_after=now()+interval '1 minute' WHERE workload_id=$1`, id); updateErr != nil {
				return updateErr
			}
		}
	}
	return nil
}

func (s *Server) reconcilePreview(ctx context.Context, id string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var encrypted []byte
	var closing, stopped bool
	var revision, deployed, cleanup, environment string
	err = tx.QueryRow(ctx, `SELECT w.configuration,w.manually_stopped,p.closing OR p.expires_at<=now(),p.revision,p.deployed_revision,p.cleanup_operation,p.environment_id FROM previews p JOIN workloads w ON w.id=p.workload_id WHERE w.id=$1 FOR UPDATE OF w,p`, id).Scan(&encrypted, &stopped, &closing, &revision, &deployed, &cleanup, &environment)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var active bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))`, id).Scan(&active); err != nil {
		return err
	}
	if closing {
		if _, err = tx.Exec(ctx, `UPDATE preview_pull_requests SET closed=true WHERE source_id=(SELECT source_id FROM previews WHERE workload_id=$1) AND number=(SELECT number FROM previews WHERE workload_id=$1)`, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE previews SET closing=true WHERE workload_id=$1`, id); err != nil {
			return err
		}
		if active {
			if _, err = tx.Exec(ctx, `UPDATE operations SET cancel_requested=true WHERE workload_id=$1 AND action<>'preview_remove' AND state IN ('queued','running')`, id); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		if cleanup != "" {
			var state string
			if err = tx.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1`, cleanup).Scan(&state); err != nil {
				return err
			}
			if state == "succeeded" {
				for _, query := range []string{`DELETE FROM previews WHERE workload_id=$1`, `DELETE FROM operations WHERE workload_id=$1`, `DELETE FROM workloads WHERE id=$1`} {
					if _, err = tx.Exec(ctx, query, id); err != nil {
						return err
					}
				}
				if _, err = tx.Exec(ctx, `DELETE FROM environments e WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM workloads WHERE environment_id=e.id)`, environment); err != nil {
					return err
				}
				return tx.Commit(ctx)
			}
			var ready bool
			if err = tx.QueryRow(ctx, `SELECT updated_at<now()-interval '1 minute' FROM operations WHERE id=$1`, cleanup).Scan(&ready); err != nil {
				return err
			}
			if !ready {
				return tx.Commit(ctx)
			}
		}
		request := worker.Request{ID: resourceID(), Workload: id, Action: "preview_remove", Preview: true}
		payload, err := s.sealPreview(request)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'preview_remove',$3)`, request.ID, id, payload); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE previews SET cleanup_operation=$2 WHERE workload_id=$1`, id, request.ID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if active || stopped || revision == deployed {
		return tx.Commit(ctx)
	}
	raw, err := unseal(s.Key, encrypted)
	if err != nil {
		return err
	}
	var config workloadInput
	if err = json.Unmarshal(raw, &config); err != nil {
		return err
	}
	request := worker.Request{ID: resourceID(), Workload: id, Action: "deploy", Preview: true, Source: config.Source, Environment: config.Environment, Routes: config.Routes, Kind: "service"}
	if err = worker.ValidateRequest(request); err != nil {
		return err
	}
	payload, err := s.sealPreview(request)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'deploy',$3)`, request.ID, id, payload); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE previews SET deployed_revision=$2 WHERE workload_id=$1`, id, revision); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
