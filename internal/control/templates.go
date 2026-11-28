package control

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"cove/internal/catalog"
	"github.com/jackc/pgx/v5"
)

type templateEntry struct {
	Origin         string           `json:"origin"`
	Repository     string           `json:"repository_id"`
	RepositoryName string           `json:"repository_name"`
	RepositoryURL  string           `json:"repository_url"`
	Revision       string           `json:"revision"`
	Warnings       []string         `json:"warnings"`
	Template       catalog.Template `json:"template"`
	Validation     catalog.Evidence `json:"validation"`
}

func (s *Server) catalogue(ctx context.Context, team string) ([]templateEntry, error) {
	index, revision, err := catalog.Builtin()
	if err != nil {
		return nil, err
	}
	entries := []templateEntry{}
	appendTemplates := func(index catalog.Index, origin, repository, revision string) error {
		for _, t := range index.Templates {
			warnings, err := catalog.Validate(t)
			if err != nil {
				return err
			}
			validation := catalog.Evidence{SchemaValidated: true, Runtime: "config_only", Message: "Compose schema validated; runtime has not been checked. Run preflight on your worker."}
			if origin == "builtin" {
				validation, err = catalog.BuiltinEvidence(t)
				if err != nil {
					return err
				}
			}
			entries = append(entries, templateEntry{Origin: origin, Repository: repository, Revision: revision, Warnings: warnings, Template: t, Validation: validation})
		}
		return nil
	}
	if err = appendTemplates(index, "builtin", "", revision); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, "SELECT id,revision,snapshot,last_error,name,url FROM template_repositories WHERE (team_id IS NULL OR team_id=$1) AND snapshot IS NOT NULL ORDER BY id", team)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, revision, refreshError, name, address string
		var snapshot []byte
		if err = rows.Scan(&id, &revision, &snapshot, &refreshError, &name, &address); err != nil {
			rows.Close()
			return nil, err
		}
		index, err = catalog.Decode(snapshot)
		if err != nil {
			rows.Close()
			return nil, err
		}
		start := len(entries)
		if err = appendTemplates(index, "repository", id, revision); err != nil {
			rows.Close()
			return nil, err
		}
		for i := start; i < len(entries); i++ {
			entries[i].RepositoryName, entries[i].RepositoryURL = name, address
			if refreshError != "" {
				entries[i].Warnings = append(entries[i].Warnings, "The source could not be refreshed; this is its last validated revision.")
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.DB.Query(ctx, "SELECT definition,revision FROM team_templates WHERE team_id=$1 ORDER BY id", team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		var revision string
		if err = rows.Scan(&data, &revision); err != nil {
			return nil, err
		}
		var t catalog.Template
		if json.Unmarshal(data, &t) != nil {
			return nil, errors.New("saved template unavailable")
		}
		if err = appendTemplates(catalog.Index{Templates: []catalog.Template{t}}, "team", "", revision); err != nil {
			return nil, err
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Template.Name < entries[j].Template.Name })
	return entries, nil
}

func (s *Server) templates(w http.ResponseWriter, r *http.Request) {
	entries, err := s.catalogue(r.Context(), actor(r).Team)
	if err != nil {
		http.Error(w, "template catalogue unavailable", 503)
		return
	}
	if r.Method == "GET" {
		respond(w, 200, map[string]any{"schema_version": 1, "templates": entries, "can_manage_team": s.teamAdmin(r, actor(r).Team)})
		return
	}
	var in struct {
		Origin       string            `json:"origin"`
		Repository   string            `json:"repository_id"`
		ID           string            `json:"id"`
		Revision     string            `json:"revision"`
		Values       map[string]string `json:"values"`
		Hostname     string            `json:"hostname"`
		Acknowledged bool              `json:"acknowledge_host_access"`
	}
	if !decode(w, r, &in) {
		return
	}
	for _, entry := range entries {
		if entry.Origin != in.Origin || entry.Repository != in.Repository || entry.Template.ID != in.ID {
			continue
		}
		if entry.Revision != in.Revision {
			http.Error(w, "template revision changed; review it again", 409)
			return
		}
		draft, err := catalog.Draft(entry.Template, in.Values, in.Hostname, in.Acknowledged)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		respond(w, 200, map[string]any{"name": entry.Template.ID, "compose": draft.Compose, "environment": draft.Environment, "routes": draft.Routes, "file_secrets": draft.FileSecrets})
		return
	}
	http.NotFound(w, r)
}

type templateRepository struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	URL        string     `json:"url"`
	Ref        string     `json:"ref"`
	Revision   string     `json:"revision"`
	Error      string     `json:"error"`
	Checked    *time.Time `json:"checked_at"`
	Next       time.Time  `json:"next_refresh_at"`
	Refreshing bool       `json:"refreshing"`
	Count      int        `json:"template_count"`
}

