//go:build linux

package worker

import "testing"

func TestMountMetricPathsAndBounds(t *testing.T) {
	data := []byte("23 1 0:1 / / rw - ext4 /dev/root rw\n24 1 0:2 / /data\\040disk rw - xfs /dev/data rw\n25 1 0:3 / /proc rw - proc proc rw\n26 1 0:4 / / rw - ext4 /dev/root rw\n")
	mounts, limited := mountPaths(data)
	if limited || len(mounts) != 3 || mounts[1].Path != "/data disk" || mounts[1].Filesystem != "xfs" {
		t.Fatal(mounts, limited)
	}
	live, limited := inspectMounts()
	if len(live) == 0 || limited {
		t.Fatal("live mount metrics unavailable", live, limited)
	}
	for _, m := range live {
		if m.Known && m.Available > m.Total {
			t.Fatal("invalid filesystem accounting", m)
		}
	}
}
