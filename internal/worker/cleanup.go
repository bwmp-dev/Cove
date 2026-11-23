package worker

import (
	"bytes"
	"context"
	"cove/internal/outbound"
	"crypto/rand"
	"crypto/sha256"
	"encoding/csv"
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

func (e *Engine) buildImages(ctx context.Context, dir string, args, env []string, out io.Writer, noCache bool) error {
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
	buildEnv := append(append([]string{}, env...), "BUILDX_CONFIG="+filepath.Join(e.Root, "buildx"), "BUILDKIT_PROGRESS=plain")
	proxy := outbound.Environment()
	if saved, err := readOutboundConfig(e.Root); err != nil {
		return err
	} else if saved != nil {
		proxy = *saved
	}
	for _, variable := range env {
		key, value, ok := strings.Cut(variable, "=")
		if !ok {
			continue
		}
		switch key {
		case "HTTP_PROXY":
			proxy.HTTP = value
		case "HTTPS_PROXY":
			proxy.HTTPS = value
		case "NO_PROXY":
			proxy.NoProxy = value
		}
	}
	if err := proxy.Validate(); err != nil {
		return err
	}
	if log, ok := out.(*boundedLog); ok {
		log.secrets = append(log.secrets, proxy.Secrets()...)
	}
	encoded, _ := json.Marshal(proxy)
	digest := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(digest[:])
	marker := filepath.Join(e.Root, "builder-ready.json")
	data, err := os.ReadFile(marker)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var previous string
	_ = json.Unmarshal(data, &previous)
	ready := previous == fingerprint || string(data) == "true" && proxy.HTTP == "" && proxy.HTTPS == "" && proxy.NoProxy == ""
	if !ready {
		if len(data) > 0 {
			if err := e.Run(ctx, dir, []string{"docker", "buildx", "rm", "--force", "cove-" + owner}, buildEnv, out); err != nil {
				return errors.New("could not refresh the private builder's outbound proxy")
			}
		}
		create := []string{"docker", "buildx", "create", "--name", "cove-" + owner, "--driver", "docker-container", "--driver-opt", "default-load=true"}
		for _, variable := range proxy.Env() {
			var encoded bytes.Buffer
			writer := csv.NewWriter(&encoded)
			if err := writer.Write([]string{"env." + variable}); err != nil {
				return err
			}
			writer.Flush()
			if err := writer.Error(); err != nil {
				return err
			}
			create = append(create, "--driver-opt", strings.TrimSuffix(encoded.String(), "\n"))
		}
		if err := e.Run(ctx, dir, create, buildEnv, out); err != nil {
			// Creation can survive a crash before its ready marker; replace only our private configuration's named builder.
			inspected := &limitedBuffer{Limit: 4096}
			inspectErr := e.Run(ctx, dir, []string{"docker", "buildx", "inspect", "cove-" + owner, "--format", "{{.Driver}}"}, buildEnv, inspected)
			if inspectErr != nil || strings.TrimSpace(inspected.data.String()) != "docker-container" {
				return err
			}
			if removeErr := e.Run(ctx, dir, []string{"docker", "buildx", "rm", "--force", "cove-" + owner}, buildEnv, out); removeErr != nil {
				return errors.Join(err, removeErr)
			}
			if createErr := e.Run(ctx, dir, create, buildEnv, out); createErr != nil {
				return createErr
			}
		}
		if err := writeJSON(marker, fingerprint); err != nil {
			return err
		}
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
	if secretBuild || noCache {
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
		if err := e.pruneIdleOperations(); err != nil {
			fmt.Fprintln(os.Stderr, "Cove idle journal cleanup failed:", err)
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
