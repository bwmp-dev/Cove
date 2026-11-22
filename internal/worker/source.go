package worker

import (
	"bytes"
	"context"
	"errors"
	"golang.org/x/crypto/ssh"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type GitSource struct {
	Username      string   `json:"username,omitempty"`
	SSHPrivateKey string   `json:"ssh_private_key,omitempty"`
	SSHHostKey    string   `json:"ssh_host_key,omitempty"`
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
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "ssh") {
		return errors.New("Git repository must be an HTTPS or SSH URL")
	}
	if u.Scheme == "ssh" {
		if u.User == nil || !regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,63}$`).MatchString(u.User.Username()) || u.Path == "" || strings.ContainsAny(u.Host+u.Path, "\r\n\x00") || strings.ContainsAny(u.Host, " \t\\\"'`;$()|&<>") || source.Token != "" {
			return errors.New("invalid SSH Git repository")
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return errors.New("SSH URLs cannot contain passwords")
		}
		if _, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(source.SSHHostKey)); err != nil || len(options) != 0 || len(strings.TrimSpace(string(rest))) != 0 {
			return errors.New("one pinned SSH host public key is required")
		}
		if source.SSHPrivateKey != "" {
			signer, err := ssh.ParsePrivateKey([]byte(source.SSHPrivateKey))
			if err != nil || signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
				return errors.New("invalid SSH deploy key")
			}
		}
	} else if u.User != nil || source.SSHHostKey != "" || source.SSHPrivateKey != "" {
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
	if len(source.Username) > 100 || strings.ContainsAny(source.Username, "\r\n\x00") {
		return errors.New("invalid Git username")
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
	if strings.HasPrefix(r.Source.Repository, "ssh://") {
		if r.Source.SSHPrivateKey == "" {
			return "", "", errors.New("SSH deploy key is unavailable")
		}
		credentialDir, err := os.MkdirTemp(root, ".cove-git-ssh-")
		if err != nil {
			return "", "", err
		}
		defer func() {
			if err := os.RemoveAll(credentialDir); err != nil {
				slog.Error("could not remove temporary Git credentials")
			}
		}()
		identity := filepath.Join(credentialDir, "identity")
		knownHosts := filepath.Join(credentialDir, "known_hosts")
		if err = os.WriteFile(identity, []byte(r.Source.SSHPrivateKey), 0600); err != nil {
			return "", "", err
		}
		if err = os.WriteFile(knownHosts, []byte("cove-git "+strings.TrimSpace(r.Source.SSHHostKey)+"\n"), 0600); err != nil {
			return "", "", err
		}
		quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
		command := "ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes -o IdentityAgent=none -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o GlobalKnownHostsFile=/dev/null -o HostKeyAlias=cove-git -o UserKnownHostsFile=" + quote(knownHosts) + " -i " + quote(identity)
		environment = append(environment, "GIT_SSH_COMMAND="+command, "GIT_SSH_VARIANT=ssh")
	}
	args := []string{"git"}
	if r.Source.Token != "" {
		username := r.Source.Username
		if username == "" {
			username = "x-access-token"
		}
		parsed, err := url.Parse(r.Source.Repository)
		if err != nil {
			return "", "", err
		}
		environment = append(environment, "COVE_GIT_TOKEN="+r.Source.Token, "COVE_GIT_USERNAME="+username, "COVE_GIT_HOST="+parsed.Host)
		args = append(args, "-c", `credential.helper=!f() { [ "$1" = get ] || return 0; protocol=; host=; while IFS= read -r line; do case "$line" in protocol=*) protocol=${line#protocol=} ;; host=*) host=${line#host=} ;; esac; done; [ "$protocol" = https ] && [ "$host" = "$COVE_GIT_HOST" ] || return 0; printf 'username=%s\npassword=%s\n' "$COVE_GIT_USERNAME" "$COVE_GIT_TOKEN"; }; f`)
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