func (s *Server) templateRepositories(w http.ResponseWriter, r *http.Request) {
	team := ""
	if strings.HasPrefix(r.URL.Path, "/api/teams/") {
		team = r.PathValue("id")
		if !s.teamAdmin(r, team) {
			http.Error(w, "team administrator required", 403)
			return
		}
	} else if !actor(r).Admin {
		http.Error(w, "instance administrator required", 403)
		return
	}
	id := r.PathValue("repository")
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), `SELECT id,name,kind,url,ref,revision,last_error,checked_at,next_refresh_at,refresh_until>now(),COALESCE(jsonb_array_length(snapshot->'templates'),0) FROM template_repositories WHERE team_id IS NOT DISTINCT FROM NULLIF($1,'') ORDER BY name`, team)
		if err != nil {
			http.Error(w, "template repositories unavailable", 503)
			return
		}
		defer rows.Close()
		out := []templateRepository{}
		for rows.Next() {
			var v templateRepository
			if rows.Scan(&v.ID, &v.Name, &v.Kind, &v.URL, &v.Ref, &v.Revision, &v.Error, &v.Checked, &v.Next, &v.Refreshing, &v.Count) != nil {
				http.Error(w, "template repositories unavailable", 503)
				return
			}
			out = append(out, v)
		}
		if rows.Err() != nil {
			http.Error(w, "template repositories unavailable", 503)
			return
		}
		respond(w, 200, map[string]any{"repositories": out})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/refresh") {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(90 * time.Second))
		var exists bool
		if err := s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM template_repositories WHERE id=$1 AND team_id IS NOT DISTINCT FROM NULLIF($2,''))", id, team).Scan(&exists); err != nil {
			http.Error(w, "repository unavailable", 503)
			return
		}
		if !exists {
			http.NotFound(w, r)
			return
		}
		if err := s.refreshTemplateRepository(r.Context(), id, actor(r).ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "repository is already refreshing or was removed", 409)
			} else {
				http.Error(w, "catalogue refresh failed; last validated revision was retained", 422)
			}
			return
		}
		respond(w, 200, map[string]bool{"refreshed": true})
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "repositories unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended('cove-template-repositories:'||$1,0))`, team); err != nil {
		http.Error(w, "repositories unavailable", 503)
		return
	}
	action := "remove template repository"
	if r.Method == "POST" {
		var in struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
			URL  string `json:"url"`
			Ref  string `json:"ref"`
		}
		if !decode(w, r, &in) {
			return
		}
		if strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 || !templateSourceURL(in.URL) || in.Kind != "git" && in.Kind != "index" || in.Kind == "git" && !validTemplateGitReference(in.Ref) || in.Kind == "index" && in.Ref != "" {
			http.Error(w, "name, public HTTPS Git/index URL and explicit Git reference required", 400)
			return
		}
		var count int
		if err = tx.QueryRow(r.Context(), "SELECT count(*) FROM template_repositories WHERE team_id IS NOT DISTINCT FROM NULLIF($1,'')", team).Scan(&count); err != nil {
			http.Error(w, "repositories unavailable", 503)
			return
		}
		if count >= 8 {
			http.Error(w, "at most eight repositories per scope", 409)
			return
		}
		id = resourceID()
		_, err = tx.Exec(r.Context(), "INSERT INTO template_repositories(id,team_id,name,kind,url,ref) VALUES($1,NULLIF($2,''),$3,$4,$5,$6)", id, team, in.Name, in.Kind, in.URL, in.Ref)
		action = "add template repository"
	} else {
		deleted, e := tx.Exec(r.Context(), "DELETE FROM template_repositories WHERE id=$1 AND team_id IS NOT DISTINCT FROM NULLIF($2,'')", id, team)
		err = e
		if err == nil && deleted.RowsAffected() == 0 {
			http.NotFound(w, r)
			return
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,$2,$3,200,NULLIF($4,''))", actor(r).ID, action, id, team)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save repository change", 503)
		return
	}
	respond(w, 200, map[string]string{"id": id})
}

