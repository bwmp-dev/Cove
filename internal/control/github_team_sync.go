package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type githubTeamConfiguration struct {
	Name          string `json:"name"`
	Mode          string `json:"mode"`
	Account       string `json:"account"`
	Installation  int64  `json:"installation_id,omitempty"`
	OAuthUser     string `json:"oauth_user_id,omitempty"`
	OAuthGitHubID string `json:"oauth_github_id,omitempty"`
	OAuthToken    string `json:"oauth_token,omitempty"`
}

type githubTeamSource struct {
	ID         string                  `json:"id"`
	Enabled    bool                    `json:"enabled"`
	Version    int64                   `json:"version"`
	CheckedAt  *time.Time              `json:"checked_at"`
	AppliedAt  *time.Time              `json:"applied_at"`
	NextCheck  time.Time               `json:"next_check_at"`
	Error      string                  `json:"last_error"`
	Configured bool                    `json:"credential_configured"`
	Config     githubTeamConfiguration `json:"configuration"`
	Mappings   []directoryMapping      `json:"mappings"`
}

func loadGithubTeamSource(ctx context.Context, tx pgx.Tx, key []byte, id string) (githubTeamSource, error) {
	source := githubTeamSource{ID: id}
	var raw []byte
	err := tx.QueryRow(ctx, "SELECT configuration,enabled,version,checked_at,applied_at,next_check_at,last_error FROM github_team_sync_sources WHERE id=$1", id).Scan(&raw, &source.Enabled, &source.Version, &source.CheckedAt, &source.AppliedAt, &source.NextCheck, &source.Error)
	if err != nil {
		return source, err
	}
	raw, err = unseal(key, raw)
	if err == nil {
		err = json.Unmarshal(raw, &source.Config)
	}
	if err == nil {
		source.Mappings, err = loadDirectoryMappings(ctx, tx, "github", id)
	}
	source.Configured = source.Config.Mode == "app" || source.Config.OAuthToken != ""
	return source, err
}

