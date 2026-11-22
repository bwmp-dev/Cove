package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type ComposeFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func composeBundleSecrets(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config struct {
		Services map[string]struct {
			Environment map[string]*string `json:"environment"`
			Build       struct {
				Args map[string]*string `json:"args"`
			} `json:"build"`
		} `json:"services"`
	}
	if err = json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, service := range config.Services {
		for _, values := range []map[string]*string{service.Environment, service.Build.Args} {
			for _, value := range values {
				if value != nil && *value != "" {
					seen[strings.ReplaceAll(*value, "$$", "$")] = true
				}
			}
		}
	}
	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	sort.Strings(values)
	return values, nil
}

type ComposeBundle struct {
	Files    []ComposeFile     `json:"files,omitempty"`
	Assets   map[string]string `json:"assets,omitempty"`
	Paths    []string          `json:"paths,omitempty"`
	Profiles []string          `json:"profiles,omitempty"`
}

var composeProfile = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

func composeRelativePath(value string) bool {
	if value == "" || len(value) > 240 || strings.ContainsAny(value, "\\:\x00\r\n") || strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == "." || strings.EqualFold(part, ".git") || strings.HasPrefix(strings.ToLower(part), ".cove-") {
			return false
		}
	}
	return true
}

func ValidateComposeBundle(bundle *ComposeBundle, source *GitSource, preview bool) error {
	if bundle == nil {
		return nil
	}
	if preview {
		return errors.New("restricted pull-request previews require one repository Compose file without profiles")
	}
	if len(bundle.Files) > 7 || len(bundle.Paths) > 7 || len(bundle.Assets) > 32 || len(bundle.Profiles) > 16 {
		return errors.New("Compose accepts at most eight selected files, 32 assets and 16 profiles")
	}
	if source != nil && (len(bundle.Files) > 0 || len(bundle.Assets) > 0) || source == nil && len(bundle.Paths) > 0 {
		return errors.New("Git sources select repository paths; inline sources provide file contents")
	}
	if source != nil && source.InlineCompose && len(bundle.Paths) > 0 {
		return errors.New("Cove-managed Git Compose cannot select repository override paths")
	}
	seen := map[string]bool{"compose.yaml": source == nil}
	if source != nil {
		seen[strings.ToLower(path.Clean(source.ComposePath))] = true
	}
	bytes := 0
	check := func(name, content string, hasContent bool) error {
		key := strings.ToLower(name)
		if !composeRelativePath(name) || seen[key] || hasContent && (len(content) > 1<<20 || strings.ContainsRune(content, 0)) {
			return errors.New("Compose files require unique relative paths and at most 1 MiB of text per file")
		}
		seen[key] = true
		bytes += len(content)
		if bytes > 512<<10 {
			return errors.New("additional Compose files and assets must not exceed 512 KiB")
		}
		return nil
	}
	for _, file := range bundle.Files {
		if err := check(file.Path, file.Content, true); err != nil {
			return err
		}
	}
	for name, content := range bundle.Assets {
		if err := check(name, content, true); err != nil {
			return err
		}
	}
	for _, name := range bundle.Paths {
		if err := check(name, "", false); err != nil {
			return err
		}
	}
	seen = map[string]bool{}
	for _, profile := range bundle.Profiles {
		if !composeProfile.MatchString(profile) || seen[profile] {
			return errors.New("Compose profiles require unique alphanumeric names with dots, dashes or underscores")
		}
		seen[profile] = true
	}
	return nil
}

func (e *Engine) prepareComposeBundle(ctx context.Context, r Request, directory, primary string, environment []string) (string, error) {
	if r.ComposeBundle == nil {
		return normalizeComposeFile(primary)
	}
	if err := ValidateComposeBundle(r.ComposeBundle, r.Source, r.Preview); err != nil {
		return "", err
	}
	files := []string{primary}
	if r.Source == nil {
		root := filepath.Join(directory, "compose-bundles", r.ID)
		if err := os.MkdirAll(root, 0700); err != nil {
			return "", err
		}
		write := func(name, content string) (string, error) {
			target := filepath.Join(root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return "", err
			}
			return target, os.WriteFile(target, []byte(content), 0600)
		}
		var err error
		files[0], err = write("compose.yaml", r.Compose)
		if err != nil {
			return "", err
		}
		for _, file := range r.ComposeBundle.Files {
			name, err := write(file.Path, file.Content)
			if err != nil {
				return "", err
			}
			files = append(files, name)
		}
		for name, content := range r.ComposeBundle.Assets {
			if _, err = write(name, content); err != nil {
				return "", err
			}
		}
	} else {
		root := filepath.Join(directory, "releases", r.ID)
		for _, name := range r.ComposeBundle.Paths {
			candidate := filepath.Join(root, filepath.FromSlash(name))
			resolved, err := filepath.EvalSymlinks(candidate)
			if err != nil {
				return "", err
			}
			relative, err := filepath.Rel(root, resolved)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return "", errors.New("Compose override escapes repository")
			}
			files = append(files, candidate)
		}
	}
	args := []string{"docker", "compose", "--project-name", "cove-" + r.Workload}
	for _, name := range files {
		normalized, err := normalizeComposeFile(name)
		if err != nil {
			return "", err
		}
		args = append(args, "--file", normalized)
	}
	for _, profile := range r.ComposeBundle.Profiles {
		args = append(args, "--profile", profile)
	}
	args = append(args, "config", "--format", "json")
	resolved := &limitedBuffer{Limit: 2 << 20}
	if err := e.Run(ctx, directory, args, environment, resolved); err != nil {
		return "", errors.New("could not resolve selected Compose files and profiles")
	}
	var config map[string]any
	if err := unmarshalCompose(resolved.data.Bytes(), &config); err != nil {
		return "", errors.New("invalid resolved Compose bundle")
	}
	services, _ := config["services"].(map[string]any)
	if len(services) == 0 {
		return "", errors.New("selected Compose files and profiles have no services")
	}
	for _, raw := range services {
		if service, ok := raw.(map[string]any); ok {
			delete(service, "profiles")
		}
	}
	target := filepath.Join(directory, r.ID+".bundle.compose.json")
	return target, writeJSON(target, literalCompose(config))
}
