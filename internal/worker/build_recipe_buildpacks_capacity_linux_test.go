//go:build linux

package worker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildpacksCgroupHeadroomUsesLimitedAncestors(t *testing.T) {
	root := t.TempDir()
	for _, entry := range []struct{ path, value string }{{"cgroup.controllers", "memory cpu"}, {"parent/memory.max", "5368709120"}, {"parent/memory.current", "2147483648"}, {"parent/docker/memory.max", "max"}} {
		path := filepath.Join(root, entry.path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(entry.value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	available, err := cnbCgroupHeadroom(root, "/parent/docker", 6<<30)
	if err != nil || available != 3<<30 {
		t.Fatal(available, err)
	}
	for _, group := range []string{"../parent", "/parent/../docker", "/missing"} {
		if _, err := cnbCgroupHeadroom(root, group, 6<<30); err == nil {
			t.Fatal("unknown cgroup headroom accepted", group)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "parent/memory.current"), []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cnbCgroupHeadroom(root, "/parent/docker", 6<<30); err == nil {
		t.Fatal("unreadable accounting accepted")
	}
}
