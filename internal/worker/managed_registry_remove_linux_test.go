package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestManagedRegistryRemovalPermissionsOnOwnedLinux(t *testing.T) {
	if os.Getenv("COVE_TEST_DELETION_NONROOT") != "1" || os.Geteuid() != 0 {
		t.Skip("set COVE_TEST_DELETION_NONROOT=1 in an owned root Linux fixture to launch unprivileged registry removal")
	}
	root, err := os.MkdirTemp("", "cove-registry-removal-nonroot-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	for _, path := range []string{root, filepath.Join(root, "workloads"), filepath.Join(root, "workloads", strings.Repeat("a", 32))} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, 65534, 65534); err != nil {
			t.Fatal(err)
		}
	}
	blocked := filepath.Join(root, "workloads", strings.Repeat("a", 32), "root-created")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "data"), []byte("container-owned data"), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), binary, "-test.v", "-test.run=^TestManagedRegistryRemovalCleansWorkerState$/^keep-data$", "-test.timeout=30s")
	command.Env = append(os.Environ(), "COVE_TEST_REGISTRY_REMOVAL_NONROOT_ROOT="+root)
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("unprivileged registry cleanup failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "--- PASS: TestManagedRegistryRemovalCleansWorkerState/keep-data") {
		t.Fatal("unprivileged registry removal did not execute", string(output))
	}
	if content, err := os.ReadFile(filepath.Join(blocked, "data")); err != nil || string(content) != "container-owned data" {
		t.Fatal("container-owned files changed", err)
	}
	t.Log("UID 65534 registry removal retained root-owned 0700 files with a warning and removed worker-owned auth files")
}
