package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const buildCacheImage = "docker.io/moby/buildkit@sha256:baf6138eb8c37644800c7c913fba051e840ebb9ba13eaf92d92ef9a1712d0b4c"

var errBuildCacheFull = errors.New("the verified build cache filesystem is full")

type BuildCacheSettings struct {
	Path string `json:"path"`
}

type BuildCacheStatus struct {
	Supported         bool   `json:"supported"`
	CanConfigure      bool   `json:"can_configure"`
	Configured        bool   `json:"configured"`
	Verified          bool   `json:"verified"`
	CleanupPending    bool   `json:"cleanup_pending"`
	Path              string `json:"path"`
	CapacityBytes     uint64 `json:"capacity_bytes"`
	AvailableBytes    uint64 `json:"available_bytes"`
	Image             string `json:"image"`
	Reason            string `json:"reason"`
	CanManage         bool   `json:"can_manage"`
	ActiveOperationID string `json:"active_operation_id"`
}

type cacheFilesystem struct {
	Filesystem string `json:"filesystem"`
	Device     uint64 `json:"device"`
	Source     string `json:"source"`
	Capacity   uint64 `json:"capacity"`
}

type cacheDaemon struct {
	ID   string `json:"id"`
	Root string `json:"root"`
}

type cacheLocation struct {
	Docker     cacheDaemon     `json:"docker"`
	Path       string          `json:"path"`
	Owner      string          `json:"owner"`
	Filesystem cacheFilesystem `json:"filesystem"`
	Image      string          `json:"image"`
	Prune      bool            `json:"prune"`
}

type buildCacheState struct {
	Current *cacheLocation  `json:"current"`
	Retired []cacheLocation `json:"retired"`
}

type protectedBuildOwner struct {
	Protocol int    `json:"cove_build_cache_protocol"`
	Owner    string `json:"owner"`
}

func decodeBuildOwner(data []byte) (string, bool, error) {
	var owner string
	protected := false
	if json.Unmarshal(data, &owner) != nil {
		var wire protectedBuildOwner
		if json.Unmarshal(data, &wire) != nil || wire.Protocol != 1 {
			return "", false, errors.New("invalid build owner")
		}
		owner, protected = wire.Owner, true
	}
	if len(owner) != 32 {
		return "", false, errors.New("invalid build owner")
	}
	if _, err := hex.DecodeString(owner); err != nil {
		return "", false, errors.New("invalid build owner")
	}
	return owner, protected, nil
}

func (e *Engine) protectBuildOwner(owner string, protect bool) error {
	path := filepath.Join(e.Root, "build-owner.json")
	if protect {
		// Older workers expect a JSON string here and must refuse uncapped builds after downgrade.
		return writeJSON(path, protectedBuildOwner{Protocol: 1, Owner: owner})
	}
	return writeJSON(path, owner)
}

func cacheBuilderName(location cacheLocation) string {
	data, _ := json.Marshal(struct {
		Path       string
		Filesystem cacheFilesystem
		Image      string
		Docker     cacheDaemon
	}{location.Path, location.Filesystem, location.Image, location.Docker})
	digest := sha256.Sum256(data)
	return "cove-cache-" + location.Owner + "-" + hex.EncodeToString(digest[:6])
}

func (e *Engine) readBuildCache() (buildCacheState, error) {
	var state buildCacheState
	data, err := os.ReadFile(filepath.Join(e.Root, "build-cache.json"))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil || json.Unmarshal(data, &state) != nil || len(state.Retired) > 32 {
		return state, errors.New("build cache configuration is unreadable")
	}
	locations := append([]cacheLocation{}, state.Retired...)
	if state.Current != nil {
		locations = append(locations, *state.Current)
	}
	for _, location := range locations {
		if len(location.Owner) != 32 || !validCachePath(location.Path) || !validCacheImage(location.Image) || location.Filesystem.Capacity == 0 || location.Docker.ID == "" || len(location.Docker.ID) > 128 || !filepath.IsAbs(location.Docker.Root) {
			return state, errors.New("build cache configuration is invalid")
		}
		if _, err := hex.DecodeString(location.Owner); err != nil {
			return state, errors.New("build cache owner is invalid")
		}
	}
	return state, nil
}

func validCacheImage(image string) bool {
	prefix, digest, ok := strings.Cut(image, "@")
	return ok && prefix == "docker.io/moby/buildkit" && imageDigest.MatchString(digest)
}

func validCachePath(path string) bool {
	return len(path) <= 4096 && filepath.IsAbs(path) && filepath.Clean(path) == path && path != string(filepath.Separator) && !strings.ContainsAny(path, ",\"\x00\r\n")
}

