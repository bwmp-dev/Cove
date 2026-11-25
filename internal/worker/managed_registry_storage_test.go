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
	"strings"
	"testing"
	"time"
)

func registryFixtureBlob(t *testing.T, root string, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	name := filepath.Join(root, "docker/registry/v2/blobs/sha256", digest[:2], digest, "data")
	if err = os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	return "sha256:" + digest
}

func registryFixtureManifest(t *testing.T, root, tag string, age int) string {
	t.Helper()
	config := registryFixtureBlob(t, root, map[string]any{"created": time.Now().UTC().Add(-time.Duration(age) * 24 * time.Hour), "config": map[string]any{"Env": []string{"MUST_NEVER_RETURN=fixture"}}})
	manifest := registryFixtureBlob(t, root, map[string]any{"schemaVersion": 2, "config": map[string]string{"digest": config}, "layers": []any{}})
	for _, relative := range []string{"revisions/sha256/" + strings.TrimPrefix(manifest, "sha256:") + "/link", "tags/" + tag + "/current/link"} {
		name := filepath.Join(root, "docker/registry/v2/repositories/team/app/_manifests", relative)
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(manifest), 0600); err != nil {
			t.Fatal(err)
		}
		aged := time.Now().UTC().Add(-time.Duration(age) * 24 * time.Hour)
		if err := os.Chtimes(name, aged, aged); err != nil {
			t.Fatal(err)
		}
	}
	return manifest
}

