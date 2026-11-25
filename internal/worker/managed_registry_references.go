package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func (e *Engine) ManagedRegistryReferences(ctx context.Context, workload, server string) ([]string, error) {
	if !workloadIdentifier.MatchString(workload) || ValidateRegistry(&Registry{Server: server, Username: "inventory", Password: "inventory"}) != nil {
		return nil, errors.New("invalid registry reference inventory scope")
	}
	e.mu.Lock()
	busy := e.busy(workload)
	e.mu.Unlock()
	if busy {
		return nil, errors.New("resource is busy; registry reference inventory is incomplete")
	}
	root := filepath.Join(e.Root, "workloads", workload, "release-states")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > 2048 {
		return nil, errors.New("retained registry inventory exceeds limit")
	}
	refs := map[string]bool{}
	images := map[string]bool{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".compose.json") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, errors.New("invalid retained Compose file")
		}
		info, err := entry.Info()
		if err != nil || info.Size() > 2<<20 {
			return nil, errors.New("retained Compose inventory is unavailable")
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		var config struct {
			Services map[string]struct {
				Image string `json:"image"`
			} `json:"services"`
		}
		if json.Unmarshal(data, &config) != nil || len(config.Services) == 0 {
			return nil, errors.New("retained Compose inventory is invalid")
		}
		for _, service := range config.Services {
			if service.Image == "" {
				return nil, errors.New("retained release has no image identity")
			}
			images[service.Image] = true
		}
	}
	for image := range images {
		var metadata struct {
			RepoDigests []string `json:"RepoDigests"`
			RepoTags    []string `json:"RepoTags"`
		}
		if err = e.imageJSON(ctx, nil, &metadata, "docker", "image", "inspect", "--format", "{{json .}}", image); err != nil {
			return nil, errors.New("retained image unavailable; registry cleanup refused")
		}
		matched := false
		for _, ref := range metadata.RepoDigests {
			if strings.HasPrefix(ref, server+"/") {
				repo, digest, ok := strings.Cut(strings.TrimPrefix(ref, server+"/"), "@")
				if !ok || ValidateRegistryRepository(repo) != nil || !managedManifestDigest.MatchString(digest) {
					return nil, errors.New("invalid retained registry digest")
				}
				refs[repo+"@"+digest] = true
				matched = true
			}
		}
		for _, tag := range metadata.RepoTags {
			if strings.HasPrefix(tag, server+"/") && !matched {
				return nil, errors.New("retained registry image has no immutable digest; cleanup refused")
			}
		}
		if len(refs) > MaxManagedRegistryReferences {
			return nil, errors.New("retained registry reference inventory exceeds its bound")
		}
	}
	result := make([]string, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	sort.Strings(result)
	return result, nil
}