func (s *Server) refreshTemplateRepository(ctx context.Context, id, by string) error {
	var kind, address, ref string
	var team *string
	err := s.DB.QueryRow(ctx, `UPDATE template_repositories SET refresh_until=now()+interval '90 seconds',next_refresh_at=now() WHERE id=$1 AND refresh_until<=now() RETURNING kind,url,ref,team_id`, id).Scan(&kind, &address, &ref, &team)
	if err != nil {
		return err
	}
	fetch := s.fetchTemplateIndex
	if s.templateFetch != nil {
		fetch = s.templateFetch
	}
	data, revision, fetchErr := fetch(ctx, kind, address, ref)
	if fetchErr == nil {
		_, fetchErr = catalog.Decode(data)
	}
	message := ""
	if fetchErr != nil {
		message = "Fetch or template validation failed; previous validated snapshot retained."
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var updated int64
	if fetchErr == nil {
		result, e := tx.Exec(ctx, "UPDATE template_repositories SET snapshot=$2,revision=$3,checked_at=now(),last_error='',refresh_until=now(),next_refresh_at=now()+interval '6 hours' WHERE id=$1", id, data, revision)
		err, updated = e, result.RowsAffected()
	} else {
		result, e := tx.Exec(ctx, "UPDATE template_repositories SET checked_at=now(),last_error=$2,refresh_until=now(),next_refresh_at=now()+interval '6 hours' WHERE id=$1", id, message)
		err, updated = e, result.RowsAffected()
	}
	if err == nil && updated == 0 {
		return pgx.ErrNoRows
	}
	status := 200
	if fetchErr != nil {
		status = 422
	}
	if err == nil {
		_, err = tx.Exec(ctx, "INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'refresh template repository',$2,$3,$4)", by, id, status, team)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	return fetchErr
}

func (s *Server) refreshTemplateRepositories(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, "SELECT id FROM template_repositories WHERE next_refresh_at<=now() AND refresh_until<=now() ORDER BY next_refresh_at LIMIT 2")
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.refreshTemplateRepository(ctx, id, "system"); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			var checked bool
			if e := s.DB.QueryRow(ctx, "SELECT checked_at IS NOT NULL AND last_error<>'' FROM template_repositories WHERE id=$1", id).Scan(&checked); errors.Is(e, pgx.ErrNoRows) {
				continue
			} else if e != nil {
				return e
			}
			if !checked {
				return err
			}
		}
	}
	return nil
}

func templateRevision(t catalog.Template) ([]byte, string, error) {
	data, err := json.Marshal(t)
	return data, fmt.Sprintf("%x", sha256.Sum256(data)), err
}

func (s *Server) runTemplateRepositories(ctx context.Context) {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := s.refreshTemplateRepositories(ctx); err != nil && ctx.Err() == nil {
			slog.Error("template catalogue refresh unavailable")
		}
	}
}
