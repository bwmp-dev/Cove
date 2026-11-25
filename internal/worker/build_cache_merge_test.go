package worker

import (
	"context"
	"cove/internal/outbound"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildCacheMergeProxyNeverLaunchesUnproxiedBuilder(t *testing.T) {
	for _, source := range []string{"environment", "saved", "request"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("HTTP_PROXY", "")
			t.Setenv("HTTPS_PROXY", "")
			t.Setenv("http_proxy", "")
			t.Setenv("https_proxy", "")
			e, state := cacheStateFixture(t)
			if err := writeJSON(filepath.Join(e.Root, "build-cache.json"), state); err != nil {
				t.Fatal(err)
			}
			proxy := "http://fixture:private-proxy-fixture@proxy.example.test:8080"
			var env []string
			switch source {
			case "environment":
				t.Setenv("HTTPS_PROXY", proxy)
			case "saved":
				if err := writeJSON(filepath.Join(e.Root, "outbound-proxy.json"), outbound.Config{HTTPS: proxy}); err != nil {
					t.Fatal(err)
				}
			case "request":
				env = []string{"HTTPS_PROXY=" + proxy}
			}
			commands := 0
			e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
				commands++
				if strings.Contains(strings.Join(args, " "), "config --format json") {
					_, err := io.WriteString(out, `{"services":{"app":{"build":{"context":"."}}}}`)
					return err
				}
				t.Fatal("capped proxy refusal reached a Docker mutation", args)
				return nil
			}
			err := e.buildImages(t.Context(), e.Root, []string{"docker", "compose"}, env, io.Discard, false)
			if err == nil || !strings.Contains(err.Error(), "HTTP outbound proxy") || strings.Contains(err.Error(), "private-proxy-fixture") || commands != 1 {
				t.Fatal("capped build bypassed proxy policy", err, commands)
			}
			if source != "request" {
				if err = e.ensureCacheBuilder(t.Context(), *state.Current, io.Discard); err == nil || commands != 1 {
					t.Fatal("recovery launched an unproxied builder", err, commands)
				}
			}
		})
	}
}

func TestBuildCacheMergeCleanupNeverUsesLegacyFallback(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://fixture.invalid:2375")
	t.Setenv("DOCKER_CONTEXT", "")
	e, state := cacheStateFixture(t)
	for _, name := range []string{"build-cache.json", "builder-ready.json"} {
		var value any = state
		if name == "builder-ready.json" {
			value = true
		}
		if err := writeJSON(filepath.Join(e.Root, name), value); err != nil {
			t.Fatal(err)
		}
	}
	e.Run = func(_ context.Context, _ string, args, _ []string, _ io.Writer) error {
		if strings.Contains(strings.Join(args, " "), "buildx") {
			t.Fatal("invalid cap fell back to a builder", args)
		}
		return nil
	}
	policy := CleanupPolicy{BuildCache: true, IntervalHours: 6, MinimumAgeHours: 168}
	if _, err := e.cleanupPlan(t.Context(), policy); err == nil {
		t.Fatal("unverifiable capped storage was treated as the legacy cache")
	}
	state.Retired, state.Current = []cacheLocation{*state.Current}, nil
	if err := writeJSON(filepath.Join(e.Root, "build-cache.json"), state); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cleanupPlan(t.Context(), policy); err == nil {
		t.Fatal("pending retirement fell back to legacy cache")
	}
}

func TestBuildCacheMergeNodeChangeAdmission(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires the Linux node-change lease")
	}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := e.lockNodeChange(true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.ConfigureBuildCache(t.Context(), BuildCacheSettings{})
	unlock()
	if err == nil || !strings.Contains(err.Error(), "another proxy, move or host maintenance change") {
		t.Fatal("cache configuration bypassed node-change lease", err)
	}
	e.hostChanging = true
	if _, err = e.ConfigureBuildCache(t.Context(), BuildCacheSettings{}); err == nil || !strings.Contains(err.Error(), "worker operations") {
		t.Fatal("cache configuration overlapped maintenance", err)
	}
	if _, err = os.Stat(filepath.Join(e.Root, "build-owner.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused configuration created ownership state", err)
	}
}

func TestCappedBuilderScheduledCleanupDocker(t *testing.T) {
	root := os.Getenv("COVE_TEST_BUILD_CACHE_ROOT")
	if runtime.GOOS != "linux" || root == "" {
		t.Skip("COVE_TEST_BUILD_CACHE_ROOT requires the owned bounded-filesystem fixture after a build")
	}
	e, err := NewEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	state, err := e.readBuildCache()
	if err != nil || state.Current == nil {
		t.Fatal("fixture has no configured cap", err)
	}
	expected := cacheBuilderName(*state.Current)
	queried := 0
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		if len(args) >= 3 && args[0] == "docker" && args[1] == "buildx" {
			if args[2] != "du" || !strings.Contains(strings.Join(args, " "), "--builder "+expected+" ") {
				t.Fatal("preview changed or queried the wrong builder", args)
			}
			queried++
		}
		return RunCommand(ctx, dir, args, env, out)
	}
	policy := CleanupPolicy{BuildCache: true, IntervalHours: 6, MinimumAgeHours: 168}
	if _, err := e.cleanupPlan(t.Context(), policy); err != nil || queried != 1 {
		t.Fatal("scheduled cleanup did not inspect the actual capped builder", err, queried)
	}
}
