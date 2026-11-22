package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
)

type gitSourceTrigger struct {
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
}

func triggerMatches(trigger gitSourceTrigger, event, ref, branch string) bool {
	if trigger.Kind == "" || trigger.Kind == "branch" {
		return event == "push" && ref == "refs/heads/"+branch
	}
	expected := "push"
	if trigger.Kind == "releases" {
		expected = "release"
	}
	if event != expected || !strings.HasPrefix(ref, "refs/tags/") {
		return false
	}
	matches, err := path.Match(trigger.Pattern, strings.TrimPrefix(ref, "refs/tags/"))
	return err == nil && matches
}

func (s *Server) gitSourceTrigger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "PUT" {
		if !s.requireAccess(w, r, "workload", id, 3) || !s.requirePersistentResource(w, r, id) {
			return
		}
		var in gitSourceTrigger
		if !decode(w, r, &in) {
			return
		}
		if in.Kind != "branch" && in.Kind != "tags" && in.Kind != "releases" {
			http.Error(w, "choose branch, tags or releases", 400)
			return
		}
		if in.Kind == "branch" {
			in.Pattern = ""
		} else {
			if len(in.Pattern) == 0 || len(in.Pattern) > 200 || strings.ContainsAny(in.Pattern, "\r\n\x00") {
				http.Error(w, "tag pattern required", 400)
				return
			}
			if _, err := path.Match(in.Pattern, "tag"); err != nil {
				http.Error(w, "invalid tag pattern", 400)
				return
			}
		}
		tx, err := s.DB.Begin(r.Context())
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		var active, managed bool
		var connection *int64
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=w.id AND state IN ('queued','running')),(EXISTS(SELECT 1 FROM github_sources WHERE workload_id=w.id) OR EXISTS(SELECT 1 FROM git_sources WHERE workload_id=w.id)),(SELECT connection_id FROM git_sources WHERE workload_id=w.id) FROM workloads w WHERE w.id=$1 FOR UPDATE`, id).Scan(&active, &managed, &connection); err != nil {
			http.Error(w, "workload unavailable", 503)
			return
		}
		if active || !managed {
			http.Error(w, "connect a managed Git provider and wait for active operations before changing the trigger", 409)
			return
		}
		if in.Kind == "releases" && connection != nil {
			provider, err := s.connectedGitProvider(r.Context(), *connection)
			if err != nil {
				http.Error(w, "connection unavailable", 503)
				return
			}
			if provider.connection.Kind == "bitbucket" {
				http.Error(w, "Bitbucket Cloud has no native release event; use a tag pattern", 409)
				return
			}
		}
		if _, err = tx.Exec(r.Context(), `INSERT INTO git_source_triggers(workload_id,kind,pattern) VALUES($1,$2,$3) ON CONFLICT(workload_id) DO UPDATE SET kind=EXCLUDED.kind,pattern=EXCLUDED.pattern`, id, in.Kind, in.Pattern); err != nil {
			http.Error(w, "could not save trigger", 503)
			return
		}
		if _, err = tx.Exec(r.Context(), `DELETE FROM webhook_deliveries WHERE workload_id=$1 AND operation_id IS NULL`, id); err != nil {
			http.Error(w, "could not clear old source deliveries", 503)
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			http.Error(w, "could not save trigger", 503)
			return
		}
	}
	var trigger gitSourceTrigger
	if err := s.DB.QueryRow(r.Context(), `SELECT COALESCE((SELECT kind FROM git_source_triggers WHERE workload_id=$1),'branch'),COALESCE((SELECT pattern FROM git_source_triggers WHERE workload_id=$1),'')`, id).Scan(&trigger.Kind, &trigger.Pattern); err != nil {
		http.Error(w, "trigger unavailable", 503)
		return
	}
	respond(w, 200, trigger)
}

func (p githubProvider) TagHead(ctx context.Context, installation, repository int64, tag string) (string, error) {
	if !validPreviewBranch(tag) {
		return "", errors.New("invalid tag")
	}
	token, err := p.CloneToken(ctx, installation, repository)
	if err != nil {
		return "", err
	}
	var result struct {
		SHA string `json:"sha"`
	}
	if err = p.server.appAPI(ctx, "GET", fmt.Sprintf("/repositories/%d/commits/%s", repository, url.PathEscape(tag)), token, nil, &result); err != nil {
		return "", err
	}
	if !gitCommit.MatchString(result.SHA) || result.SHA == strings.Repeat("0", 40) {
		return "", errors.New("provider returned invalid tag commit")
	}
	return result.SHA, nil
}

func (p connectedGitProvider) TagHead(ctx context.Context, installation, repository int64, tag string) (string, error) {
	if installation != p.connection.ID || !validPreviewBranch(tag) {
		return "", errors.New("invalid tag")
	}
	base, err := p.repositoryPath(ctx, repository)
	if err != nil {
		return "", err
	}
	suffix := "/tags/"
	switch p.connection.Kind {
	case "gitlab":
		suffix = "/repository/tags/"
	case "bitbucket":
		suffix = "/refs/tags/"
	}
	var result struct {
		Name   string `json:"name"`
		Commit struct {
			ID  string `json:"id"`
			SHA string `json:"sha"`
		} `json:"commit"`
		Target struct {
			Hash string `json:"hash"`
		} `json:"target"`
	}
	if err = p.api(ctx, "GET", base+suffix+url.PathEscape(tag), nil, &result); err != nil {
		return "", err
	}
	commit := result.Commit.ID
	if p.connection.Kind == "gitea" {
		commit = result.Commit.SHA
	}
	if p.connection.Kind == "bitbucket" {
		commit = result.Target.Hash
	}
	if result.Name != tag || !gitCommit.MatchString(commit) || commit == strings.Repeat("0", 40) {
		return "", errors.New("provider returned invalid tag commit")
	}
	return commit, nil
}

func (s *Server) githubReleaseWebhook(w http.ResponseWriter, r *http.Request, body []byte) {
	var release struct {
		Action  string `json:"action"`
		Release struct {
			Tag   string `json:"tag_name"`
			Draft bool   `json:"draft"`
		} `json:"release"`
		Repository   appRepository `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if json.Unmarshal(body, &release) != nil {
		http.Error(w, "invalid release", 400)
		return
	}
	if release.Action != "published" || release.Release.Draft {
		respond(w, 200, map[string]bool{"ignored": true})
		return
	}
	revision, err := s.githubProvider().TagHead(r.Context(), release.Installation.ID, release.Repository.ID, release.Release.Tag)
	if err != nil {
		http.Error(w, "release tag could not be resolved", 502)
		return
	}
	normalized, err := json.Marshal(map[string]any{"ref": "refs/tags/" + release.Release.Tag, "after": revision, "repository": release.Repository, "installation": release.Installation})
	if err != nil {
		http.Error(w, "invalid release", 400)
		return
	}
	s.githubAppPush(w, r, normalized, "release")
}
