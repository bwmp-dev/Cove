package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type gitBranch struct {
	Ref string `json:"ref"`
}

type gitCommitStatus struct {
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description,omitempty"`
	TargetURL   string `json:"target_url,omitempty"`
}

type gitProvider interface {
	CloneToken(context.Context, int64, int64) (string, error)
	Repositories(context.Context, int64) ([]appRepository, error)
	BranchHead(context.Context, int64, int64, string) (string, error)
	TagHead(context.Context, int64, int64, string) (string, error)
	Branches(context.Context, int64, int64, string) ([]gitBranch, error)
	VerifyWebhook([]byte, []byte, http.Header) bool
	PullRequest(context.Context, []byte) (previewEvent, error)
	CommitStatus(context.Context, int64, int64, string, gitCommitStatus) error
}

type githubProvider struct{ server *Server }

func (s *Server) githubProvider() gitProvider { return githubProvider{server: s} }

func (p githubProvider) CloneToken(ctx context.Context, installation, repository int64) (string, error) {
	return p.server.installationToken(ctx, installation, repository)
}

func (p githubProvider) Repositories(ctx context.Context, installation int64) ([]appRepository, error) {
	token, err := p.CloneToken(ctx, installation, 0)
	if err != nil {
		return nil, errors.New("GitHub installation is unavailable; check its permissions or suspension")
	}
	all := []appRepository{}
	for page := 1; ; page++ {
		var result struct {
			Repositories []appRepository `json:"repositories"`
		}
		if err = p.server.appAPI(ctx, "GET", fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), token, nil, &result); err != nil {
			return nil, errors.New("could not list GitHub repositories")
		}
		for _, repo := range result.Repositories {
			repo.InstallationID = installation
			all = append(all, repo)
		}
		if len(result.Repositories) < 100 {
			return all, nil
		}
	}
}

func (p githubProvider) BranchHead(ctx context.Context, installation, repository int64, branch string) (string, error) {
	token, err := p.CloneToken(ctx, installation, repository)
	if err != nil {
		return "", err
	}
	segments := strings.Split(branch, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err = p.server.appAPI(ctx, "GET", fmt.Sprintf("/repositories/%d/git/ref/heads/%s", repository, strings.Join(segments, "/")), token, nil, &ref)
	var status *githubStatusError
	if errors.As(err, &status) && status.Status == 404 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if ref.Ref != "refs/heads/"+branch || !gitCommit.MatchString(ref.Object.SHA) {
		return "", errors.New("GitHub returned an unexpected branch")
	}
	return ref.Object.SHA, nil
}

func (p githubProvider) Branches(ctx context.Context, installation, repository int64, prefix string) ([]gitBranch, error) {
	token, err := p.CloneToken(ctx, installation, repository)
	if err != nil {
		return nil, err
	}
	segments := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	path := fmt.Sprintf("/repositories/%d/git/matching-refs/heads/%s", repository, strings.Join(segments, "/"))
	if strings.HasSuffix(prefix, "/") {
		path += "/"
	}
	var refs []gitBranch
	if err = p.server.appAPI(ctx, "GET", path, token, nil, &refs); err != nil {
		return nil, err
	}
	if len(refs) > 200 {
		refs = refs[:200]
	}
	return refs, nil
}

func (p githubProvider) VerifyWebhook(secret, body []byte, headers http.Header) bool {
	signature := headers.Get("X-Hub-Signature-256")
	decoded, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil || !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(decoded, mac.Sum(nil))
}

func (p githubProvider) PullRequest(_ context.Context, body []byte) (previewEvent, error) {
	var event previewEvent
	err := json.Unmarshal(body, &event)
	return event, err
}

func (p githubProvider) CommitStatus(ctx context.Context, installation, repository int64, revision string, status gitCommitStatus) error {
	if installation <= 0 || repository <= 0 || !gitCommit.MatchString(revision) || revision == strings.Repeat("0", 40) {
		return errors.New("invalid commit status target")
	}
	if status.State != "pending" && status.State != "success" && status.State != "failure" && status.State != "error" {
		return errors.New("invalid commit status state")
	}
	if status.Context == "" || len(status.Context) > 100 || len(status.Description) > 140 || strings.ContainsAny(status.Context+status.Description, "\r\n") {
		return errors.New("invalid commit status description or context")
	}
	if status.TargetURL != "" {
		target, err := url.Parse(status.TargetURL)
		if err != nil || target.Scheme != "https" || target.Host == "" || target.User != nil || len(status.TargetURL) > 2048 {
			return errors.New("invalid commit status URL")
		}
	}
	token, err := p.server.installationAccessToken(ctx, installation, repository, map[string]string{"statuses": "write"})
	if err != nil {
		return err
	}
	return p.server.appAPI(ctx, "POST", fmt.Sprintf("/repositories/%d/statuses/%s", repository, revision), token, status, nil)
}
