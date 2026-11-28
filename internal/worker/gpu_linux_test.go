package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGPUCDIAssignmentOnOwnedLinuxVM(t *testing.T) {
	binary := os.Getenv("COVE_TEST_WORKER_BINARY")
	if os.Getenv("COVE_TEST_OWNED_HOST_INSPECTION") != "1" || os.Geteuid() != 0 || binary == "" {
		t.Skip("explicit owned disposable Linux Docker host and worker binary required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.hostHelperBinary = binary
	owner, err := e.backupHelperOwner(true)
	if err != nil {
		t.Fatal(err)
	}
	if receipt := os.Getenv("COVE_TEST_GPU_RECEIPT"); receipt != "" {
		if err := writeJSON(receipt, map[string]string{"root": e.Root, "owner": owner}); err != nil {
			t.Fatal(err)
		}
	}
	created := false
	if err = os.Mkdir("/etc/cdi", 0755); err == nil {
		created = true
	} else if !os.IsExist(err) {
		t.Fatal(err)
	}
	if created {
		t.Cleanup(func() {
			if err := os.Remove("/etc/cdi"); err != nil {
				t.Error(err)
			}
		})
	}
	id := "cove.test/gpu=" + owner
	filename := filepath.Join("/etc/cdi", "cove-test-"+owner+".json")
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(filename); err != nil {
			t.Error(err)
		}
	})
	data := map[string]any{"cdiVersion": "0.6.0", "kind": "cove.test/gpu", "devices": []any{map[string]any{"name": owner, "containerEdits": map[string]any{"env": []string{"COVE_TEST_GPU=" + owner}}}}}
	if err = json.NewEncoder(file).Encode(data); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := e.GPUs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	patches, err := e.HostMaintenance(ctx, HostMaintenanceRequest{Action: "inspect"})
	if err != nil || patches == nil || patches.Manager != "apt" || patches.SecurityUpdates == nil {
		t.Fatal("real native host-maintenance transport failed", patches, err)
	}
	firewall, err := e.ConfigureFirewall(ctx, FirewallControl{Action: "inspect", Reserved: []uint16{22, 80, 443}})
	if err != nil || firewall == nil || !firewall.Supported {
		t.Fatal("real native firewall inspection transport failed", firewall, err)
	}
	found := false
	for _, device := range report.Devices {
		found = found || device.ID == id && device.Assignable
	}
	if !found {
		t.Fatal("real Docker CDI driver or the root-owned device specification was not detected", report)
	}
	compose := filepath.Join(e.Root, "compose.yaml")
	if err = os.WriteFile(compose, []byte("services:\n  app:\n    image: busybox:1.37\n    command: [sleep, '120']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	selection := []string{id}
	project := "cove-gpu-test-" + owner
	args := []string{"docker", "compose", "-p", project, "-f", compose}
	prepared, err := e.prepareLimits(ctx, Request{ID: "gpu-config", Limits: map[string]ServiceLimits{"app": {GPUs: &selection}}}, e.Root, args, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	args = []string{"docker", "compose", "-p", project, "-f", prepared}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := e.Run(cleanup, e.Root, append(append([]string{}, args...), "down"), nil, io.Discard); err != nil {
			t.Error(err)
		}
	})
	if err = e.Run(ctx, e.Root, append(append([]string{}, args...), "up", "-d", "--wait"), nil, os.Stderr); err != nil {
		t.Fatal(err)
	}
	container, err := e.output(ctx, "docker", "ps", "-q", "--filter", "label=com.docker.compose.project="+project)
	if err != nil {
		t.Fatal(err)
	}
	value, err := e.output(ctx, "docker", "exec", strings.TrimSpace(container), "cat", "/proc/1/environ")
	if err != nil || !slices.Contains(strings.Split(value, "\x00"), "COVE_TEST_GPU="+owner) {
		t.Fatal("CDI edits were not applied by Docker", err)
	}
	missing := []string{"cove.test/gpu=absent"}
	if err = e.validateGPUs(ctx, map[string]ServiceLimits{"app": {GPUs: &missing}}); err == nil {
		t.Fatal("absent GPU device was admitted")
	}
	t.Log("Root-owned synthetic CDI GPU inventory and actual Docker device assignment passed; this verifies transport/reservation, not physical GPU computation.")
}
