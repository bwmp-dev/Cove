package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

func (e *Engine) cleanupBuilder(ctx context.Context, owner string) (string, error) {
	state, err := e.readBuildCache()
	if err != nil {
		return "", err
	}
	if len(state.Retired) > 0 || state.Current != nil && state.Current.Prune {
		return "", errors.New("finish pending build-cache recovery before previewing scheduled cleanup")
	}
	if state.Current == nil {
		data, err := os.ReadFile(filepath.Join(e.Root, "build-owner.json"))
		if err != nil {
			return "", err
		}
		_, protected, err := decodeBuildOwner(data)
		if err != nil || protected {
			return "", errors.New("build-cache configuration is incomplete; explicitly configure or disable it before cleanup")
		}
		if _, err = os.Stat(filepath.Join(e.Root, "builder-ready.json")); errors.Is(err, os.ErrNotExist) {
			return "", nil
		} else if err != nil {
			return "", err
		}
		return "cove-" + owner, nil
	}
	if _, err = e.verifyCacheLocation(ctx, *state.Current, false); err != nil {
		return "", err
	}
	container, err := e.cacheContainer(ctx, *state.Current)
	if err != nil || container == nil {
		return "", err
	}
	if !container.State.Running {
		return "", errors.New("the capped builder is stopped; verify it before previewing scheduled cleanup")
	}
	name := cacheBuilderName(*state.Current)
	if _, err = os.Stat(filepath.Join(e.Root, "buildx", "instances", name)); err != nil {
		return "", errors.New("the capped builder registration is unavailable; verify it before cleanup")
	}
	if err = e.validateCacheBuildx(*state.Current); err != nil {
		return "", err
	}
	return name, nil
}
