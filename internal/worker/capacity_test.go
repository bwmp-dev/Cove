package worker

import (
	"strings"
	"testing"
)

func TestCapacityWarningsFromDeclaredLimits(t *testing.T) {
	config := map[string]any{"services": map[string]any{"app": map[string]any{"cpus": "2.5", "mem_limit": float64(512 << 20), "deploy": map[string]any{"replicas": float64(2)}}, "worker": map[string]any{"deploy": map[string]any{"resources": map[string]any{"limits": map[string]any{"cpus": "1.5", "memory": "268435456"}}}}}}
	checks := capacityChecks(config, &NodeMetrics{CPUs: 4, CPUPercent: 50, MemoryTotal: 2 << 30, MemoryAvailable: 512 << 20})
	if len(checks) != 2 || checks[0].State != "warning" || checks[1].State != "warning" || !strings.Contains(checks[0].Message, "6.50 cores") || !strings.Contains(checks[1].Message, "1280 MiB") {
		t.Fatal(checks)
	}
	config = map[string]any{"services": map[string]any{"app": map[string]any{}}}
	checks = capacityChecks(config, &NodeMetrics{CPUs: 4, MemoryTotal: 1 << 30, MemoryAvailable: 1 << 30})
	if checks[0].State != "warning" || !strings.Contains(checks[0].Message, "no declared CPU cap") {
		t.Fatal("unlimited service treated as zero demand", checks)
	}
	if got := capacityChecks(config, nil); len(got) != 1 || got[0].State != "warning" {
		t.Fatal(got)
	}
}

func TestCapacityAccountingIncludesInheritedDefaultsAndReplicas(t *testing.T) {
	config := map[string]any{"services": map[string]any{"web": map[string]any{}, "sidecar": map[string]any{"cpus": "0.25", "mem_limit": float64(32 << 20)}}}
	if err := applyDefaultLimits(config, &ServiceLimits{CPUs: 0.5, MemoryMB: 128}); err != nil {
		t.Fatal(err)
	}
	applyCapacityReplicas(config, &RolloutOptions{Strategy: "rolling", Replicas: map[string]int{"web": 3}})
	cpu, memory, unknownCPU, unknownMemory, err := declaredCapacity(config)
	if err != nil || cpu != 1.75 || memory != float64(416<<20) || unknownCPU || unknownMemory {
		t.Fatal("inherited replica totals", cpu, memory, unknownCPU, unknownMemory, err)
	}
}
