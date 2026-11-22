package worker

import (
	"context"
	"cove/internal/outbound"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type ReleaseComparisonRequest struct {
	Source                *GitSource `json:"source,omitempty"`
	DifferentRepositories bool       `json:"different_repositories,omitempty"`
	Workload              string     `json:"workload"`
	From                  string     `json:"from"`
	To                    string     `json:"to"`
}
type ImageChange struct {
	Service string `json:"service"`
	From    string `json:"from"`
	To      string `json:"to"`
	Changed bool   `json:"changed"`
}
type ReleaseCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
}
type ReleaseComparison struct {
	EnvironmentChanges map[string]bool `json:"environment_changes,omitempty"`
	FromRevision       string          `json:"from_revision"`
	ToRevision         string          `json:"to_revision"`
	Images             []ImageChange   `json:"images"`
	Commits            []ReleaseCommit `json:"commits"`
	CommitsComplete    bool            `json:"commits_complete"`
	CommitMessage      string          `json:"commit_message"`
}

func (e *Engine) releaseComparisonState(workload, id string) (deployedState, map[string]string, error) {
	var state deployedState
	j, err := e.read(id)
	if err != nil || j.Operation.Workload != workload || j.Operation.State != "succeeded" {
		return state, nil, errors.New("select successful retained releases of this workload")
	}
	dir := filepath.Join(e.Root, "workloads", workload, "release-states")
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return state, nil, err
	}
	if err = json.Unmarshal(data, &state); err != nil {
		return state, nil, err
	}
	if !strings.HasPrefix(filepath.Clean(state.ComposePath), filepath.Clean(dir)+string(os.PathSeparator)) {
		return state, nil, errors.New("invalid retained Compose path")
	}
	data, err = os.ReadFile(state.ComposePath)
	if err != nil {
		return state, nil, err
	}
	var c struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err = json.Unmarshal(data, &c); err != nil {
		return state, nil, err
	}
	images := map[string]string{}
	for service, config := range c.Services {
		images[service] = config.Image
	}
	return state, images, nil
}

func (e *Engine) CompareReleases(ctx context.Context, in ReleaseComparisonRequest, proxy *outbound.Config) (*ReleaseComparison, error) {
	if !workloadIdentifier.MatchString(in.Workload) || !identifier.MatchString(in.From) || !identifier.MatchString(in.To) {
		return nil, errors.New("invalid release comparison")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !e.comparisonMu.TryLock() {
		return nil, errors.New("another release comparison is running; retry when it completes")
	}
	defer e.comparisonMu.Unlock()
	e.mu.Lock()
	busy := e.busy(in.Workload)
	e.mu.Unlock()
	if busy {
		return nil, errors.New("wait for active operations before comparing releases")
	}

	from, a, err := e.releaseComparisonState(in.Workload, in.From)
	if err != nil {
		return nil, err
	}
	to, b, err := e.releaseComparisonState(in.Workload, in.To)
	if err != nil {
		return nil, err
	}
	out := &ReleaseComparison{FromRevision: from.Revision, ToRevision: to.Revision, Images: []ImageChange{}, Commits: []ReleaseCommit{}}
	out.EnvironmentChanges = map[string]bool{}
	for name, value := range from.Environment {
		other, exists := to.Environment[name]
		out.EnvironmentChanges[name] = !exists || value != other
	}
	for name, value := range to.Environment {
		other, exists := from.Environment[name]
		out.EnvironmentChanges[name] = !exists || value != other
	}
	names := map[string]bool{}
	for name := range a {
		names[name] = true
	}
	for name := range b {
		names[name] = true
	}
	for name := range names {
		out.Images = append(out.Images, ImageChange{name, a[name], b[name], a[name] != b[name]})
	}
	sort.Slice(out.Images, func(i, j int) bool { return out.Images[i].Service < out.Images[j].Service })
	if in.DifferentRepositories {
		out.CommitMessage = "The releases use different repositories; their Git histories are not comparable."
		return out, nil
	}
	if from.Revision == "" && to.Revision == "" {
		out.CommitsComplete = true
		out.CommitMessage = "Neither release is Git-backed; there are no Git commits to compare."
		return out, nil
	}
	if from.Revision == "" || to.Revision == "" {
		out.CommitMessage = "One or both releases are not Git-backed."
		return out, nil
	}
	if from.Revision == to.Revision {
		out.CommitsComplete = true
		out.CommitMessage = "Both releases use the same Git commit."
		return out, nil
	}
	if !regexp.MustCompile(`^[a-f0-9]{40,64}$`).MatchString(from.Revision) || !regexp.MustCompile(`^[a-f0-9]{40,64}$`).MatchString(to.Revision) {
		return nil, errors.New("invalid retained Git revision")
	}
	checkoutID := strings.TrimSuffix(filepath.Base(to.ComposePath), ".compose.json")
	directory := filepath.Join(e.Root, "workloads", in.Workload, "releases", checkoutID)
	journal, readErr := e.read(checkoutID)
	if readErr == nil && journal.Request.Source != nil {
		request := journal.Request
		if in.Source != nil {
			if validateSource(in.Source) != nil {
				return nil, errors.New("invalid comparison source")
			}
			request.Source = in.Source
		}
		request.OutboundProxy = proxy
		environment, gitArgs := gitAccess(request)
		shallow := &limitedBuffer{Limit: 64}
		if e.Run(ctx, directory, []string{"git", "rev-parse", "--is-shallow-repository"}, environment, shallow) == nil {
			fetch := append(gitArgs, "fetch", "--no-tags")
			if strings.TrimSpace(shallow.data.String()) == "true" {
				fetch = append(fetch, "--unshallow")
			}
			fetch = append(fetch, "--", request.Source.Repository, from.Revision, to.Revision)
			// Only Git metadata is fetched; no checkout or deployment is performed.
			if e.Run(ctx, directory, fetch, environment, io.Discard) != nil {
				out.CommitMessage = "Repository history refresh failed; retained commits are shown where available."
			}
		}
	}
	log := &limitedBuffer{Limit: 256 * 1024}
	args := []string{"git", "log", "--no-decorate", "--format=%H%x09%s", "--max-count=101", from.Revision + ".." + to.Revision, "--"}
	if err = e.Run(ctx, directory, args, []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}, log); err != nil {
		out.CommitMessage = "Intermediate history is not retained in this shallow checkout. Compare the two pinned revisions in the repository; a repository history refresh could not provide the missing commits."
		out.Commits = []ReleaseCommit{{SHA: to.Revision, Subject: "Target release commit"}}
		return out, nil
	}
	for _, line := range strings.Split(strings.TrimSpace(log.data.String()), "\n") {
		sha, subject, ok := strings.Cut(line, "\t")
		if ok && regexp.MustCompile(`^[a-f0-9]{40,64}$`).MatchString(sha) {
			out.Commits = append(out.Commits, ReleaseCommit{sha, subject})
		}
	}
	out.CommitsComplete = len(out.Commits) <= 100
	if len(out.Commits) > 100 {
		out.Commits = out.Commits[:100]
	}
	shallow := &limitedBuffer{Limit: 64}
	if e.Run(ctx, directory, []string{"git", "rev-parse", "--is-shallow-repository"}, nil, shallow) != nil || strings.TrimSpace(shallow.data.String()) != "false" {
		out.CommitsComplete = false
	}
	out.CommitMessage = "Retained commits reachable from the target but not the source (at most 100); shallow or truncated history is explicitly incomplete."
	return out, nil
}
