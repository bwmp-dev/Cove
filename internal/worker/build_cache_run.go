package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type cacheDiagnosticTail []byte

func (tail *cacheDiagnosticTail) Write(data []byte) (int, error) {
	const limit = 32768
	n := len(data)
	if len(data) >= limit {
		*tail = append((*tail)[:0], data[len(data)-limit:]...)
	} else {
		if len(*tail)+len(data) > limit {
			*tail = append((*tail)[:0], (*tail)[len(*tail)+len(data)-limit:]...)
		}
		*tail = append(*tail, data...)
	}
	return n, nil
}

func (e *Engine) settleCachePrune(ctx context.Context, state *buildCacheState, out io.Writer) error {
	if state.Current == nil || !state.Current.Prune {
		return nil
	}
	location := *state.Current
	if _, err := e.verifyCacheLocation(ctx, location, false); err != nil {
		return err
	}
	if err := e.ensureCacheBuilder(ctx, location, out); err != nil {
		return err
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "buildx", "prune", "--builder", cacheBuilderName(location), "--force", "--all"}, []string{"BUILDX_CONFIG=" + filepath.Join(e.Root, "buildx")}, out); err != nil {
		return errors.New("owned BuildKit cache pruning remains pending")
	}
	if _, err := e.verifyCacheLocation(ctx, location, true); err != nil {
		return err
	}
	state.Current.Prune = false
	return writeJSON(filepath.Join(e.Root, "build-cache.json"), state)
}

func (e *Engine) recoverBuildCache(ctx context.Context, out io.Writer, ensure bool) (*cacheLocation, error) {
	state, err := e.readBuildCache()
	if err != nil {
		return nil, err
	}
	if err = e.settleCacheRetirement(ctx, &state, out); err != nil {
		return nil, err
	}
	if state.Current == nil {
		data, err := os.ReadFile(filepath.Join(e.Root, "build-owner.json"))
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		owner, protected, err := decodeBuildOwner(data)
		if err != nil {
			return nil, err
		}
		if protected {
			if _, err = os.Stat(filepath.Join(e.Root, "build-cache.json")); err != nil {
				return nil, errors.New("build-cache configuration is incomplete; explicitly configure or disable it before building")
			}
			if err = e.protectBuildOwner(owner, false); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	if state.Current.Image != buildCacheImage {
		previous := *state.Current
		state.Current.Image = buildCacheImage
		state.Retired = append(state.Retired, previous)
		if err = writeJSON(filepath.Join(e.Root, "build-cache.json"), state); err != nil {
			return nil, err
		}
		if err = e.settleCacheRetirement(ctx, &state, out); err != nil {
			return nil, err
		}
	}
	if err = e.settleCachePrune(ctx, &state, out); err != nil {
		return nil, err
	}
	if _, err = e.verifyCacheLocation(ctx, *state.Current, true); err != nil {
		if errors.Is(err, errBuildCacheFull) {
			state.Current.Prune = true
			if save := writeJSON(filepath.Join(e.Root, "build-cache.json"), state); save != nil {
				return nil, errors.Join(err, save)
			}
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			return nil, cacheFullError(e.settleCachePrune(cleanup, &state, out))
		}
		return nil, err
	}
	if ensure {
		if err = e.ensureCacheBuilder(ctx, *state.Current, out); err != nil {
			return nil, err
		}
	}
	return state.Current, nil
}

func (e *Engine) finishCappedBuild(ctx context.Context, location cacheLocation, failure error, diagnostic []byte, out io.Writer) error {
	_, verifyErr := e.verifyCacheLocation(ctx, location, true)
	full := errors.Is(verifyErr, errBuildCacheFull)
	if failure != nil {
		text := strings.ToLower(string(diagnostic))
		full = full || strings.Contains(text, "no space left on device") && strings.Contains(text, "/var/lib/buildkit/")
	}
	if !full {
		return errors.Join(failure, verifyErr)
	}
	state, err := e.readBuildCache()
	if err != nil || state.Current == nil || cacheBuilderName(*state.Current) != cacheBuilderName(location) {
		return errors.Join(errBuildCacheFull, errors.New("owned cache cleanup could not be checkpointed"), err)
	}
	state.Current.Prune = true
	if err = writeJSON(filepath.Join(e.Root, "build-cache.json"), state); err != nil {
		return errors.Join(errBuildCacheFull, err)
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	return cacheFullError(e.settleCachePrune(cleanup, &state, out))
}