func (s *Server) githubTeamSources(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		tx, err := s.DB.Begin(r.Context())
		if err != nil {
			http.Error(w, "directory settings unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		rows, err := tx.Query(r.Context(), "SELECT id FROM github_team_sync_sources ORDER BY id")
		if err != nil {
			http.Error(w, "directory settings unavailable", 503)
			return
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err != nil || rows.Err() != nil {
			http.Error(w, "directory settings unavailable", 503)
			return
		}
		out := []githubTeamSource{}
		for _, id := range ids {
			source, e := loadGithubTeamSource(r.Context(), tx, s.Key, id)
			if e != nil {
				http.Error(w, "directory settings unavailable", 503)
				return
			}
			source.Config.OAuthToken = ""
			out = append(out, source)
		}
		respond(w, 200, map[string]any{"sources": out})
		return
	}
	tx, err := s.identityTransaction(r.Context())
	if err != nil {
		http.Error(w, "directory settings unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	id := r.PathValue("source")
	if r.Method == "DELETE" {
		var current int64
		if err = tx.QueryRow(r.Context(), "SELECT version FROM github_team_sync_sources WHERE id=$1 FOR UPDATE", id).Scan(&current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.NotFound(w, r)
			} else {
				http.Error(w, "directory settings unavailable", 503)
			}
			return
		}
		var in struct {
			Version int64 `json:"version"`
			Confirm bool  `json:"confirm"`
		}
		if !decode(w, r, &in) {
			return
		}
		if !in.Confirm || in.Version != current {
			http.Error(w, "review and confirm the current source before removing its managed access", 409)
			return
		}
		if _, err = syncDirectoryMemberships(r.Context(), tx, "github", id, "", nil, actor(r).ID); err == nil {
			_, err = tx.Exec(r.Context(), "DELETE FROM identity_group_mappings WHERE source_kind='github' AND source_id=$1", id)
		}
		if err == nil {
			_, err = tx.Exec(r.Context(), "DELETE FROM github_team_sync_sources WHERE id=$1", id)
		}
	} else {
		var in struct {
			Name         string             `json:"name"`
			Mode         string             `json:"mode"`
			Account      string             `json:"account"`
			Installation int64              `json:"installation_id"`
			Enabled      bool               `json:"enabled"`
			Version      int64              `json:"version"`
			Mappings     []directoryMapping `json:"mappings"`
		}
		if !decode(w, r, &in) {
			return
		}
		in.Name, in.Account = strings.TrimSpace(in.Name), strings.ToLower(strings.TrimSpace(in.Account))
		if in.Name == "" || len(in.Name) > 120 || !githubAccountName.MatchString(in.Account) || (in.Mode != "app" && in.Mode != "oauth") {
			http.Error(w, "name, GitHub organization and app or oauth connection required", 400)
			return
		}
		for _, mapping := range in.Mappings {
			if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,99}$`).MatchString(mapping.Group) {
				http.Error(w, "GitHub team slugs must use lowercase letters, digits, underscores or hyphens", 400)
				return
			}
		}
		config := githubTeamConfiguration{Name: in.Name, Mode: in.Mode, Account: in.Account}
		var previous githubTeamSource
		if id != "" {
			previous, err = loadGithubTeamSource(r.Context(), tx, s.Key, id)
			if err != nil || previous.Version != in.Version {
				http.Error(w, "source changed or was removed; reload its settings", 409)
				return
			}
		} else {
			var count int
			if err = tx.QueryRow(r.Context(), "SELECT count(*) FROM github_team_sync_sources").Scan(&count); err != nil {
				http.Error(w, "directory settings unavailable", 503)
				return
			}
			if count >= 8 {
				http.Error(w, "at most eight GitHub directory sources are allowed", 409)
				return
			}
			id = resourceID()
		}
		if in.Mode == "app" {
			var account string
			if err = tx.QueryRow(r.Context(), "SELECT account FROM github_installations WHERE id=$1", in.Installation).Scan(&account); err != nil || !strings.EqualFold(account, in.Account) {
				http.Error(w, "select an existing installation for this organization", 400)
				return
			}
			config.Installation = in.Installation
		} else {
			config.OAuthUser = actor(r).ID
			if err = tx.QueryRow(r.Context(), "SELECT COALESCE(github_id,'') FROM users WHERE id=$1 AND is_admin AND NOT disabled", config.OAuthUser).Scan(&config.OAuthGitHubID); err != nil || config.OAuthGitHubID == "" {
				http.Error(w, "link the administrator's GitHub account before configuring OAuth team sync", 409)
				return
			}
			if previous.Config.Mode == "oauth" && previous.Config.OAuthUser == config.OAuthUser && previous.Config.OAuthGitHubID == config.OAuthGitHubID && previous.Config.Account == config.Account {
				config.OAuthToken = previous.Config.OAuthToken
			}
		}
		if err = saveDirectoryMappings(r.Context(), tx, "github", id, in.Mappings); err != nil {
			http.Error(w, "valid, unique group, team and role mappings required", 400)
			return
		}
		raw, e := json.Marshal(config)
		if e == nil {
			raw, e = seal(s.Key, raw)
		}
		if e != nil {
			http.Error(w, "directory settings unavailable", 503)
			return
		}
		if previous.ID == "" {
			_, err = tx.Exec(r.Context(), "INSERT INTO github_team_sync_sources(id,configuration,enabled) VALUES($1,$2,$3)", id, raw, in.Enabled)
		} else {
			_, err = tx.Exec(r.Context(), "UPDATE github_team_sync_sources SET configuration=$2,enabled=$3,version=version+1,next_check_at=now() WHERE id=$1", id, raw, in.Enabled)
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status) VALUES($1,$2,$3,200)", actor(r).ID, "GitHub team sync source "+strings.ToLower(r.Method), id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save directory settings", 503)
		return
	}
	respond(w, 200, map[string]string{"id": id})
}

func (s *Server) githubTeamPreview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version int64  `json:"version"`
		Hash    string `json:"roster_hash"`
		Confirm bool   `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	apply := strings.HasSuffix(r.URL.Path, "/reconcile")
	if in.Version <= 0 || (apply && (!in.Confirm || len(in.Hash) != 64)) {
		http.Error(w, "preview the current source and confirm its roster before applying", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	report, err := s.reconcileGithubTeamSource(ctx, r.PathValue("source"), actor(r).ID, "", apply, in.Version, in.Hash)
	if err != nil {
		status, message := 503, "directory reconciliation unavailable; existing access was retained"
		if errors.Is(err, errGithubDirectoryConflict) {
			status, message = 409, "source or roster changed, or a refresh is active; preview again"
		} else if errors.Is(err, errGithubDirectoryProvider) {
			status, message = 502, "GitHub connection or Members permission unavailable; existing access was retained"
		}
		http.Error(w, message, status)
		return
	}
	respond(w, 200, report)
}
