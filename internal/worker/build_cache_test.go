package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cacheStateFixture(t *testing.T) (*Engine, buildCacheState) {
	t.Helper()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := e.buildOwner()
	if err != nil {
		t.Fatal(err)
	}
	if err = e.protectBuildOwner(owner, true); err != nil {
		t.Fatal(err)
	}
	return e, buildCacheState{Current: &cacheLocation{Path: t.TempDir(), Owner: owner, Image: buildCacheImage, Docker: cacheDaemon{ID: "fixture-original", Root: t.TempDir()}, Filesystem: cacheFilesystem{Filesystem: "fixture", Capacity: 64 * 1024 * 1024}}}
}

func TestBuildCacheProtectionRefusesOldOwnerReader(t *testing.T) {
	e, state := cacheStateFixture(t)
	data, err := os.ReadFile(filepath.Join(e.Root, "build-owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	var oldOwner string
	if json.Unmarshal(data, &oldOwner) == nil {
		t.Fatal("older worker could create an uncapped builder")
	}
	owner, protected, err := decodeBuildOwner(data)
	if err != nil || !protected || owner != state.Current.Owner {
		t.Fatal(owner, protected, err)
	}
	if err = e.protectBuildOwner(owner, false); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(e.Root, "build-owner.json"))
	if err != nil || json.Unmarshal(data, &oldOwner) != nil || oldOwner != owner {
		t.Fatal("explicit disable did not restore the ordinary ownership shape", err)
	}
}

func TestBuildCacheUnverifiableConfigurationNeverFallsBack(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://fixture.invalid:2375")
	t.Setenv("DOCKER_CONTEXT", "")
	e, state := cacheStateFixture(t)
	if err := writeJSON(filepath.Join(e.Root, "build-cache.json"), state); err != nil {
		t.Fatal(err)
	}
	var commands []string
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		command := strings.Join(args, " ")
		commands = append(commands, command)
		if strings.Contains(command, "config --format json") {
			_, err := io.WriteString(out, `{"services":{"app":{"build":{"context":"."}}}}`)
			return err
		}
		return errors.New("unexpected Docker mutation")
	}
	if err := e.buildImages(context.Background(), e.Root, []string{"docker", "compose", "-f", "fixture.yaml"}, nil, io.Discard, false); err == nil {
		t.Fatal("unverified cache accepted")
	}
	if len(commands) != 1 || strings.Contains(commands[0], "buildx") {
		t.Fatal("fell through to an uncapped builder", commands)
	}
	if _, err := os.Stat(filepath.Join(e.Root, "builder-ready.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created uncapped readiness marker", err)
	}
	status := e.BuildCacheStatus(context.Background())
	if status.Verified || status.CapacityBytes != 0 {
		t.Fatal("unverifiable storage reported a cap", status)
	}
}

func TestBuildCacheRetirementAndPruneStayCheckpointed(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://fixture.invalid:2375")
	t.Setenv("DOCKER_CONTEXT", "")
	for _, retirement := range []bool{false, true} {
		e, state := cacheStateFixture(t)
		if retirement {
			state.Retired, state.Current = []cacheLocation{*state.Current}, nil
		} else {
			state.Current.Prune = true
		}
		if err := writeJSON(filepath.Join(e.Root, "build-cache.json"), state); err != nil {
			t.Fatal(err)
		}
		e.Run = func(context.Context, string, []string, []string, io.Writer) error {
			t.Fatal("a different daemon was used for cleanup")
			return nil
		}
		if _, err := e.recoverBuildCache(context.Background(), io.Discard, false); err == nil {
			t.Fatal("lost unsettled cleanup")
		}
		got, err := e.readBuildCache()
		if err != nil || retirement && len(got.Retired) != 1 || !retirement && (got.Current == nil || !got.Current.Prune) {
			t.Fatal(got, err)
		}
	}
}

func TestBuildCacheIncompleteConfigurationBlocksBuilds(t *testing.T) {
	e, _ := cacheStateFixture(t)
	if _, err := e.recoverBuildCache(context.Background(), io.Discard, false); err == nil {
		t.Fatal("protected owner without a committed configuration fell back")
	}
}