func TestManagedRegistryStorageInventoryDeletesOnlyEligibleCoveLinks(t *testing.T) {
	root := t.TempDir()
	latest := registryFixtureManifest(t, root, registryBuildFixtureTag("1"), 1)
	expired := registryFixtureManifest(t, root, registryBuildFixtureTag("2"), 30)
	protected := registryFixtureManifest(t, root, registryBuildFixtureTag("3"), 30)
	foreign := registryFixtureManifest(t, root, "stable", 30)
	input := ManagedRegistryStorageRequest{Policy: ManagedRegistryRetention{KeepPerRepository: 1, MinimumAgeDays: 7}, Protected: []string{"team/app@" + protected}, Now: time.Now().UTC(), Delete: true}
	data, _ := json.Marshal(input)
	var output bytes.Buffer
	if err := RunManagedRegistryStorage(root, bytes.NewReader(data), &output); err != nil {
		t.Fatal(err)
	}
	var result ManagedRegistryStorageResult
	if json.Unmarshal(output.Bytes(), &result) != nil || result.Manifests != 4 || result.Deleted != 1 {
		t.Fatal(output.String())
	}
	if strings.Contains(output.String(), "MUST_NEVER_RETURN") || strings.Contains(output.String(), "Env") {
		t.Fatal("image metadata exposed")
	}
	for _, digest := range []string{latest, protected, foreign} {
		if _, err := os.Stat(filepath.Join(root, "docker/registry/v2/repositories/team/app/_manifests/revisions/sha256", strings.TrimPrefix(digest, "sha256:"), "link")); err != nil {
			t.Fatal("protected manifest removed", err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "docker/registry/v2/repositories/team/app/_manifests/revisions/sha256", strings.TrimPrefix(expired, "sha256:"), "link")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("eligible manifest link retained", err)
	}
	if _, err := os.Stat(filepath.Join(root, "docker/registry/v2/repositories/team/app/_manifests/tags/"+registryBuildFixtureTag("2"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("eligible tag retained", err)
	}
}

func TestManagedRegistryStorageRefusesIncompleteInventoryBeforeMutation(t *testing.T) {
	root := t.TempDir()
	digest := registryFixtureManifest(t, root, registryBuildFixtureTag("1"), 30)
	input := ManagedRegistryStorageRequest{Policy: ManagedRegistryRetention{KeepPerRepository: 1, MinimumAgeDays: 7}, Protected: []string{"team/app@sha256:" + strings.Repeat("a", 64)}, Now: time.Now().UTC(), Delete: true}
	data, _ := json.Marshal(input)
	if err := RunManagedRegistryStorage(root, bytes.NewReader(data), io.Discard); err == nil {
		t.Fatal("missing protected reference accepted")
	}
	name := filepath.Join(root, "docker/registry/v2/blobs/sha256", strings.TrimPrefix(digest, "sha256:")[:2], strings.TrimPrefix(digest, "sha256:"), "data")
	if err := os.WriteFile(name, []byte(`{"schemaVersion":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	input.Protected = nil
	data, _ = json.Marshal(input)
	if err := RunManagedRegistryStorage(root, bytes.NewReader(data), io.Discard); err == nil {
		t.Fatal("manifest digest mismatch accepted")
	}
}

func TestManagedRegistryStorageInitializesEmptyNamespaceForGC(t *testing.T) {
	root := t.TempDir()
	input := ManagedRegistryStorageRequest{Policy: ManagedRegistryRetention{KeepPerRepository: 10, MinimumAgeDays: 30}, Now: time.Now().UTC(), Delete: true}
	data, _ := json.Marshal(input)
	var output bytes.Buffer
	if err := RunManagedRegistryStorage(root, bytes.NewReader(data), &output); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"docker/registry/v2/repositories", "docker/registry/v2/blobs/sha256"} {
		if info, err := os.Stat(filepath.Join(root, name)); err != nil || !info.IsDir() {
			t.Fatal("empty GC namespace unavailable", name, err)
		}
	}
	var result ManagedRegistryStorageResult
	if json.Unmarshal(output.Bytes(), &result) != nil || result.Manifests != 0 || result.Deleted != 0 {
		t.Fatal(output.String())
	}
}

func TestManagedRegistryRetentionUsesPublicationTimeInsteadOfImageCreated(t *testing.T) {
	root := t.TempDir()
	freshTag := registryBuildFixtureTag("1")
	oldTag := registryBuildFixtureTag("2")
	fresh := registryFixtureManifest(t, root, freshTag, 100)
	old := registryFixtureManifest(t, root, oldTag, 0)
	for _, item := range []struct {
		digest, tag string
		at          time.Time
	}{{fresh, freshTag, time.Now().UTC()}, {old, oldTag, time.Now().UTC().Add(-40 * 24 * time.Hour)}} {
		for _, relative := range []string{"revisions/sha256/" + strings.TrimPrefix(item.digest, "sha256:") + "/link", "tags/" + item.tag + "/current/link"} {
			if err := os.Chtimes(filepath.Join(root, "docker/registry/v2/repositories/team/app/_manifests", relative), item.at, item.at); err != nil {
				t.Fatal(err)
			}
		}
	}
	input := ManagedRegistryStorageRequest{Policy: ManagedRegistryRetention{KeepPerRepository: 1, MinimumAgeDays: 7}, Now: time.Now().UTC(), Delete: true}
	data, _ := json.Marshal(input)
	if err := RunManagedRegistryStorage(root, bytes.NewReader(data), io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "docker/registry/v2/repositories/team/app/_manifests/tags", freshTag)); err != nil {
		t.Fatal("old image timestamp aged a freshly published manifest", err)
	}
	if _, err := os.Stat(filepath.Join(root, "docker/registry/v2/repositories/team/app/_manifests/tags", oldTag)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("image metadata overrode the expired publication time", err)
	}
}

func TestManagedRegistryRecoveryRetainsIntentUntilRestartConfirmed(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "registry-gc", Workload: strings.Repeat("a", 32), Action: "registry_maintenance", Kind: "service", ManagedRegistry: &ManagedRegistryOperation{Mode: "gc", Policy: ManagedRegistryRetention{KeepPerRepository: 1, MinimumAgeDays: 7}}}
	if _, err = e.Submit(r); err != nil {
		t.Fatal(err)
	}
	j, _ := e.read(r.ID)
	j.Operation.State = "failed"
	j.RegistryCleanup = &managedRegistryCleanup{Container: strings.Repeat("b", 64), Restart: "unless-stopped", Running: true}
	if err = writeJSON(e.path(r.ID), j); err != nil {
		t.Fatal(err)
	}
	failed := true
	starts := 0
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		switch args[1] {
		case "ps":
			return nil
		case "update":
			return nil
		case "inspect":
			_, err := io.WriteString(out, `"healthy"`)
			return err
		case "start":
			starts++
			if failed {
				return errors.New("fixture start failed")
			}
			return nil
		default:
			t.Fatal(args)
			return nil
		}
	}
	if err = e.Recover(); err == nil {
		t.Fatal("failed restoration cleared maintenance")
	}
	status, err := e.Status(r.ID)
	if err != nil || !status.RegistryRecoveryPending {
		t.Fatal(status, err)
	}
	failed = false
	if err = e.Recover(); err != nil {
		t.Fatal(err)
	}
	status, err = e.Status(r.ID)
	if err != nil || status.RegistryRecoveryPending || starts != 2 {
		t.Fatal(status, starts, err)
	}
}

func TestManagedRegistryRecoveryRetiresANeverAcknowledgedSubmission(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		t.Fatal("unsubmitted maintenance changed Docker resources")
		return nil
	}
	r := Request{ID: "lost-registry-submit", Workload: strings.Repeat("a", 32), Action: "registry_maintenance", Kind: "service", ManagedRegistry: &ManagedRegistryOperation{Mode: "gc", Policy: ManagedRegistryRetention{KeepPerRepository: 1, MinimumAgeDays: 7}}}
	response := e.RPC(context.Background(), RPCRequest{Method: "managed_registry_recover", ID: r.ID, Request: &r})
	if response.Error != "" || response.Operation == nil || response.Operation.State != "cancelled" {
		t.Fatal(response)
	}
	late, err := e.Submit(r)
	if err != nil || late.State != "cancelled" {
		t.Fatal("late delivery could execute retired maintenance", late, err)
	}
}
