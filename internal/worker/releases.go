package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func unmarshalCompose(data []byte, target any) error {
	// Docker config escapes mapping keys too; Compose input keys remain literal.
	return json.Unmarshal(bytes.ReplaceAll(data, []byte("$$"), []byte("$")), target)
}

// Escape resolved literal dollars before Compose reads the snapshot a second time.
func literalCompose(value any) any {
	next := composeDollars(value, "$", "$$")
	if config, ok := next.(map[string]any); ok {
		services, _ := config["services"].(map[string]any)
		for _, raw := range services {
			service, _ := raw.(map[string]any)
			deploy, _ := service["deploy"].(map[string]any)
			// Compose emits this empty default, then warns on re-reading it as a Swarm setting.
			if placement, ok := deploy["placement"].(map[string]any); ok && len(placement) == 0 {
				delete(deploy, "placement")
			}
		}
	}
	return next
}

func decodeResolvedCompose(data []byte, config *map[string]any) error {
	return unmarshalCompose(data, config)
}

func composeDollars(value any, from, to string) any {
	switch value := value.(type) {
	case string:
		return strings.ReplaceAll(value, from, to)
	case []any:
		for i, v := range value {
			value[i] = composeDollars(v, from, to)
		}
		return value
	case map[string]any:
		for k, v := range value {
			value[k] = composeDollars(v, from, to)
		}
		return value
	default:
		return value
	}
}

type Release struct {
	State    string    `json:"state,omitempty"`
	Revision string    `json:"revision,omitempty"`
	ID       string    `json:"id"`
	Created  time.Time `json:"created_at"`
}

func (e *Engine) Releases(workload string) ([]Release, error) {
	if !workloadIdentifier.MatchString(workload) {
		return nil, errors.New("invalid workload")
	}
	dir := filepath.Join(e.Root, "workloads", workload, "release-states")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []Release{}, nil
	}
	if err != nil {
		return nil, err
	}
	items := []Release{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".compose.json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !identifier.MatchString(id) {
			return nil, errors.New("invalid saved release")
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		release := Release{ID: id, Created: info.ModTime()}
		journal, readErr := e.read(id)
		if readErr == nil {
			release.State = journal.Operation.State
			release.Revision = journal.Operation.Revision
		} else if !os.IsNotExist(readErr) {
			return nil, errors.New("retained release journal unavailable")
		}
		items = append(items, release)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Created.After(items[j].Created) })
	return items, nil
}

