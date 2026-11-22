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

func (e *Engine) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := e.scheduledCleanup(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "Cove scheduled cleanup failed:", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (e *Engine) scheduledCleanup(ctx context.Context) error {
	e.buildMu.Lock()
	state, err := e.cleanupState()
	e.buildMu.Unlock()
	if err != nil {
		return err
	}
	if !state.Policy.Enabled || state.LastRun != nil && time.Since(*state.LastRun) < time.Duration(state.Policy.IntervalHours)*time.Hour {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	_, plan, err := e.CleanupPolicy(cleanupCtx, CleanupControl{Action: "preview"})
	if err == nil && len(plan.Items) == 0 {
		e.buildMu.Lock()
		defer e.buildMu.Unlock()
		current, err := e.cleanupState()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		current.LastRun = &now
		current.LastError = ""
		current.Removed = 0
		current.EstimatedBytes = 0
		return writeJSON(filepath.Join(e.Root, "cleanup-policy.json"), current)
	}
	if err == nil {
		_, _, err = e.CleanupPolicy(cleanupCtx, CleanupControl{Action: "run", Revision: plan.Revision})
	}
	if err != nil {
		e.buildMu.Lock()
		defer e.buildMu.Unlock()
		current, readErr := e.cleanupState()
		if readErr != nil {
			return readErr
		}
		now := time.Now().UTC()
		current.LastRun = &now
		current.LastError = "Scheduled cleanup deferred or failed; inspect and preview before retry."
		if saveErr := writeJSON(filepath.Join(e.Root, "cleanup-policy.json"), current); saveErr != nil {
			return saveErr
		}
	}
	return err
}
