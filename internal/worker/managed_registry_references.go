package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	registrySnapshotUnreadable = "Retained Compose snapshot is unreadable and its release cannot be proven older than this registry."
	registryImageUnresolved    = "Retained Compose image reference is missing or unresolved and its release cannot be proven older than this registry."
	registryImageUnavailable   = "Retained image is unavailable and its release cannot be proven older than this registry."
	registryDigestUnavailable  = "Retained registry image has no immutable digest; restore its local image metadata before retrying."
	registryInventoryInvalid   = "Retained release inventory is invalid or exceeds its safety limit."
)

// RPC errors can contain transport details. Only these fixed reasons are safe
// to combine with resource names in an operator-facing inventory refusal.
func ManagedRegistryInventoryReason(message string) string {
	switch message {
	case registrySnapshotUnreadable, registryImageUnresolved, registryImageUnavailable, registryDigestUnavailable, registryInventoryInvalid:
		return message
	case "resource is busy; registry reference inventory is incomplete":
		return "Resource is busy; wait for its operation to finish."
	case "retained release has no image identity":
		return "Retained release has no image identity; upgrade this worker to inspect legacy releases."
	default:
		return "Worker reference inventory is unavailable; retry when the worker is ready."
	}
}

func (e *Engine) ManagedRegistryReferences(ctx context.Context, workload, server string, previous []string) ([]string, error) {
	if !workloadIdentifier.MatchString(workload) || ValidateRegistry(&Registry{Server: server, Username: "inventory", Password: "inventory"}) != nil {
		return nil, errors.New("invalid registry reference inventory scope")
	}
	if len(previous) > 2048 {
		return nil, errors.New(registryInventoryInvalid)
	}
	predates := map[string]bool{}
	for _, id := range previous {
		if !identifier.MatchString(id) || predates[id] {
			return nil, errors.New(registryInventoryInvalid)
		}
		predates[id] = true
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
		return nil, errors.New(registrySnapshotUnreadable)
	}
	if len(entries) > 2048 {
		return nil, errors.New(registryInventoryInvalid)
	}
	workloadRoot := filepath.Dir(root)
	files, err := os.OpenRoot(workloadRoot)
	if err != nil {
		return nil, errors.New(registrySnapshotUnreadable)
	}
	defer files.Close()
	refs := map[string]bool{}
	inspected := map[string]error{}
	snapshots := map[string]bool{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimSuffix(entry.Name(), ".json"), ".compose")
		if !identifier.MatchString(id) || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return nil, errors.New(registryInventoryInvalid)
		}
		path := filepath.Join("release-states", entry.Name())
		if strings.HasSuffix(entry.Name(), ".compose.json") {
			snapshots[path] = snapshots[path] || predates[id]
			continue
		}
		data, err := registryReleaseData(files, path)
		var state struct {
			ComposePath string `json:"compose_path"`
		}
		if err != nil || json.Unmarshal(data, &state) != nil || state.ComposePath == "" {
			if predates[id] && (err == nil || err.Error() == registrySnapshotUnreadable) {
				continue
			}
			if err != nil {
				return nil, err
			}
			return nil, errors.New(registrySnapshotUnreadable)
		}
		relative, err := filepath.Rel(workloadRoot, state.ComposePath)
		if err != nil || !filepath.IsAbs(state.ComposePath) || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, errors.New(registryInventoryInvalid)
		}
		older := predates[id]
		if filepath.Dir(relative) == "release-states" && strings.HasSuffix(relative, ".compose.json") {
			original := strings.TrimSuffix(filepath.Base(relative), ".compose.json")
			older = older || predates[original]
		}
		snapshots[relative] = snapshots[relative] || older
	}
	for path, older := range snapshots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		images, err := registryReleaseImages(files, path)
		if err != nil {
			if older && err.Error() == registrySnapshotUnreadable {
				continue
			}
			return nil, err
		}
		for _, image := range images {
			if image == "" || len(image) > 512 || strings.ContainsAny(image, "$ \t\r\n\x00") {
				if older {
					continue
				}
				return nil, errors.New(registryImageUnresolved)
			}
			if !imageDigest.MatchString(image) {
				repository := imageRepository(image)
				if !strings.HasPrefix(repository, server+"/") {
					continue
				}
				repo := strings.TrimPrefix(repository, server+"/")
				if _, digest, ok := strings.Cut(image, "@"); ok && ValidateRegistryRepository(repo) == nil && managedManifestDigest.MatchString(digest) {
					refs[repo+"@"+digest] = true
					continue
				}
			}
			err, seen := inspected[image]
			if !seen {
				err = e.retainedRegistryImage(ctx, image, server, refs)
				inspected[image] = err
			}
			if err != nil {
				// An explicit reference to this registry is evidence even when
				// its local metadata is lost; age must never override it.
				if older && imageDigest.MatchString(image) && err.Error() == registryImageUnavailable {
					continue
				}
				return nil, err
			}
		}
		if len(refs) > MaxManagedRegistryReferences {
			return nil, errors.New(registryInventoryInvalid)
		}
	}
	result := make([]string, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	sort.Strings(result)
	return result, nil
}

func registryReleaseData(root *os.Root, path string) ([]byte, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return nil, errors.New(registrySnapshotUnreadable)
	}
	if !info.Mode().IsRegular() || info.Size() > 2<<20 {
		return nil, errors.New(registryInventoryInvalid)
	}
	file, err := root.Open(path)
	if err != nil {
		return nil, errors.New(registrySnapshotUnreadable)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 2<<20+1))
	if err != nil || len(data) > 2<<20 {
		return nil, errors.New(registrySnapshotUnreadable)
	}
	return data, nil
}

func registryReleaseImages(root *os.Root, path string) ([]string, error) {
	data, err := registryReleaseData(root, path)
	if err != nil {
		return nil, err
	}
	var config struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if json.Unmarshal(data, &config) != nil || len(config.Services) == 0 || len(config.Services) > 64 {
		return nil, errors.New(registrySnapshotUnreadable)
	}
	images := make([]string, 0, len(config.Services))
	for _, service := range config.Services {
		images = append(images, service.Image)
	}
	return images, nil
}

func (e *Engine) retainedRegistryImage(ctx context.Context, image, server string, refs map[string]bool) error {
	var metadata struct {
		RepoDigests []string `json:"RepoDigests"`
		RepoTags    []string `json:"RepoTags"`
	}
	if err := e.imageJSON(ctx, nil, &metadata, "docker", "image", "inspect", "--format", "{{json .}}", image); err != nil {
		return errors.New(registryImageUnavailable)
	}
	matched := false
	for _, ref := range metadata.RepoDigests {
		if strings.HasPrefix(ref, server+"/") {
			repo, digest, ok := strings.Cut(strings.TrimPrefix(ref, server+"/"), "@")
			if !ok || ValidateRegistryRepository(repo) != nil || !managedManifestDigest.MatchString(digest) {
				return errors.New(registryInventoryInvalid)
			}
			refs[repo+"@"+digest] = true
			matched = true
		}
	}
	for _, tag := range metadata.RepoTags {
		if strings.HasPrefix(tag, server+"/") && !matched {
			return errors.New(registryDigestUnavailable)
		}
	}
	if strings.HasPrefix(imageRepository(image), server+"/") && !matched {
		return errors.New(registryDigestUnavailable)
	}
	return nil
}
