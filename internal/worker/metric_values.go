package worker

import (
	"math"
	"strconv"
	"strings"
)

type ContainerUsage struct {
	CPUPercent   float64 `json:"cpu_percent"`
	MemoryUsed   float64 `json:"memory_used"`
	MemoryLimit  float64 `json:"memory_limit"`
	NetworkRX    float64 `json:"network_rx"`
	NetworkTX    float64 `json:"network_tx"`
	BlockRead    float64 `json:"block_read"`
	BlockWritten float64 `json:"block_written"`
}

func metricBytes(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	i := 0
	for i < len(value) && (value[i] >= '0' && value[i] <= '9' || value[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.ParseFloat(value[:i], 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	factor, ok := map[string]float64{"B": 1, "kB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "KiB": 1024, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}[strings.TrimSpace(value[i:])]
	return n * factor, ok && !math.IsInf(n*factor, 0)
}

func containerUsage(m *ContainerMetrics) *ContainerUsage {
	usage := &ContainerUsage{}
	cpu, err := strconv.ParseFloat(strings.TrimSuffix(m.CPU, "%"), 64)
	if err != nil || cpu < 0 || math.IsNaN(cpu) || math.IsInf(cpu, 0) {
		return nil
	}
	usage.CPUPercent = cpu
	for _, pair := range []struct {
		value       string
		left, right *float64
	}{{m.Memory, &usage.MemoryUsed, &usage.MemoryLimit}, {m.Network, &usage.NetworkRX, &usage.NetworkTX}, {m.DiskIO, &usage.BlockRead, &usage.BlockWritten}} {
		left, right, ok := strings.Cut(pair.value, "/")
		if !ok {
			return nil
		}
		var valid bool
		*pair.left, valid = metricBytes(left)
		if !valid {
			return nil
		}
		*pair.right, valid = metricBytes(right)
		if !valid {
			return nil
		}
	}
	return usage
}
