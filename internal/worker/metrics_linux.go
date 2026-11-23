//go:build linux

package worker

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func cpuCounters() (uint64, uint64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(strings.SplitN(string(data), "\n", 2)[0])
	if len(fields) < 9 {
		return 0, 0, fmt.Errorf("CPU counters unavailable")
	}
	var total, idle uint64
	// Guest time is already included in user/nice; count only the first eight fields.
	for i := 1; i <= 8; i++ {
		value, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return 0, 0, err
		}
		total += value
		if i == 4 || i == 5 {
			idle += value
		}
	}
	return total, idle, nil
}
func inspectMetrics(ctx context.Context, root string) *NodeMetrics {
	total, idle, err := cpuCounters()
	if err != nil {
		return nil
	}
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil
	case <-timer.C:
	}
	next, nextIdle, err := cpuCounters()
	if err != nil || next <= total || nextIdle < idle {
		return nil
	}
	sample := &NodeMetrics{SampledAt: time.Now().UTC(), CPUPercent: 100 * (1 - float64(nextIdle-idle)/float64(next-total))}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			sample.MemoryTotal = value * 1024
		case "MemAvailable:":
			sample.MemoryAvailable = value * 1024
		}
	}
	var disk syscall.Statfs_t
	if syscall.Statfs(root, &disk) != nil || sample.MemoryTotal == 0 {
		return nil
	}
	sample.DiskTotal = disk.Blocks * uint64(disk.Bsize)
	sample.DiskAvailable = disk.Bavail * uint64(disk.Bsize)
	if rx, tx, ok := networkCounters(); ok {
		sample.NetworkRX = &rx
		sample.NetworkTX = &tx
	}
	sample.Mounts, sample.MountsLimited = inspectMounts()
	sample.CPUs = runtime.NumCPU()
	return sample
}

func networkCounters() (uint64, uint64, bool) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	var rx, tx uint64
	for _, line := range strings.Split(string(data), "\n") {
		name, counters, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		fields := strings.Fields(counters)
		if len(fields) < 16 {
			return 0, 0, false
		}
		received, e1 := strconv.ParseUint(fields[0], 10, 64)
		sent, e2 := strconv.ParseUint(fields[8], 10, 64)
		if e1 != nil || e2 != nil {
			return 0, 0, false
		}
		rx += received
		tx += sent
	}
	return rx, tx, true
}