func (e *Engine) cacheDockerEndpoint(ctx context.Context) (string, error) {
	endpoint := os.Getenv("DOCKER_HOST")
	if os.Getenv("DOCKER_CONTEXT") != "" || endpoint == "" {
		out := &limitedBuffer{Limit: 8192}
		if err := e.Run(ctx, e.Root, []string{"docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"}, nil, out); err != nil {
			return "", errors.New("the local Docker endpoint cannot be verified")
		}
		endpoint = strings.TrimSpace(out.data.String())
	}
	if !strings.HasPrefix(endpoint, "unix:///") || strings.ContainsAny(endpoint, "\x00\r\n?#") {
		return "", errors.New("hard cache verification requires a local Unix Docker endpoint and the worker's host filesystem namespace")
	}
	info, err := os.Stat(strings.TrimPrefix(endpoint, "unix://"))
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return "", errors.New("the local Docker socket cannot be verified")
	}
	return endpoint, nil
}

func (e *Engine) cacheDockerRoot(ctx context.Context) (cacheDaemon, error) {
	if _, err := e.cacheDockerEndpoint(ctx); err != nil {
		return cacheDaemon{}, err
	}
	out := &limitedBuffer{Limit: 8192}
	if err := e.Run(ctx, e.Root, []string{"docker", "info", "--format", `{"root":{{json .DockerRootDir}},"id":{{json .ID}}}`}, nil, out); err != nil {
		return cacheDaemon{}, errors.New("Docker's identity and filesystem location cannot be verified")
	}
	var daemon cacheDaemon
	if json.Unmarshal(out.data.Bytes(), &daemon) != nil || !filepath.IsAbs(daemon.Root) || daemon.ID == "" || len(daemon.ID) > 128 {
		return cacheDaemon{}, errors.New("Docker's identity and filesystem location cannot be verified")
	}
	return daemon, nil
}

func (e *Engine) verifyCacheLocation(ctx context.Context, location cacheLocation, writable bool) (uint64, error) {
	root, err := e.cacheDockerRoot(ctx)
	if err != nil {
		return 0, err
	}
	if root != location.Docker {
		return 0, errors.New("the configured Docker daemon changed; restore the original endpoint before using or cleaning its cache")
	}
	identity, available, err := inspectCacheFilesystem(location.Path, root.Root)
	if err != nil {
		return 0, err
	}
	if identity != location.Filesystem {
		return 0, errors.New("the build cache mount was replaced or resized; explicitly verify the supplied filesystem again")
	}
	data, err := os.ReadFile(filepath.Join(e.Root, "build-owner.json"))
	owner, protected, decodeErr := decodeBuildOwner(data)
	if err != nil || decodeErr != nil || !protected || owner != location.Owner {
		return 0, errors.New("build cache ownership does not match this worker")
	}
	if err = verifyCacheOwner(location.Path, owner, writable); err != nil {
		return available, err
	}
	if err = e.verifyCacheDaemonView(ctx, location); err != nil {
		return available, err
	}
	if err = e.verifyCacheMountUsers(ctx, location); err != nil {
		return available, err
	}
	return available, nil
}

func (e *Engine) BuildCacheStatus(ctx context.Context) BuildCacheStatus {
	status := BuildCacheStatus{Supported: runtime.GOOS == "linux", Image: buildCacheImage}
	if !status.Supported {
		status.Reason = "hard build-cache verification requires Linux"
		return status
	}
	_, endpointErr := e.cacheDockerRoot(ctx)
	proxyErr := e.cacheProxyReady()
	status.CanConfigure = endpointErr == nil && proxyErr == nil
	state, err := e.readBuildCache()
	if err != nil {
		status.Reason = err.Error()
		return status
	}
	status.CleanupPending = len(state.Retired) > 0 || state.Current != nil && state.Current.Prune
	if state.Current == nil {
		status.Reason = "no dedicated bounded build-cache filesystem is configured; existing garbage collection is not a hard cap"
		if endpointErr != nil {
			status.Reason = endpointErr.Error()
		}
		if proxyErr != nil {
			status.Reason = proxyErr.Error()
		}
		if data, err := os.ReadFile(filepath.Join(e.Root, "build-owner.json")); err == nil {
			if _, protected, err := decodeBuildOwner(data); err != nil || protected {
				status.Reason = "build-cache configuration or cleanup is incomplete; explicitly configure or disable it before building"
			}
		}
		return status
	}
	status.Configured, status.Path = true, state.Current.Path
	if proxyErr != nil {
		status.Reason = proxyErr.Error()
		return status
	}
	available, err := e.verifyCacheLocation(ctx, *state.Current, true)
	if err != nil {
		status.Reason = err.Error()
		return status
	}
	if status.CleanupPending {
		status.Reason = "owned build-cache cleanup is pending; new builds remain paused"
		return status
	}
	status.Verified, status.CapacityBytes, status.AvailableBytes = true, state.Current.Filesystem.Capacity, available
	return status
}

