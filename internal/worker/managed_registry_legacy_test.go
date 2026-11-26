package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// This is the release-states pair and operation journal written by f53333c:
// image is a local ID (or the original image on a disabled preview service),
// with no dev.cove.image-reference label or separate image-identity record.
func legacyRegistryRelease(t *testing.T, e *Engine, workload, id, compose string, finished time.Time) {
	t.Helper()
	dir := filepath.Join(e.Root, "workloads", workload, "release-states")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".compose.json")
	if err := os.WriteFile(path, []byte(compose), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, id+".json"), map[string]any{"compose_path": path, "environment": map[string]string{}, "revision": "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(e.path(id), map[string]any{"operation": map[string]any{"id": id, "workload": workload, "action": "deploy", "state": "succeeded", "created_at": finished.Add(-time.Minute), "finished_at": finished}, "request": map[string]any{"id": id, "workload": workload, "action": "deploy", "kind": "service"}}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedRegistryLegacyReleaseReferences(t *testing.T) {
	testLegacyRegistryReleaseReferences(t, false)
}

func testLegacyRegistryReleaseReferences(t *testing.T, actualDocker bool) {
	created := time.Now().UTC().Add(-time.Hour)
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, test := range []struct {
		name, compose string
		newer         bool
		want          []string
		refused       bool
	}{
		{name: "other-registry", compose: `{"services":{"app":{"image":"other.example.test/app:legacy","scale":0}}}`},
		{name: "this-registry", compose: `{"services":{"app":{"image":"registry.example.test/app@` + digest + `","scale":0}}}`, want: []string{"app@" + digest}},
		{name: "this-registry-uppercase-host", compose: `{"services":{"app":{"image":"REGISTRY.example.test/app@` + digest + `","scale":0}}}`, want: []string{"app@" + digest}},
		{name: "named-mutable-old-is-not-exempt", compose: `{"services":{"app":{"image":"registry.example.test/app:legacy","scale":0}}}`, refused: true},
		{name: "unreadable-older", compose: `{"services":`},
		{name: "unreadable-newer", compose: `{"services":`, newer: true, refused: true},
		{name: "unresolved-older", compose: `{"services":{"app":{"image":"${REGISTRY}/app:legacy"}}}`},
		{name: "unresolved-newer", compose: `{"services":{"app":{"image":"${REGISTRY}/app:legacy"}}}`, newer: true, refused: true},
		{name: "missing-older", compose: `{"services":{"disabled":{"scale":0,"build":{"context":"/app"}}}}`},
		{name: "missing-newer", compose: `{"services":{"disabled":{"scale":0,"build":{"context":"/app"}}}}`, newer: true, refused: true},
		{name: "known-reference-with-older-missing", compose: `{"services":{"app":{"image":"registry.example.test/app@` + digest + `"},"disabled":{"scale":0}}}`, want: []string{"app@" + digest}},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if !actualDocker {
				e.Run = func(context.Context, string, []string, []string, io.Writer) error {
					return errors.New("must not inspect a missing or explicitly named image")
				}
			}
			finished := created.Add(-time.Hour)
			if test.newer {
				finished = created.Add(time.Minute)
			}
			legacyRegistryRelease(t, e, "legacy", "release", test.compose, finished)
			var previous []string
			if !test.newer {
				previous = []string{"release"}
			}
			response := e.RPC(t.Context(), RPCRequest{Method: "managed_registry_references", ID: "legacy", Registry: &Registry{Server: "registry.example.test"}, RegistryPrevious: previous})
			if test.refused {
				if response.Error == "" {
					t.Fatal("uninspectable newer release was accepted")
				}
				return
			}
			if response.Error != "" || !slices.Equal(response.RegistryReferences, test.want) {
				t.Fatal("legacy release inventory", response)
			}
		})
	}
}

func TestManagedRegistryLegacyProvenanceCannotBeGuessed(t *testing.T) {
	for _, scenario := range []string{"missing-proof", "wrong-release", "missing-snapshot", "duplicate-proof", "invalid-proof", "unbounded-proof"} {
		t.Run(scenario, func(t *testing.T) {
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			legacyRegistryRelease(t, e, "legacy", "release", `{`, time.Now().Add(-365*24*time.Hour))
			var previous []string
			switch scenario {
			case "wrong-release":
				previous = []string{"other-release"}
			case "missing-snapshot":
				if err = os.Remove(filepath.Join(e.Root, "workloads", "legacy", "release-states", "release.compose.json")); err != nil {
					t.Fatal(err)
				}
			case "duplicate-proof":
				previous = []string{"release", "release"}
			case "invalid-proof":
				previous = []string{"../release"}
			case "unbounded-proof":
				previous = make([]string, 2049)
			}
			response := e.RPC(t.Context(), RPCRequest{Method: "managed_registry_references", ID: "legacy", Registry: &Registry{Server: "registry.example.test"}, RegistryPrevious: previous})
			if response.Error == "" {
				t.Fatal("unproven age was accepted", scenario)
			}
		})
	}
}

