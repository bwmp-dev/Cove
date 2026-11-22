package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A private Buildx configuration prevents pruning another application's builder.
func (e *Engine) buildOwner() (string, error) {
	path := filepath.Join(e.Root, "build-owner.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var owner string
		if json.Unmarshal(data, &owner) != nil || len(owner) != 32 {
			return "", errors.New("invalid build owner")
		}
		if _, err := hex.DecodeString(owner); err != nil {
			return "", err
		}
		return owner, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	owner := hex.EncodeToString(id[:])
	return owner, writeJSON(path, owner)
}

func (e *Engine) buildImages(ctx context.Context, dir string, args, env []string, out io.Writer) error {
	e.buildMu.Lock()
	defer e.buildMu.Unlock()
	resolved := &limitedBuffer{Limit: 2 * 1024 * 1024}
	if err := e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, resolved); err != nil {
		return err
	}
	var config struct {
		Services map[string]struct {
			Build json.RawMessage `json:"build"`
		} `json:"services"`
	}
	if err := json.Unmarshal(resolved.data.Bytes(), &config); err != nil {
		return err
	}
	services := map[string]any{}
	secretBuild := false
	for name, service := range config.Services {
		if len(service.Build) > 0 && string(service.Build) != "null" {
			services[name] = nil
			var build struct {
				Secrets []json.RawMessage `json:"secrets"`
			}
			if len(service.Build) > 0 && service.Build[0] == '{' {
				if err := json.Unmarshal(service.Build, &build); err != nil {
					return err
				}
			}
			secretBuild = secretBuild || len(build.Secrets) > 0
		}
	}
	if len(services) == 0 {
		return nil
	}
	owner, err := e.buildOwner()
	if err != nil {
		return err
	}
	buildEnv := append(append([]string{}, env...), "BUILDX_CONFIG="+filepath.Join(e.Root, "buildx"))
	marker := filepath.Join(e.Root, "builder-ready.json")
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		if err := e.Run(ctx, dir, []string{"docker", "buildx", "create", "--name", "cove-" + owner, "--driver", "docker-container", "--driver-opt", "default-load=true"}, buildEnv, out); err != nil {
			// Creation can succeed before persisting the marker fails. Recover only our named private builder.
			inspected := &limitedBuffer{Limit: 4096}
			inspectErr := e.Run(ctx, dir, []string{"docker", "buildx", "inspect", "cove-" + owner, "--format", "{{.Driver}}"}, buildEnv, inspected)
			if inspectErr != nil || strings.TrimSpace(inspected.data.String()) != "docker-container" {
				return err
			}
		}
		if err := writeJSON(marker, true); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	for name := range services {
		services[name] = map[string]any{"build": map[string]any{"labels": map[string]string{"dev.cove.build-owner": owner}}}
	}
	override := filepath.Join(dir, "build-cleanup.compose.json")
	if err := writeJSON(override, map[string]any{"services": services}); err != nil {
		return err
	}
	buildArgs := append(append([]string{}, args...), "--file", override, "build", "--builder", "cove-"+owner)
	// BuildKit does not include secret contents in its cache key.
	if secretBuild {
		buildArgs = append(buildArgs, "--no-cache")
	}
	return e.Run(ctx, dir, buildArgs, buildEnv, out)
}

func (e *Engine) cleanupBuilds(ctx context.Context, out io.Writer) error {
	e.buildMu.Lock()
	defer e.buildMu.Unlock()
	// Never create ownership state during cleanup or adopt legacy shared cache.
	if _, err := os.Stat(filepath.Join(e.Root, "build-owner.json")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	owner, err := e.buildOwner()
	if err != nil {
		return err
	}
	// Without --all Docker retains tagged releases and images used by any container.
	if err := e.Run(ctx, e.Root, []string{"docker", "image", "prune", "--force", "--filter", "label=dev.cove.build-owner=" + owner, "--filter", "until=168h"}, nil, out); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(e.Root, "builder-ready.json")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return e.Run(ctx, e.Root, []string{"docker", "buildx", "prune", "--builder", "cove-" + owner, "--force", "--filter", "until=168h", "--keep-storage", "2GB"}, []string{"BUILDX_CONFIG=" + filepath.Join(e.Root, "buildx")}, out)
}

func (e *Engine) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		err := e.cleanupBuilds(cleanupCtx, os.Stderr)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Cove build cleanup failed:", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