func (e *Engine) ConfigureBuildCache(ctx context.Context, settings BuildCacheSettings) (BuildCacheStatus, error) {
	if runtime.GOOS != "linux" {
		return BuildCacheStatus{}, errors.New("hard build-cache verification requires Linux")
	}
	unlock, err := e.lockNodeChange(false)
	if err != nil {
		return BuildCacheStatus{}, err
	}
	defer unlock()
	if err := e.requireInterlockedHostSchedule(); err != nil {
		return BuildCacheStatus{}, err
	}
	if !e.buildMu.TryLock() {
		return BuildCacheStatus{}, errors.New("wait for the active build or cache cleanup before configuring the build cache")
	}
	defer e.buildMu.Unlock()
	e.mu.Lock()
	if e.draining || e.hasMoveFence() || e.hostChanging || e.proxyRunning || e.customRunning || e.customUnsettled || len(e.active) > 0 || len(e.pending) > 0 || len(e.recovering) > 0 {
		e.mu.Unlock()
		return BuildCacheStatus{}, errors.New("wait for worker operations and proxy recovery before changing build-cache settings")
	}
	e.hostChanging = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.hostChanging = false; e.mu.Unlock() }()
	if settings.Path != "" {
		if err := e.cacheProxyReady(); err != nil {
			return BuildCacheStatus{}, err
		}
	}
	state, err := e.readBuildCache()
	if err != nil {
		return BuildCacheStatus{}, err
	}
	if err = e.settleCacheRetirement(ctx, &state, io.Discard); err != nil {
		return BuildCacheStatus{}, err
	}
	if settings.Path != "" && state.Current != nil && state.Current.Prune {
		if err = e.settleCachePrune(ctx, &state, io.Discard); err != nil {
			return BuildCacheStatus{}, err
		}
	}
	owner, err := e.buildOwner()
	if err != nil {
		return BuildCacheStatus{}, err
	}
	var next *cacheLocation
	if settings.Path != "" {
		if !validCachePath(settings.Path) {
			return BuildCacheStatus{}, errors.New("supply an existing absolute dedicated filesystem mount path")
		}
		root, err := e.cacheDockerRoot(ctx)
		if err != nil {
			return BuildCacheStatus{}, err
		}
		if state.Current != nil && state.Current.Docker != root {
			return BuildCacheStatus{}, errors.New("restore the original Docker endpoint and disable its cache before changing daemons")
		}
		identity, _, err := inspectCacheFilesystem(settings.Path, root.Root)
		if err != nil {
			return BuildCacheStatus{}, err
		}
		next = &cacheLocation{Path: settings.Path, Owner: owner, Filesystem: identity, Image: buildCacheImage, Docker: root}
		if err = e.verifyCacheDaemonView(ctx, *next); err != nil {
			return BuildCacheStatus{}, err
		}
		if err = e.verifyCacheMountUsers(ctx, *next); err != nil {
			return BuildCacheStatus{}, err
		}
		if state.Current != nil && state.Current.Path == settings.Path && identity.Capacity < state.Current.Filesystem.Capacity {
			return BuildCacheStatus{}, errors.New("restore the configured filesystem size or disable the cap before supplying a smaller filesystem; Cove does not resize storage")
		}
		if err = claimCacheOwner(settings.Path, owner); err != nil {
			return BuildCacheStatus{}, err
		}
		if err = e.protectBuildOwner(owner, true); err != nil {
			return BuildCacheStatus{}, err
		}
	}
	if state.Current != nil && next != nil && *state.Current == *next {
		return e.BuildCacheStatus(ctx), nil
	}
	if state.Current != nil {
		state.Retired = append(state.Retired, *state.Current)
	}
	state.Current = next
	if err = writeJSON(filepath.Join(e.Root, "build-cache.json"), state); err != nil {
		return BuildCacheStatus{}, err
	}
	if err = e.settleCacheRetirement(ctx, &state, io.Discard); err != nil {
		return BuildCacheStatus{}, err
	}
	if next == nil {
		if err = e.protectBuildOwner(owner, false); err != nil {
			return BuildCacheStatus{}, err
		}
	}
	return e.BuildCacheStatus(ctx), nil
}

func cacheFullError(cleanup error) error {
	if cleanup != nil {
		return errors.Join(errBuildCacheFull, fmt.Errorf("owned cache cleanup remains pending: %w", cleanup))
	}
	return fmt.Errorf("%w; unused owned cache was pruned, and daemon images, releases and registry storage were left untouched", errBuildCacheFull)
}
