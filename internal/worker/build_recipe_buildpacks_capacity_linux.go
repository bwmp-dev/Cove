//go:build linux

package worker

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

func cnbMemoryValue(data []byte) (uint64, error) {
	if len(data) > 128 {
		return 0, errors.New("CNB cgroup memory value exceeds its bound")
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, errors.New("CNB cgroup memory accounting is unavailable")
	}
	return value, nil
}

func cnbCgroupHeadroom(root, group string, available uint64) (uint64, error) {
	if !filepath.IsAbs(group) || filepath.Clean(group) != group || strings.Contains(group, "..") {
		return 0, errors.New("CNB cannot verify the Docker daemon's cgroup path")
	}
	for depth := 0; depth < 64; depth++ {
		directory := filepath.Join(root, strings.TrimPrefix(group, "/"))
		maximum, err := os.ReadFile(filepath.Join(directory, "memory.max"))
		if group == "/" && errors.Is(err, os.ErrNotExist) {
			controllers, readErr := os.ReadFile(filepath.Join(directory, "cgroup.controllers"))
			if readErr == nil && slices.Contains(strings.Fields(string(controllers)), "memory") {
				return available, nil
			}
		}
		if err != nil {
			return 0, errors.New("CNB requires readable cgroup v2 memory accounting for Docker and its ancestors")
		}
		if strings.TrimSpace(string(maximum)) != "max" {
			limit, err := cnbMemoryValue(maximum)
			if err != nil {
				return 0, err
			}
			data, err := os.ReadFile(filepath.Join(directory, "memory.current"))
			if err != nil {
				return 0, err
			}
			used, err := cnbMemoryValue(data)
			if err != nil || used > limit {
				return 0, errors.New("CNB cgroup memory headroom is unavailable")
			}
			if limit-used < available {
				available = limit - used
			}
		}
		if group == "/" {
			return available, nil
		}
		group = filepath.Dir(group)
	}
	return 0, errors.New("CNB cgroup ancestry exceeds its bound")
}

func inspectBuildpacksCapacity(state, dockerRoot string) (BuildpacksCapacity, error) {
	var result BuildpacksCapacity
	pidData, err := os.ReadFile("/var/run/docker.pid")
	if err != nil || len(pidData) > 32 {
		return result, errors.New("CNB resource admission requires a local Docker worker with readable daemon PID and cgroup accounting")
	}
	pid, err := strconv.ParseUint(strings.TrimSpace(string(pidData)), 10, 32)
	if err != nil || pid == 0 {
		return result, errors.New("CNB cannot verify the local Docker daemon PID")
	}
	process := filepath.Join("/proc", strconv.FormatUint(pid, 10))
	name, err := os.ReadFile(filepath.Join(process, "comm"))
	if err != nil || strings.TrimSpace(string(name)) != "dockerd" {
		return result, errors.New("CNB requires the worker to share Docker's process view for resource admission")
	}
	groups, err := os.ReadFile(filepath.Join(process, "cgroup"))
	if err != nil || len(groups) > 4096 {
		return result, errors.New("CNB cannot read Docker's memory cgroup")
	}
	group := ""
	for _, line := range strings.Split(string(groups), "\n") {
		if strings.HasPrefix(line, "0::/") {
			group = strings.TrimPrefix(line, "0::")
		}
	}
	if group == "" {
		return result, errors.New("CNB resource admission requires cgroup v2")
	}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil || len(data) > 65536 {
		return result, errors.New("CNB cannot read available node memory")
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemAvailable:" && fields[2] == "kB" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil && value <= ^uint64(0)/1024 {
				result.MemoryAvailable = value * 1024
			}
		}
	}
	if result.MemoryAvailable == 0 {
		return result, errors.New("CNB available node memory is unknown")
	}
	result.MemoryAvailable, err = cnbCgroupHeadroom("/sys/fs/cgroup", group, result.MemoryAvailable)
	if err != nil {
		return result, err
	}
	for _, check := range []struct {
		path  string
		value *uint64
	}{{state, &result.StateAvailable}, {dockerRoot, &result.DiskAvailable}} {
		var filesystem syscall.Statfs_t
		if check.path == "" || syscall.Statfs(check.path, &filesystem) != nil || filesystem.Bsize <= 0 || filesystem.Bavail > ^uint64(0)/uint64(filesystem.Bsize) {
			return result, errors.New("CNB cannot verify free storage at the local worker/Docker filesystem")
		}
		*check.value = filesystem.Bavail * uint64(filesystem.Bsize)
	}
	return result, nil
}

func verifyBuildpacksWorkFilesystem() error {
	for _, check := range []struct {
		path     string
		capacity uint64
	}{{"/meta", 16 << 20}} {
		var filesystem syscall.Statfs_t
		if syscall.Statfs(check.path, &filesystem) != nil || filesystem.Type != 0x01021994 || filesystem.Bsize <= 0 || filesystem.Blocks > ^uint64(0)/uint64(filesystem.Bsize) || filesystem.Blocks*uint64(filesystem.Bsize) != check.capacity {
			return errors.New("CNB frozen metadata must use the verified 16 MiB tmpfs volume")
		}
	}
	var work syscall.Statfs_t
	if syscall.Statfs("/work", &work) != nil || work.Type == 0x01021994 {
		return errors.New("CNB work state must be operation-owned disk storage; its monitored target is not a hard storage cap")
	}
	return nil
}