func TestManagedRegistryLegacyReleaseAliases(t *testing.T) {
	for _, scenario := range []string{"rollback", "promotion-snapshot", "older-missing-target", "unproven-missing-target", "foreign-path"} {
		t.Run(scenario, func(t *testing.T) {
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			digest := "sha256:" + strings.Repeat("b", 64)
			legacyRegistryRelease(t, e, "legacy", "original", `{"services":{"app":{"image":"registry.example.test/app@`+digest+`"}}}`, time.Now().Add(-24*time.Hour))
			dir := filepath.Join(e.Root, "workloads", "legacy")
			path := filepath.Join(dir, "release-states", "original.compose.json")
			if scenario == "promotion-snapshot" {
				next := filepath.Join(dir, "promoted.compose.json")
				if err = os.Rename(path, next); err != nil {
					t.Fatal(err)
				}
				path = next
				if err = os.Remove(filepath.Join(dir, "release-states", "original.json")); err != nil {
					t.Fatal(err)
				}
			}
			var previous []string
			if scenario == "older-missing-target" || scenario == "unproven-missing-target" {
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if scenario == "older-missing-target" {
					previous = []string{"original"}
				}
			}
			if scenario == "foreign-path" {
				path = filepath.Join(e.Root, "workloads", "other", "original.compose.json")
			}
			if err = writeJSON(filepath.Join(dir, "release-states", "rollback.json"), map[string]string{"compose_path": path}); err != nil {
				t.Fatal(err)
			}
			response := e.RPC(t.Context(), RPCRequest{Method: "managed_registry_references", ID: "legacy", Registry: &Registry{Server: "registry.example.test"}, RegistryPrevious: previous})
			if scenario == "foreign-path" || scenario == "unproven-missing-target" {
				if response.Error == "" {
					t.Fatal("uninspectable alias accepted", response)
				}
				return
			}
			if response.Error != "" {
				t.Fatal("valid retained alias refused", response.Error)
			}
			if scenario != "older-missing-target" && !slices.Equal(response.RegistryReferences, []string{"app@" + digest}) {
				t.Fatal("retained alias lost its image reference", response.RegistryReferences)
			}
		})
	}
}

func TestManagedRegistryLegacyImageInspectionIsShared(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	image := "sha256:" + strings.Repeat("a", 64)
	digest := "sha256:" + strings.Repeat("b", 64)
	for _, id := range []string{"first", "second"} {
		legacyRegistryRelease(t, e, "legacy", id, `{"services":{"app":{"image":"`+image+`"}}}`, time.Now())
	}
	inspections := 0
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		if !slices.Equal(args, []string{"docker", "image", "inspect", "--format", "{{json .}}", image}) {
			t.Fatal("unexpected inventory command", args)
		}
		inspections++
		return json.NewEncoder(out).Encode(map[string]any{"RepoDigests": []string{"registry.example.test/app@" + digest}})
	}
	refs, err := e.ManagedRegistryReferences(t.Context(), "legacy", "registry.example.test", nil)
	if err != nil || inspections != 1 || !slices.Equal(refs, []string{"app@" + digest}) {
		t.Fatal("repeated image inventory", inspections, refs, err)
	}
}

func TestManagedRegistryLegacyReleaseDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_MANAGED_REGISTRY_DOCKER") != "1" || runtime.GOOS != "linux" {
		t.Skip("owned isolated Docker fixture required")
	}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var image struct {
		ID string `json:"Id"`
	}
	if err = e.imageJSON(t.Context(), nil, &image, "docker", "image", "inspect", "--format", "{{json .}}", "busybox:1.37"); err != nil || !imageDigest.MatchString(image.ID) {
		t.Fatal("cached fixture image required", err)
	}
	created := time.Now().UTC().Add(-time.Hour)
	legacyRegistryRelease(t, e, "legacy", "local-image", `{"services":{"app":{"image":"`+image.ID+`","pull_policy":"never"}}}`, created.Add(-time.Hour))
	refs, err := e.ManagedRegistryReferences(t.Context(), "legacy", "registry.example.test", nil)
	if err != nil || len(refs) != 0 {
		t.Fatal("real legacy local image", refs, err)
	}
	// Exercise the old on-disk layout through the actual RPC dispatcher too.
	t.Run("legacy-snapshot-cases", func(t *testing.T) { testLegacyRegistryReleaseReferences(t, true) })
	state, err := os.ReadFile(filepath.Join(e.Root, "workloads", "legacy", "release-states", "local-image.json"))
	var fields map[string]json.RawMessage
	if err != nil || json.Unmarshal(state, &fields) != nil || len(fields) != 3 {
		t.Fatal("legacy state layout changed", err)
	}
}
