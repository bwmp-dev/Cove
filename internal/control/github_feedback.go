package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cove/internal/worker"
)

func (s *Server) githubReporting(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "PUT" {
		if !s.requireAccess(w, r, "workload", id, 3) || !s.requireAccess(w, r, "team", "", 3) || !s.requirePersistentResource(w, r, id) {
			return
		}
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if !decode(w, r, &in) {
			return
		}
		var installation, repository int64
		if err := s.DB.QueryRow(r.Context(), `SELECT g.installation_id,g.repository_id FROM github_sources g JOIN workloads w ON w.id=g.workload_id JOIN github_installation_teams i ON i.installation_id=g.installation_id AND i.team_id=w.team_id WHERE g.workload_id=$1`, id).Scan(&installation, &repository); err != nil {
			http.Error(w, "connect a GitHub repository shared with this team first", 409)
			return
		}
		if in.Enabled {
			if _, err := s.installationAccessToken(r.Context(), installation, repository, map[string]string{"statuses": "write", "deployments": "write", "pull_requests": "write"}); err != nil {
				http.Error(w, "GitHub App needs commit statuses, deployments and pull requests write permissions; approve the updated installation permissions before enabling reporting", 502)
				return
			}
		}
		_, err := s.DB.Exec(r.Context(), `INSERT INTO github_reporting(workload_id,enabled) VALUES($1,$2) ON CONFLICT(workload_id) DO UPDATE SET enabled=EXCLUDED.enabled,enabled_at=CASE WHEN EXCLUDED.enabled AND NOT github_reporting.enabled THEN now() ELSE github_reporting.enabled_at END`, id, in.Enabled)
		if err != nil {
			http.Error(w, "could not save GitHub reporting", 503)
			return
		}
	}
	var enabled bool
	if err := s.DB.QueryRow(r.Context(), `SELECT COALESCE((SELECT enabled FROM github_reporting WHERE workload_id=COALESCE((SELECT source_id FROM previews WHERE workload_id=$1),$1)),false)`, id).Scan(&enabled); err != nil {
		http.Error(w, "GitHub reporting unavailable", 503)
		return
	}
	rows, err := s.DB.Query(r.Context(), `SELECT f.operation_id,f.reported_state,f.error,f.next_attempt_at FROM github_operation_feedback f JOIN operations o ON o.id=f.operation_id WHERE o.workload_id=$1 ORDER BY o.created_at DESC LIMIT 10`, id)
	if err != nil {
		http.Error(w, "GitHub reporting unavailable", 503)
		return
	}
	defer rows.Close()
	reports := []map[string]any{}
	for rows.Next() {
		var operation, state, message string
		var next time.Time
		if err = rows.Scan(&operation, &state, &message, &next); err != nil {
			http.Error(w, "GitHub reporting unavailable", 503)
			return
		}
		reports = append(reports, map[string]any{"operation_id": operation, "reported_state": state, "error": message, "next_attempt_at": next})
	}
	if rows.Err() != nil {
		http.Error(w, "GitHub reporting unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"enabled": enabled, "reports": reports})
}

type githubFeedback struct {
	operation, workload, state, repository, revision, reported, environment string
	installation, repositoryID, deployment                                  int64
	number, attempts                                                        int
	payload                                                                 []byte
}

func (s *Server) dispatchGithubFeedback(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT o.id,o.workload_id,o.state,g.repository,g.installation_id,g.repository_id,o.payload,COALESCE(o.result->>'revision',''),COALESCE(p.number,0),COALESCE(f.reported_state,''),COALESCE(f.deployment_id,0),COALESCE(f.attempts,0),COALESCE(w.environment_id,w.project_id)
 FROM operations o JOIN workloads w ON w.id=o.workload_id JOIN github_sources g ON g.workload_id=w.id
 JOIN github_installation_teams i ON i.installation_id=g.installation_id AND i.team_id=w.team_id
 LEFT JOIN previews p ON p.workload_id=w.id JOIN github_reporting settings ON settings.workload_id=COALESCE(p.source_id,w.id) AND settings.enabled
 LEFT JOIN github_operation_feedback f ON f.operation_id=o.id
 WHERE o.action='deploy' AND o.created_at>=settings.enabled_at AND (f.operation_id IS NULL OR (f.reported_state<>o.state AND f.next_attempt_at<=now()))
 ORDER BY o.created_at LIMIT 4`)
	if err != nil {
		return err
	}
	items := []githubFeedback{}
	for rows.Next() {
		var f githubFeedback
		if err = rows.Scan(&f.operation, &f.workload, &f.state, &f.repository, &f.installation, &f.repositoryID, &f.payload, &f.revision, &f.number, &f.reported, &f.deployment, &f.attempts, &f.environment); err != nil {
			rows.Close()
			return err
		}
		items = append(items, f)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, f := range items {
		var request worker.Request
		raw, err := unseal(s.Key, f.payload)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &request); err != nil {
			return err
		}
		if request.Source == nil || request.Source.Repository != f.repository {
			continue
		}
		if request.Source.Revision != "" {
			f.revision = request.Source.Revision
		}
		if !gitCommit.MatchString(f.revision) || f.revision == strings.Repeat("0", 40) {
			continue
		}
		if _, err = s.DB.Exec(ctx, `INSERT INTO github_operation_feedback(operation_id,revision) VALUES($1,$2) ON CONFLICT DO NOTHING`, f.operation, f.revision); err != nil {
			return err
		}
		reportContext, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = s.publishGithubFeedback(reportContext, f, request.Routes)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			delay := time.Duration(1<<min(f.attempts, 8)) * time.Minute
			if _, err = s.DB.Exec(ctx, `UPDATE github_operation_feedback SET attempts=attempts+1,next_attempt_at=now()+$2*interval '1 second',error='GitHub reporting failed; check App permissions and provider availability' WHERE operation_id=$1`, f.operation, int(delay.Seconds())); err != nil {
				return err
			}
			continue
		}
		if _, err = s.DB.Exec(ctx, `UPDATE github_operation_feedback SET reported_state=$2,attempts=0,error='',next_attempt_at=now() WHERE operation_id=$1`, f.operation, f.state); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) publishGithubFeedback(ctx context.Context, f githubFeedback, routes []worker.Route) error {
	repository, err := url.Parse(f.repository)
	if err != nil || repository.Host != "github.com" {
		return errors.New("invalid GitHub repository")
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(repository.Path, ".git"), "/"), "/")
	if len(parts) != 2 {
		return errors.New("invalid GitHub repository")
	}
	path := "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
	status, deploymentState := "pending", "queued"
	switch f.state {
	case "running":
		deploymentState = "in_progress"
	case "succeeded":
		status, deploymentState = "success", "success"
	case "failed", "interrupted":
		status, deploymentState = "failure", "failure"
	case "cancelled":
		status, deploymentState = "error", "inactive"
	}
	operationURL := s.PublicURL + "/operations/" + url.PathEscape(f.operation)
	if err = s.githubProvider().CommitStatus(ctx, f.installation, f.repositoryID, f.revision, gitCommitStatus{State: status, Context: "cove/" + f.workload + "/" + f.operation, Description: "Deployment " + f.state, TargetURL: operationURL}); err != nil {
		return err
	}
	token, err := s.installationAccessToken(ctx, f.installation, f.repositoryID, map[string]string{"deployments": "write"})
	if err != nil {
		return err
	}
	environment := "cove/" + f.environment + "/" + f.workload
	var slug string
	if err = s.DB.QueryRow(ctx, `SELECT slug FROM github_app WHERE id=1`).Scan(&slug); err != nil {
		return err
	}
	if f.deployment == 0 {
		// Recover a remotely accepted create whose response was lost before local persistence.
		for page := 1; page <= 100; page++ {
			var deployments []struct {
				ID      int64 `json:"id"`
				Creator struct {
					Login string `json:"login"`
				} `json:"creator"`
				Payload struct {
					Operation string `json:"cove_operation"`
				} `json:"payload"`
			}
			if err = s.appAPI(ctx, "GET", fmt.Sprintf("%s/deployments?sha=%s&environment=%s&per_page=100&page=%d", path, f.revision, url.QueryEscape(environment), page), token, nil, &deployments); err != nil {
				return err
			}
			for _, d := range deployments {
				if d.Payload.Operation == f.operation && d.Creator.Login == slug+"[bot]" {
					f.deployment = d.ID
					break
				}
			}
			if f.deployment != 0 || len(deployments) < 100 {
				break
			}
			if page == 100 {
				return errors.New("deployment recovery listing exceeded limit")
			}
		}
		if f.deployment == 0 {
			var created struct {
				ID int64 `json:"id"`
			}
			input := map[string]any{"ref": f.revision, "auto_merge": false, "required_contexts": []string{}, "environment": environment, "transient_environment": f.number > 0, "production_environment": false, "payload": map[string]string{"cove_operation": f.operation}}
			if err = s.appAPI(ctx, "POST", path+"/deployments", token, input, &created); err != nil {
				return err
			}
			if created.ID <= 0 {
				return errors.New("invalid deployment response")
			}
			f.deployment = created.ID
		}
		if _, err = s.DB.Exec(ctx, `UPDATE github_operation_feedback SET deployment_id=$2 WHERE operation_id=$1`, f.operation, f.deployment); err != nil {
			return err
		}
	}
	previewURL := ""
	if len(routes) > 0 {
		previewURL = "https://" + routes[0].Domain
	}
	if err = s.appAPI(ctx, "POST", fmt.Sprintf("%s/deployments/%d/statuses", path, f.deployment), token, map[string]any{"state": deploymentState, "log_url": operationURL, "environment_url": previewURL, "auto_inactive": false}, nil); err != nil {
		return err
	}
	if f.number == 0 {
		return nil
	}
	var newer bool
	if err = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations current JOIN operations next ON next.workload_id=current.workload_id AND next.action='deploy' AND (next.created_at,next.id)>(current.created_at,current.id) WHERE current.id=$1)`, f.operation).Scan(&newer); err != nil {
		return err
	}
	if newer {
		return nil
	}

	token, err = s.installationAccessToken(ctx, f.installation, f.repositoryID, map[string]string{"pull_requests": "write"})
	if err != nil {
		return err
	}
	marker := "<!-- cove-preview:" + f.workload + " -->"
	body := marker + "\nCove preview: **" + f.state + "**\n\n[Deployment details](" + operationURL + ")"
	if previewURL != "" {
		body += "\n\n[Preview](" + previewURL + ")"
	}
	var commentID int64
	// Find only comments authored by this App, including after an ambiguous create reply.
	for page := 1; page <= 100; page++ {
		var comments []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err = s.appAPI(ctx, "GET", fmt.Sprintf("%s/issues/%d/comments?per_page=100&page=%d", path, f.number, page), token, nil, &comments); err != nil {
			return err
		}
		for _, c := range comments {
			if c.User.Login == slug+"[bot]" && strings.HasPrefix(c.Body, marker+"\n") {
				commentID = c.ID
				break
			}
		}
		if commentID != 0 || len(comments) < 100 {
			break
		}
		if page == 100 {
			return errors.New("comment recovery listing exceeded limit")
		}
	}
	if commentID != 0 {
		return s.appAPI(ctx, "PATCH", fmt.Sprintf("%s/issues/comments/%d", path, commentID), token, map[string]string{"body": body}, nil)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err = s.appAPI(ctx, "POST", fmt.Sprintf("%s/issues/%d/comments", path, f.number), token, map[string]string{"body": body}, &created); err != nil {
		return err
	}
	if created.ID <= 0 {
		return errors.New("invalid comment response")
	}
	return nil
}
