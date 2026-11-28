package control

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

func (s *Server) prepareConnectedGitDeployment(ctx context.Context, request *worker.Request) (bool, error) {
	var connection, repository int64
	var clone string
	err := s.DB.QueryRow(ctx, `SELECT g.connection_id,g.repository_id,r.clone_url FROM git_sources g JOIN workloads w ON w.id=g.workload_id JOIN git_connections c ON c.id=g.connection_id AND c.team_id=w.team_id JOIN git_repositories r ON r.id=g.repository_id WHERE g.workload_id=$1`, request.Workload).Scan(&connection, &repository, &clone)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if request.Source.Repository != clone {
		return true, errors.New("provider repository changed; reconnect the source")
	}
	if request.Source.Token != "" {
		return true, nil
	}
	provider, err := s.connectedGitProvider(ctx, connection)
	if err != nil {
		return true, err
	}
	if request.Source.Revision == "" {
		revision, err := provider.BranchHead(ctx, connection, repository, request.Source.Branch)
		if err != nil {
			return true, err
		}
		if revision == "" {
			return true, errors.New("provider branch is unavailable")
		}
		request.Source.Revision = revision
	}
	token, err := provider.CloneToken(ctx, connection, repository)
	if err != nil {
		return true, err
	}
	request.Source.Token = token
	request.Source.Username = provider.connection.Account
	switch provider.connection.Kind {
	case "gitlab":
		request.Source.Username = "oauth2"
	case "bitbucket":
		request.Source.Username = "x-token-auth"
		if provider.connection.AuthUser != "" {
			request.Source.Username = "x-bitbucket-api-token-auth"
		}
	}
	payload, err := s.sealPreview(request)
	if err != nil {
		return true, err
	}
	_, err = s.DB.Exec(ctx, `UPDATE operations SET payload=$2 WHERE id=$1`, request.ID, payload)
	return true, err
}

