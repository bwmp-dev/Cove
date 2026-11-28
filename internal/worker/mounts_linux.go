//go:build linux

package worker

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"syscall"
)

func mountPaths(data []byte) ([]MountMetric, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 65536)
	out := []MountMetric{}
	seen := map[string]bool{}
	size := 0
	decode := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, "\\")
	limited := false
	for scanner.Scan() {
		left, right, ok := strings.Cut(scanner.Text(), " - ")
		if !ok {
			continue
		}
		fields, kind := strings.Fields(left), strings.Fields(right)
		if len(fields) < 5 || len(kind) < 1 {
			continue
		}
		path := decode.Replace(fields[4])
		if seen[path] {
			continue
		}
		if len(out) >= 2048 || size+len(path) > 256*1024 {
			limited = true
			break
		}
		seen[path] = true
		size += len(path)
		out = append(out, MountMetric{Path: path, Filesystem: kind[0]})
	}
	return out, limited || scanner.Err() != nil
}

func inspectMounts() ([]MountMetric, bool) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, true
	}
	mounts, limited := mountPaths(data)
	for i := range mounts {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(mounts[i].Path, &stat); err != nil {
			mounts[i].Error = "Filesystem capacity unavailable"
			continue
		}
		if stat.Blocks == 0 || stat.Bsize <= 0 {
			continue
		}
		mounts[i].Total = stat.Blocks * uint64(stat.Bsize)
		mounts[i].Available = stat.Bavail * uint64(stat.Bsize)
		mounts[i].Known = true
	}
	return mounts, limited
}
