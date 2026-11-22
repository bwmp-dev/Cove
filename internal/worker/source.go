package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type GitSource struct {
	InlineCompose bool     `json:"inline_compose,omitempty"`
	WatchPaths    []string `json:"watch_paths,omitempty"`
	Revision      string   `json:"revision,omitempty"`
	Repository    string   `json:"repository"`
	Branch        string   `json:"branch"`
	ComposePath   string   `json:"compose_path"`
	Token         string   `json:"token,omitempty"`
}

func validateSource(source *GitSource) error {
	if source == nil {
		return nil
	}
	if source.Revision != "" && !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(source.Revision) {
		return errors.New("invalid Git revision")
	}
	if err := ValidateWatchPaths(source.WatchPaths); err != nil {
		return err
	}
	u, err := url.Parse(source.Repository)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("Git repository must be an HTTPS URL without embedded credentials")
	}
	if source.Branch == "" || len(source.Branch) > 200 || strings.HasPrefix(source.Branch, "-") || strings.ContainsAny(source.Branch, " \r\n\t~^:?*[\\") || strings.Contains(source.Branch, "..") {
		return errors.New("invalid Git branch")
	}
	if source.InlineCompose && source.ComposePath != "" {
		return errors.New("inline Compose cannot also select a repository Compose file")
	}
	if !source.InlineCompose && (source.ComposePath == "" || filepath.IsAbs(source.ComposePath) || strings.Contains(source.ComposePath, "\\") || strings.HasPrefix(filepath.Clean(source.ComposePath), "..") || strings.Contains(source.ComposePath, ":")) {
		return errors.New("compose_path must be relative to the repository")
	}
	if len(source.Token) > 8192 || strings.ContainsAny(source.Token, "\r\n\x00") {
		return errors.New("invalid repository credential")
	}
	return nil
}

func (e *Engine) checkout(ctx context.Context, r Request, root string, out io.Writer) (string, string, error) {
	directory := filepath.Join(root, "releases", r.ID)
	if err := os.MkdirAll(filepath.Dir(directory), 0700); err != nil {
		return "", "", err
	}
	environment := []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	if r.OutboundProxy != nil {
		environment = append(environment, r.OutboundProxy.Env()...)
	}
	args := []string{"git"}
	if r.Source.Token != "" {
		environment = append(environment, "COVE_GIT_TOKEN="+r.Source.Token)
		args = append(args, "-c", `credential.helper=!f() { printf 'username=x-access-token\npassword=%s\n' "$COVE_GIT_TOKEN"; }; f`)
	}
	clone := append(append([]string{}, args...), "clone", "--depth", "1", "--single-branch", "--branch", r.Source.Branch, "--", r.Source.Repository, directory)
	if err := e.Run(ctx, root, clone, environment, out); err != nil {
		return "", "", err
	}
	if r.Source.Revision != "" {
		fetch := append(append([]string{}, args...), "fetch", "--depth", "1", "origin", r.Source.Revision)
		if err := e.Run(ctx, directory, fetch, environment, out); err != nil {
			return "", "", err
		}
		if err := e.Run(ctx, directory, []string{"git", "checkout", "--detach", r.Source.Revision}, environment, out); err != nil {
			return "", "", err
		}
	}
	var revision bytes.Buffer
	if err := e.Run(ctx, directory, []string{"git", "rev-parse", "HEAD"}, environment, &revision); err != nil {
		return "", "", err
	}
	commit := strings.TrimSpace(revision.String())
	if r.Source.Revision != "" && commit != r.Source.Revision {
		return "", "", errors.New("Git revision did not match requested commit")
	}
	if !regexp.MustCompile(`^[0-9a-f]{40,64}$`).MatchString(commit) {
		return "", "", errors.New("could not resolve Git commit")
	}
	if err := prepareCheckoutPermissions(directory); err != nil {
		return "", "", err
	}
	if r.Source.InlineCompose {
		file, err := os.CreateTemp(directory, ".cove-compose-*.yaml")
		if err != nil {
			return "", "", err
		}
		if _, err := file.WriteString(r.Compose); err != nil {
			file.Close()
			return "", "", err
		}
		if err := file.Close(); err != nil {
			return "", "", err
		}
		return file.Name(), commit, nil
	}
	composePath := filepath.Join(directory, filepath.FromSlash(r.Source.ComposePath))
	resolved, err := filepath.EvalSymlinks(composePath)
	if err != nil {
		return "", "", err
	}
	relative, err := filepath.Rel(directory, resolved)
	if err != nil || strings.HasPrefix(relative, "..") {
		return "", "", errors.New("compose file escapes repository")
	}
	return composePath, commit, nil
}

func prepareCheckoutPermissions(directory string) error {
	// The worker's private umask must not make COPY inputs unreadable to non-root image users.
	// The enclosing releases directory stays private; Git metadata and symlinks are untouched.
	return filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return os.Chmod(path, 0755)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := fs.FileMode(0644)
		if info.Mode()&0111 != 0 {
			mode = 0755
		}
		return os.Chmod(path, mode)
	})
}