func TestBuildCacheConfigureWaitsForBuildAndPrune(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.buildMu.Lock()
	defer e.buildMu.Unlock()
	if _, err = e.ConfigureBuildCache(context.Background(), BuildCacheSettings{}); err == nil {
		t.Fatal("configuration overlapped a build")
	}
	if _, err = os.Stat(filepath.Join(e.Root, "build-cache.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("busy configuration mutated saved settings", err)
	}
}

func TestBuildCacheDefaultDoesNotReportGarbageCollectionAsACap(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	status := e.BuildCacheStatus(context.Background())
	if status.Configured || status.Verified || status.CapacityBytes != 0 {
		t.Fatal(status)
	}
}

func TestBuildCacheProtectedOwnerAllowsBuildCleanup(t *testing.T) {
	e, state := cacheStateFixture(t)
	r := Request{ID: "capped-build", Workload: "app", Action: "build", Compose: "services: {}", Build: &BuildOptions{Repository: "fixture/app"}, Registry: &Registry{Server: "registry.example.test", Username: "fixture", Password: "fixture"}}
	if _, err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "pending-build.json")
	if err := writeJSON(marker, pendingBuild{Operation: r.ID, Owner: state.Current.Owner, Services: []string{"app"}}); err != nil {
		t.Fatal(err)
	}
	e.Run = func(context.Context, string, []string, []string, io.Writer) error { return nil }
	if err := e.recoverBuild(context.Background(), r.Workload); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cleanup did not settle", err)
	}
}

func TestBuildCacheRegistrationRejectsOtherEndpointsAndDrivers(t *testing.T) {
	e, state := cacheStateFixture(t)
	name := cacheBuilderName(*state.Current)
	path := filepath.Join(e.Root, "buildx", "instances", name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ driver, endpoint, load string }{{"remote", "docker-container://" + name, "true"}, {"docker", "default", "true"}, {"remote", "docker-container://foreign", "true"}, {"remote", "docker-container://" + name, "false"}} {
		if err := writeJSON(path, map[string]any{"Name": name, "Driver": test.driver, "Nodes": []any{map[string]any{"Endpoint": test.endpoint, "DriverOpts": map[string]string{"default-load": test.load}}}}); err != nil {
			t.Fatal(err)
		}
		err := e.cacheBuildx(context.Background(), *state.Current, io.Discard)
		valid := test.driver == "remote" && test.endpoint == "docker-container://"+name && test.load == "true"
		if (err == nil) != valid {
			t.Fatal(test, err)
		}
	}
}

func TestBuildCacheDaemonViewRequiresTheSameFilesystem(t *testing.T) {
	e, state := cacheStateFixture(t)
	for _, matches := range []bool{false, true} {
		e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
			if len(args) > 1 && args[1] == "run" {
				command := strings.Join(args, " ")
				for _, required := range []string{"--network none", "--read-only", "--cap-drop ALL", "--memory 32m", "--pids-limit 16", "target=/state,readonly"} {
					if !strings.Contains(command, required) {
						t.Fatal("filesystem helper not confined", command)
					}
				}
				actual := state.Current.Filesystem
				if !matches {
					actual.Filesystem = "different-daemon-filesystem"
				}
				return json.NewEncoder(out).Encode(actual)
			}
			return nil
		}
		err := e.verifyCacheDaemonView(context.Background(), *state.Current)
		if (err == nil) != matches {
			t.Fatal(matches, err)
		}
	}
}

func TestBuildCacheProbeCleanupPreservesForeignContainers(t *testing.T) {
	e, state := cacheStateFixture(t)
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		switch args[1] {
		case "ps":
			_, err := io.WriteString(out, strings.Repeat("a", 64))
			return err
		case "inspect":
			return json.NewEncoder(out).Encode(map[string]any{"Name": "/cove-cache-probe-" + state.Current.Owner, "Config": map[string]any{"Labels": map[string]string{"cove.owner": "foreign"}}})
		default:
			t.Fatal("foreign probe was mutated", args)
			return nil
		}
	}
	if err := e.removeCacheProbe(context.Background(), state.Current.Owner); err == nil {
		t.Fatal("foreign resource accepted for cleanup")
	}
}