func (s *Server) dispatchConnectedGitFeedback(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT o.id,o.workload_id,feedback.state,o.payload,COALESCE(o.result->>'revision',''),g.connection_id,g.repository_id,r.clone_url,COALESCE(p.number,0),COALESCE(f.attempts,0) FROM operations o JOIN cove_deployment_feedback_states feedback ON feedback.id=o.id JOIN workloads w ON w.id=o.workload_id JOIN git_sources g ON g.workload_id=w.id JOIN git_connections c ON c.id=g.connection_id AND c.team_id=w.team_id JOIN git_repositories r ON r.id=g.repository_id LEFT JOIN previews p ON p.workload_id=w.id JOIN git_sources settings ON settings.workload_id=COALESCE(p.source_id,w.id) AND settings.reporting LEFT JOIN git_provider_feedback f ON f.operation_id=o.id WHERE o.action='deploy' AND o.created_at>=settings.connected_at AND (f.operation_id IS NULL OR (f.reported_state<>feedback.state AND f.next_attempt_at<=now())) ORDER BY o.created_at LIMIT 4`)
	if err != nil {
		return err
	}
	type entry struct {
		operation, workload, state, revision, clone string
		payload                                     []byte
		connection, repository                      int64
		number, attempts                            int
	}
	entries := []entry{}
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.operation, &e.workload, &e.state, &e.payload, &e.revision, &e.connection, &e.repository, &e.clone, &e.number, &e.attempts); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, e)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, e := range entries {
		var request worker.Request
		if err = s.unsealJSON(e.payload, &request); err != nil {
			return err
		}
		if request.Source == nil || request.Source.Repository != e.clone {
			continue
		}
		if request.Source.Revision != "" {
			e.revision = request.Source.Revision
		}
		if !gitCommit.MatchString(e.revision) {
			continue
		}
		provider, err := s.connectedGitProvider(ctx, e.connection)
		if err != nil {
			return err
		}
		state := "pending"
		switch e.state {
		case "succeeded":
			state = "success"
		case "failed", "interrupted":
			state = "failure"
		case "cancelled":
			state = "error"
		}
		reportCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = provider.CommitStatus(reportCtx, e.connection, e.repository, e.revision, gitCommitStatus{State: state, Context: "cove/" + e.operation, Description: "Deployment " + e.state, TargetURL: s.PublicURL + "/operations/" + e.operation})
		if err == nil && e.number > 0 {
			var newer bool
			err = s.DB.QueryRow(reportCtx, `SELECT EXISTS(SELECT 1 FROM operations current JOIN operations next ON next.workload_id=current.workload_id AND next.action='deploy' AND (next.created_at,next.id)>(current.created_at,current.id) WHERE current.id=$1)`, e.operation).Scan(&newer)
			if err == nil && !newer {
				previewURL := ""
				if len(request.Routes) > 0 {
					previewURL = "https://" + request.Routes[0].Domain
				}
				err = provider.previewComment(reportCtx, e.repository, e.number, e.workload, e.state, s.PublicURL+"/operations/"+e.operation, previewURL)
			}
		}
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			delay := time.Duration(1<<min(e.attempts, 8)) * time.Minute
			if _, err = s.DB.Exec(ctx, `INSERT INTO git_provider_feedback(operation_id,attempts,next_attempt_at,error) SELECT id,1,now()+$2*interval '1 second','Provider reporting failed; check connection permissions and availability' FROM operations WHERE id=$1 FOR KEY SHARE ON CONFLICT(operation_id) DO UPDATE SET attempts=git_provider_feedback.attempts+1,next_attempt_at=EXCLUDED.next_attempt_at,error=EXCLUDED.error`, e.operation, int(delay.Seconds())); err != nil {
				return err
			}
			continue
		}
		if _, err = s.DB.Exec(ctx, `INSERT INTO git_provider_feedback(operation_id,reported_state) SELECT id,$2 FROM operations WHERE id=$1 FOR KEY SHARE ON CONFLICT(operation_id) DO UPDATE SET reported_state=EXCLUDED.reported_state,attempts=0,error='',next_attempt_at=now()`, e.operation, e.state); err != nil {
			return err
		}
	}
	return nil
}

func (p connectedGitProvider) previewComment(ctx context.Context, repository int64, number int, workload, state, operationURL, previewURL string) error {
	path, err := p.repositoryPath(ctx, repository)
	if err != nil {
		return err
	}
	marker := "<!-- cove-preview:" + workload + " -->"
	body := marker + "\nCove preview: **" + state + "**\n\n[Deployment details](" + operationURL + ")"
	if previewURL != "" {
		body += "\n\n[Preview](" + previewURL + ")"
	}
	commentsPath := fmt.Sprintf("%s/issues/%d/comments", path, number)
	switch p.connection.Kind {
	case "gitlab":
		commentsPath = fmt.Sprintf("%s/merge_requests/%d/notes", path, number)
	case "bitbucket":
		commentsPath = fmt.Sprintf("%s/pullrequests/%d/comments", path, number)
	}
	var id int64
	type comment struct {
		ID      int64  `json:"id"`
		Body    string `json:"body"`
		Content struct {
			Raw string `json:"raw"`
		} `json:"content"`
		Author struct {
			Username string `json:"username"`
		} `json:"author"`
		User struct {
			Login string `json:"login"`
			UUID  string `json:"uuid"`
		} `json:"user"`
	}
	for page := 1; page <= 100; page++ {
		var comments []comment
		more := false
		if p.connection.Kind == "bitbucket" {
			var result struct {
				Values []comment `json:"values"`
				Next   string    `json:"next"`
			}
			if err = p.api(ctx, "GET", fmt.Sprintf("%s?pagelen=100&page=%d", commentsPath, page), nil, &result); err != nil {
				return err
			}
			comments, more = result.Values, result.Next != ""
		} else {
			if err = p.api(ctx, "GET", fmt.Sprintf("%s?per_page=100&limit=100&page=%d", commentsPath, page), nil, &comments); err != nil {
				return err
			}
			more = len(comments) == 100
			if p.connection.Kind == "gitea" {
				more = len(comments) > 0
			}
		}
		for _, c := range comments {
			author, text := c.User.Login, c.Body
			switch p.connection.Kind {
			case "gitlab":
				author = c.Author.Username
			case "bitbucket":
				author, text = c.User.UUID, c.Content.Raw
			}
			if author == p.connection.Account && strings.HasPrefix(text, marker+"\n") {
				id = c.ID
				break
			}
		}
		if id != 0 || !more {
			break
		}
		if page == 100 {
			return errors.New("preview comment recovery exceeds 100 pages")
		}
	}
	method, target, input := "POST", commentsPath, any(map[string]string{"body": body})
	if p.connection.Kind == "bitbucket" {
		input = map[string]any{"content": map[string]string{"raw": body}}
	}
	if id != 0 {
		switch p.connection.Kind {
		case "gitea":
			method, target = "PATCH", fmt.Sprintf("%s/issues/comments/%d", path, id)
		default:
			method, target = "PUT", commentsPath+"/"+url.PathEscape(fmt.Sprint(id))
		}
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err = p.api(ctx, method, target, input, &result); err != nil {
		return err
	}
	if result.ID <= 0 {
		return errors.New("invalid preview comment response")
	}
	return nil
}

func (s *Server) providerFeedbackReports(ctx context.Context, workload string) ([]map[string]any, error) {
	rows, err := s.DB.Query(ctx, `SELECT f.operation_id,f.reported_state,f.error,f.next_attempt_at FROM git_provider_feedback f JOIN operations o ON o.id=f.operation_id WHERE o.workload_id=$1 ORDER BY o.created_at DESC LIMIT 10`, workload)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var operation, state, message string
		var next time.Time
		if err = rows.Scan(&operation, &state, &message, &next); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"operation_id": operation, "reported_state": state, "error": message, "next_attempt_at": next})
	}
	return out, rows.Err()
}

var _ gitProvider = connectedGitProvider{}
