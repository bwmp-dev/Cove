package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type gitConnection struct {
	ID        int64  `json:"id"`
	Account   string `json:"account,omitempty"`
	AuthUser  string `json:"username,omitempty"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	URL       string `json:"url"`
	Workspace string `json:"workspace,omitempty"`
	Token     string `json:"token,omitempty"`
	Secret    string `json:"webhook_secret,omitempty"`
}

type connectedGitProvider struct {
	server     *Server
	connection gitConnection
}

func (p connectedGitProvider) api(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, p.connection.URL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+p.connection.Token)
	if p.connection.Kind == "bitbucket" && p.connection.AuthUser != "" {
		request.SetBasicAuth(p.connection.AuthUser, p.connection.Token)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Cove")
	request.Header.Set("Content-Type", "application/json")
	config, err := p.server.outboundConfig(ctx)
	if err != nil {
		return errors.New("Git provider outbound settings unavailable")
	}
	transport := publicWebhookTransport(config)
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if p.server.OAuthClient != nil && p.server.OAuthClient.Transport != nil {
		client.Transport = p.server.OAuthClient.Transport
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Git provider is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &githubStatusError{Status: response.StatusCode}
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 8*1024*1024)).Decode(output)
}

func (s *Server) connectedGitProvider(ctx context.Context, id int64) (connectedGitProvider, error) {
	p := connectedGitProvider{server: s}
	var raw []byte
	err := s.DB.QueryRow(ctx, `SELECT configuration FROM git_connections WHERE id=$1`, id).Scan(&raw)
	if err != nil {
		return p, err
	}
	plain, err := unseal(s.Key, raw)
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(plain, &p.connection)
	p.connection.ID = id
	return p, err
}

func (p connectedGitProvider) CloneToken(_ context.Context, installation, repository int64) (string, error) {
	if installation != p.connection.ID || repository <= 0 {
		return "", errors.New("invalid provider source")
	}
	return p.connection.Token, nil
}

func (p connectedGitProvider) repository(ctx context.Context, id int64) (string, string, error) {
	var external, name string
	err := p.server.DB.QueryRow(ctx, `SELECT external_id,full_name FROM git_repositories WHERE id=$1 AND connection_id=$2`, id, p.connection.ID).Scan(&external, &name)
	return external, name, err
}

func (p connectedGitProvider) Repositories(ctx context.Context, installation int64) ([]appRepository, error) {
	if installation != p.connection.ID {
		return nil, errors.New("invalid provider connection")
	}
	all := []appRepository{}
	for page := 1; page <= 100; page++ {
		type repository struct {
			ID       json.Number `json:"id"`
			UUID     string      `json:"uuid"`
			FullName string      `json:"full_name"`
			Path     string      `json:"path_with_namespace"`
			CloneURL string      `json:"clone_url"`
			HTTPURL  string      `json:"http_url_to_repo"`
			Default  string      `json:"default_branch"`
			Main     struct {
				Name string `json:"name"`
			} `json:"mainbranch"`
			Links struct {
				Clone []struct {
					Href string `json:"href"`
					Name string `json:"name"`
				} `json:"clone"`
			} `json:"links"`
		}
		var repositories []repository
		hasMore := false
		switch p.connection.Kind {
		case "gitlab":
			if err := p.api(ctx, "GET", fmt.Sprintf("/projects?membership=true&simple=true&per_page=100&page=%d", page), nil, &repositories); err != nil {
				return nil, err
			}
			hasMore = len(repositories) == 100
		case "gitea":
			if err := p.api(ctx, "GET", fmt.Sprintf("/user/repos?limit=100&page=%d", page), nil, &repositories); err != nil {
				return nil, err
			}
			hasMore = len(repositories) > 0
		case "bitbucket":
			var result struct {
				Values []repository `json:"values"`
				Next   string       `json:"next"`
			}
			if err := p.api(ctx, "GET", fmt.Sprintf("/repositories/%s?pagelen=100&page=%d", url.PathEscape(p.connection.Workspace), page), nil, &result); err != nil {
				return nil, err
			}
			repositories = result.Values
			hasMore = result.Next != ""
		default:
			return nil, errors.New("unsupported Git provider")
		}
		for _, repo := range repositories {
			external, name, clone, branch := repo.ID.String(), repo.FullName, repo.CloneURL, repo.Default
			if p.connection.Kind == "gitlab" {
				name, clone = repo.Path, repo.HTTPURL
			}
			if p.connection.Kind == "bitbucket" {
				external, branch = repo.UUID, repo.Main.Name
				for _, link := range repo.Links.Clone {
					if link.Name == "https" {
						clone = link.Href
					}
				}
			}
			cloneURL, err := url.Parse(clone)
			if err == nil && p.connection.Kind == "bitbucket" && cloneURL.User != nil {
				if _, hasPassword := cloneURL.User.Password(); hasPassword {
					return nil, errors.New("provider returned embedded credentials")
				}
				cloneURL.User = nil
				clone = cloneURL.String()
			}

			base, _ := url.Parse(p.connection.URL)
			host := base.Host
			if p.connection.Kind == "bitbucket" {
				host = "bitbucket.org"
			}
			if err != nil || cloneURL.Scheme != "https" || cloneURL.Host != host || cloneURL.User != nil || cloneURL.RawQuery != "" || cloneURL.Fragment != "" || external == "" || name == "" {
				return nil, errors.New("provider returned an unsafe repository")
			}
			var id int64
			if err = p.server.DB.QueryRow(ctx, `INSERT INTO git_repositories(connection_id,external_id,full_name,clone_url,default_branch) VALUES($1,$2,$3,$4,$5) ON CONFLICT(connection_id,external_id) DO UPDATE SET full_name=EXCLUDED.full_name,clone_url=EXCLUDED.clone_url,default_branch=EXCLUDED.default_branch RETURNING id`, p.connection.ID, external, name, clone, branch).Scan(&id); err != nil {
				return nil, err
			}
			all = append(all, appRepository{ID: id, FullName: name, CloneURL: clone, DefaultBranch: branch, InstallationID: p.connection.ID})
		}
		if !hasMore {
			return all, nil
		}
	}
	return nil, errors.New("repository listing exceeds 100 pages; use a more narrowly scoped connection")
}

func (p connectedGitProvider) repositoryPath(ctx context.Context, id int64) (string, error) {
	external, name, err := p.repository(ctx, id)
	if err != nil {
		return "", err
	}
	if p.connection.Kind == "gitlab" {
		return "/projects/" + url.PathEscape(external), nil
	}
	parts := strings.Split(name, "/")
	if len(parts) != 2 {
		return "", errors.New("invalid repository identity")
	}
	prefix := "/repos/"
	if p.connection.Kind == "bitbucket" {
		prefix = "/repositories/"
	}
	return prefix + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]), nil
}

func (p connectedGitProvider) BranchHead(ctx context.Context, installation, repository int64, branch string) (string, error) {
	if installation != p.connection.ID {
		return "", errors.New("invalid provider connection")
	}
	path, err := p.repositoryPath(ctx, repository)
	if err != nil {
		return "", err
	}
	suffix := "/branches/"
	switch p.connection.Kind {
	case "gitlab":
		suffix = "/repository/branches/"
	case "bitbucket":
		suffix = "/refs/branches/"
	}
	var result struct {
		Name   string `json:"name"`
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
		Target struct {
			Hash string `json:"hash"`
		} `json:"target"`
	}
	err = p.api(ctx, "GET", path+suffix+url.PathEscape(branch), nil, &result)
	var status *githubStatusError
	if errors.As(err, &status) && status.Status == 404 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	commit := result.Commit.ID
	if p.connection.Kind == "bitbucket" {
		commit = result.Target.Hash
	}
	if result.Name != branch || !gitCommit.MatchString(commit) || commit == strings.Repeat("0", 40) {
		return "", errors.New("provider returned unexpected branch head")
	}
	return commit, nil
}

func (p connectedGitProvider) Branches(ctx context.Context, installation, repository int64, prefix string) ([]gitBranch, error) {
	if installation != p.connection.ID {
		return nil, errors.New("invalid provider connection")
	}
	path, err := p.repositoryPath(ctx, repository)
	if err != nil {
		return nil, err
	}
	suffix := "/branches"
	switch p.connection.Kind {
	case "gitlab":
		suffix = "/repository/branches"
	case "bitbucket":
		suffix = "/refs/branches"
	}
	all := []gitBranch{}
	for page := 1; page <= 100; page++ {
		var branches []struct {
			Name string `json:"name"`
		}
		more := false
		query := fmt.Sprintf("?per_page=100&limit=100&page=%d", page)
		if p.connection.Kind == "bitbucket" {
			var result struct {
				Values []struct {
					Name string `json:"name"`
				} `json:"values"`
				Next string `json:"next"`
			}
			if err = p.api(ctx, "GET", path+suffix+fmt.Sprintf("?pagelen=100&page=%d", page), nil, &result); err != nil {
				return nil, err
			}
			branches, more = result.Values, result.Next != ""
		} else {
			if err = p.api(ctx, "GET", path+suffix+query, nil, &branches); err != nil {
				return nil, err
			}
			more = len(branches) == 100
			if p.connection.Kind == "gitea" {
				more = len(branches) > 0
			}
		}
		for _, branch := range branches {
			if strings.HasPrefix(branch.Name, prefix) && validPreviewBranch(branch.Name) {
				all = append(all, gitBranch{Ref: "refs/heads/" + branch.Name})
			}
		}
		if !more {
			return all, nil
		}
	}
	return nil, errors.New("branch listing exceeds 100 pages")
}

func (p connectedGitProvider) VerifyWebhook(secret, body []byte, headers http.Header) bool {
	if p.connection.Kind == "gitlab" {
		key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(string(secret), "whsec_"))
		if err != nil || len(key) < 32 {
			return false
		}
		timestamp, err := strconv.ParseInt(headers.Get("webhook-timestamp"), 10, 64)
		if err != nil || time.Since(time.Unix(timestamp, 0)).Abs() > 5*time.Minute {
			return false
		}
		id := headers.Get("webhook-id")
		if id == "" || len(id) > 200 {
			return false
		}
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(id + "." + headers.Get("webhook-timestamp") + "."))
		mac.Write(body)
		expected := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		for _, signature := range strings.Fields(headers.Get("webhook-signature")) {
			if hmac.Equal([]byte(signature), []byte(expected)) {
				return true
			}
		}
		return false
	}
	header := headers.Get("X-Gitea-Signature")
	if header == "" && p.connection.Kind == "gitea" {
		header = headers.Get("X-Forgejo-Signature")
	}
	if p.connection.Kind == "bitbucket" {
		header = headers.Get("X-Hub-Signature")
		if !strings.HasPrefix(header, "sha256=") {
			return false
		}
		header = strings.TrimPrefix(header, "sha256=")
	}
	signature, err := hex.DecodeString(header)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(signature, mac.Sum(nil))
}

func (p connectedGitProvider) CommitStatus(ctx context.Context, installation, repository int64, revision string, status gitCommitStatus) error {
	if installation != p.connection.ID || !gitCommit.MatchString(revision) || revision == strings.Repeat("0", 40) {
		return errors.New("invalid status target")
	}
	if status.State != "pending" && status.State != "success" && status.State != "failure" && status.State != "error" {
		return errors.New("invalid status state")
	}
	path, err := p.repositoryPath(ctx, repository)
	if err != nil {
		return err
	}
	switch p.connection.Kind {
	case "gitlab":
		state := status.State
		if state == "error" {
			state = "failed"
		}
		if state == "failure" {
			state = "failed"
		}
		return p.api(ctx, "POST", path+"/statuses/"+revision, map[string]string{"state": state, "name": status.Context, "target_url": status.TargetURL, "description": status.Description}, nil)
	case "gitea":
		return p.api(ctx, "POST", path+"/statuses/"+revision, status, nil)
	case "bitbucket":
		state := map[string]string{"pending": "INPROGRESS", "success": "SUCCESSFUL", "failure": "FAILED", "error": "STOPPED"}[status.State]
		return p.api(ctx, "POST", path+"/commit/"+revision+"/statuses/build", map[string]string{"state": state, "key": status.Context, "url": status.TargetURL, "description": status.Description}, nil)
	}
	return errors.New("unsupported provider")
}

func (p connectedGitProvider) PullRequest(ctx context.Context, body []byte) (previewEvent, error) {
	var event previewEvent
	if p.connection.Kind == "gitea" {
		err := json.Unmarshal(body, &event)
		event.Installation.ID = p.connection.ID
		if event.Action == "synchronized" {
			event.Action = "synchronize"
		}
		if event.Action == "deleted" {
			event.Action = "closed"
			event.PullRequest.State = "closed"
		}
		if err != nil {
			return event, err
		}
		return p.mapPullRequest(ctx, event)
	}
	if p.connection.Kind == "gitlab" {
		var input struct {
			Project struct {
				ID int64 `json:"id"`
			} `json:"project"`
			Attributes struct {
				Number       int    `json:"iid"`
				Action       string `json:"action"`
				State        string `json:"state"`
				Source       int64  `json:"source_project_id"`
				Target       int64  `json:"target_project_id"`
				SourceBranch string `json:"source_branch"`
				TargetBranch string `json:"target_branch"`
				Updated      string `json:"updated_at"`
				Commit       struct {
					ID string `json:"id"`
				} `json:"last_commit"`
			} `json:"object_attributes"`
		}
		if err := json.Unmarshal(body, &input); err != nil {
			return event, err
		}
		a := input.Attributes
		event.Action = map[string]string{"open": "opened", "reopen": "reopened", "update": "synchronize", "close": "closed", "merge": "closed"}[a.Action]
		event.Number = a.Number
		event.Repository.ID = input.Project.ID
		event.PullRequest.Head.Repo.ID = a.Source
		event.PullRequest.Base.Repo.ID = a.Target
		event.PullRequest.Head.Ref = a.SourceBranch
		event.PullRequest.Base.Ref = a.TargetBranch
		event.PullRequest.Head.SHA = a.Commit.ID
		event.PullRequest.State = "closed"
		if a.State == "opened" {
			event.PullRequest.State = "open"
		}
		updated, err := time.Parse(time.RFC3339Nano, a.Updated)
		if err != nil {
			updated, err = time.Parse("2006-01-02 15:04:05 UTC", a.Updated)
		}
		if err != nil {
			return event, errors.New("invalid merge request timestamp")
		}
		event.PullRequest.UpdatedAt = updated
		event.Installation.ID = p.connection.ID
		return p.mapPullRequest(ctx, event)
	}
	var input struct {
		Repository struct {
			UUID string `json:"uuid"`
		} `json:"repository"`
		Request struct {
			ID      int       `json:"id"`
			State   string    `json:"state"`
			Updated time.Time `json:"updated_on"`
			Source  struct {
				Repository struct {
					UUID string `json:"uuid"`
				} `json:"repository"`
				Branch struct {
					Name string `json:"name"`
				} `json:"branch"`
				Commit struct {
					Hash string `json:"hash"`
				} `json:"commit"`
			} `json:"source"`
			Destination struct {
				Repository struct {
					UUID string `json:"uuid"`
				} `json:"repository"`
				Branch struct {
					Name string `json:"name"`
				} `json:"branch"`
			} `json:"destination"`
		} `json:"pullrequest"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		return event, err
	}
	request := input.Request
	if input.Repository.UUID == "" || request.Destination.Repository.UUID != input.Repository.UUID {
		return event, errors.New("unexpected pull request repository")
	}
	var id int64
	if err := p.server.DB.QueryRow(ctx, `SELECT id FROM git_repositories WHERE connection_id=$1 AND external_id=$2`, p.connection.ID, input.Repository.UUID).Scan(&id); err != nil {
		return event, err
	}
	event.Installation.ID = p.connection.ID
	event.Repository.ID = id
	event.Number = request.ID
	event.Action = "closed"
	event.PullRequest.State = "closed"
	if request.State == "OPEN" {
		event.Action = "synchronize"
		event.PullRequest.State = "open"
	}
	event.PullRequest.UpdatedAt = request.Updated
	event.PullRequest.Base.Repo.ID = id
	if request.Source.Repository.UUID == input.Repository.UUID {
		event.PullRequest.Head.Repo.ID = id
	}
	event.PullRequest.Head.Ref = request.Source.Branch.Name
	event.PullRequest.Base.Ref = request.Destination.Branch.Name
	event.PullRequest.Head.SHA = request.Source.Commit.Hash
	return event, nil
}

func (p connectedGitProvider) mapPullRequest(ctx context.Context, event previewEvent) (previewEvent, error) {
	var id int64
	if err := p.server.DB.QueryRow(ctx, `SELECT id FROM git_repositories WHERE connection_id=$1 AND external_id=$2`, p.connection.ID, strconv.FormatInt(event.Repository.ID, 10)).Scan(&id); err != nil {
		return event, err
	}
	if event.PullRequest.Head.Repo.ID == event.Repository.ID {
		event.PullRequest.Head.Repo.ID = id
	} else {
		event.PullRequest.Head.Repo.ID = 0
	}
	if event.PullRequest.Base.Repo.ID == event.Repository.ID {
		event.PullRequest.Base.Repo.ID = id
	} else {
		event.PullRequest.Base.Repo.ID = 0
	}
	event.Repository.ID = id
	return event, nil
}
