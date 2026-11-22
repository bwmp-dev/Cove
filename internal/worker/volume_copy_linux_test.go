package worker

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestVolumeCopyPreservesTreeAndLeavesSourceUnchanged(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(from, "uploads", "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, "uploads", "a.txt"), []byte("alpha"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, "uploads", "nested", "b.bin"), bytes.Repeat([]byte{7}, 70000), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(from, "uploads", "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(from, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(from, "uploads", "nested"), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(from, "uploads", "nested"), 0700)
		_ = os.Chmod(filepath.Join(to, "uploads", "nested"), 0700)
	})
	before, err := digestTree(from)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = VolumeCopy(from, to, &out); err != nil {
		t.Fatal(err)
	}
	var result volumeCopyResult
	if err = json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	after, err := digestTree(from)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := digestTree(to)
	if err != nil {
		t.Fatal(err)
	}
	if before.Digest != after.Digest || copied.Digest != before.Digest || result.Digest != before.Digest || result.Files != 3 || result.Bytes != 70005 {
		t.Fatalf("copy mismatch: before %v after %v copied %v result %v", before, after, copied, result)
	}
	info, err := os.Stat(filepath.Join(to, "uploads", "nested"))
	if err != nil || info.Mode().Perm() != 0500 {
		t.Fatalf("directory mode not preserved: %v %v", info, err)
	}
	if target, err := os.Readlink(filepath.Join(to, "uploads", "link")); err != nil || target != "a.txt" {
		t.Fatal("symbolic link not preserved")
	}
}

func TestVolumeCopyRefusesDevices(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("creating a device node requires root")
	}
	from := t.TempDir()
	if err := syscall.Mknod(filepath.Join(from, "null"), syscall.S_IFCHR|0600, 0x103); err != nil {
		t.Skip("device nodes unavailable:", err)
	}
	if err := VolumeCopy(from, t.TempDir(), &bytes.Buffer{}); err == nil {
		t.Fatal("device file was copied")
	}
}