func (e *Engine) pruneReleases(ctx context.Context, workload string, out io.Writer) error {
	releases, err := e.Releases(workload)
	if err != nil || len(releases) <= 10 {
		return err
	}
	dir := filepath.Join(e.Root, "workloads", workload)
	keep := map[string]bool{}
	for _, release := range releases[:10] {
		keep[release.ID] = true
	}
	protect := func(state deployedState) {
		keep[strings.TrimSuffix(filepath.Base(state.ComposePath), ".compose.json")] = true
	}
	data, err := os.ReadFile(filepath.Join(dir, "current.json"))
	if err != nil {
		return err
	}
	var current deployedState
	if err = json.Unmarshal(data, &current); err != nil {
		return err
	}
	protect(current)
	backups, err := e.Backups(workload)
	if err != nil {
		return err
	}
	for _, backup := range backups {
		data, err = os.ReadFile(filepath.Join(dir, "backups", backup.ID+".json"))
		if err != nil {
			return err
		}
		var manifest backupManifest
		if err = json.Unmarshal(data, &manifest); err != nil {
			return err
		}
		protect(manifest.State)
	}
	// Rollback records may point at an older release's immutable Compose file.
	for _, release := range releases {
		if !keep[release.ID] {
			continue
		}
		data, err = os.ReadFile(filepath.Join(dir, "release-states", release.ID+".json"))
		if err != nil {
			return err
		}
		var state deployedState
		if err = json.Unmarshal(data, &state); err != nil {
			return err
		}
		protect(state)
	}
	for _, release := range releases {
		if keep[release.ID] {
			continue
		}
		prefix := "cove-release/" + workload + ":" + strings.ToLower(release.ID) + "-"
		tags, err := e.output(ctx, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference="+prefix+"*")
		if err != nil {
			return err
		}
		for _, tag := range strings.Fields(tags) {
			if !strings.HasPrefix(tag, prefix) {
				return errors.New("unexpected retained image tag")
			}
			if err = e.Run(ctx, e.Root, []string{"docker", "image", "rm", tag}, nil, out); err != nil {
				return err
			}
		}
		for _, path := range []string{filepath.Join(dir, "release-states", release.ID+".json"), filepath.Join(dir, "release-states", release.ID+".compose.json"), filepath.Join(dir, release.ID+".yaml")} {
			if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err = os.RemoveAll(filepath.Join(dir, "releases", release.ID)); err != nil {
			return err
		}
		if err = os.RemoveAll(filepath.Join(e.Root, "file-secrets", workload, release.ID)); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) captureRelease(ctx context.Context, r Request, dir string, args, env []string) (deployedState, error) {
	output := &limitedBuffer{Limit: 2 * 1024 * 1024}
	if err := e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, output); err != nil {
		return deployedState{}, err
	}
	var config map[string]any
	if err := unmarshalCompose(output.data.Bytes(), &config); err != nil {
		return deployedState{}, err
	}
	services, ok := config["services"].(map[string]any)
	if !ok || len(services) == 0 {
		return deployedState{}, errors.New("release has no services")
	}
	for name, raw := range services {
		service, ok := raw.(map[string]any)
		if !ok || !serviceIdentifier.MatchString(name) {
			return deployedState{}, errors.New("invalid release service")
		}
		// A preview's disabled background services have no container to snapshot.
		if scale, ok := service["scale"].(float64); ok && scale == 0 && r.EnvironmentPreview != nil {
			continue
		}
		ids := &limitedBuffer{Limit: 65536}
		if err := e.Run(ctx, dir, []string{"docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=" + e.composeProject(r.Workload), "--filter", "label=com.docker.compose.service=" + name, "--filter", "label=com.docker.compose.oneoff=False"}, nil, ids); err != nil {
			return deployedState{}, err
		}
		containers := strings.Fields(ids.data.String())
		if len(containers) == 0 {
			return deployedState{}, errors.New("cannot snapshot a service without a created container")
		}
		image := ""
		for _, container := range containers {
			inspected := &limitedBuffer{Limit: 1024}
			if err := e.Run(ctx, dir, []string{"docker", "inspect", "--format", "{{.Image}}", container}, nil, inspected); err != nil {
				return deployedState{}, err
			}
			digest := strings.TrimSpace(inspected.data.String())
			if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 || (image != "" && image != digest) {
				return deployedState{}, errors.New("service images are not consistent")
			}
			image = digest
		}
		serviceHash := sha256.Sum256([]byte(name))
		retained := "cove-release/" + r.Workload + ":" + r.ID + "-" + hex.EncodeToString(serviceHash[:8])
		if err := e.Run(ctx, dir, []string{"docker", "image", "tag", image, strings.ToLower(retained)}, nil, &limitedBuffer{Limit: 1024}); err != nil {
			return deployedState{}, err
		}
		service["image"] = image
		service["pull_policy"] = "never"
		delete(service, "build")
		delete(service, "profiles")
	}
	snapshots := filepath.Join(dir, "release-states")
	if err := os.MkdirAll(snapshots, 0700); err != nil {
		return deployedState{}, err
	}
	path := filepath.Join(snapshots, r.ID+".compose.json")
	if err := writeJSON(path, literalCompose(config)); err != nil {
		return deployedState{}, err
	}
	state := deployedState{DatabaseMetadata: r.DatabaseMetadata, ComposePath: path, Environment: r.Environment}
	e.mu.Lock()
	j, err := e.read(r.ID)
	e.mu.Unlock()
	if err != nil {
		return deployedState{}, err
	}
	state.Revision = j.Operation.Revision
	return state, nil
}
