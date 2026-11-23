package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func capacityNumber(value any) float64 {
	switch value := value.(type) {
	case float64:
		if !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 {
			return value
		}
	case string:
		n, err := strconv.ParseFloat(value, 64)
		if err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 {
			return n
		}
		if n, ok := metricBytes(value); ok {
			return n
		}
	}
	return 0
}

type ResourceUsage struct {
	CPUs     float64 `json:"cpus"`
	MemoryMB int64   `json:"memory_mb"`
}

func declaredCapacity(config map[string]any) (float64, float64, bool, bool, error) {
	cpu, memory := float64(0), float64(0)
	unknownCPU, unknownMemory := false, false
	services, _ := config["services"].(map[string]any)
	for _, raw := range services {
		service, _ := raw.(map[string]any)
		deploy, _ := service["deploy"].(map[string]any)
		resources, _ := deploy["resources"].(map[string]any)
		limits, _ := resources["limits"].(map[string]any)
		replicas := float64(1)
		if count, ok := deploy["replicas"]; ok {
			replicas = capacityNumber(count)
			if replicas < 0 || replicas > 10000 {
				return 0, 0, false, false, fmt.Errorf("replica capacity could not be estimated")
			}
		}
		if count, ok := service["scale"]; ok {
			replicas = capacityNumber(count)
			if replicas < 0 || replicas > 10000 {
				return 0, 0, false, false, fmt.Errorf("replica capacity could not be estimated")
			}
		}
		cores := capacityNumber(service["cpus"])
		if cores == 0 {
			cores = capacityNumber(limits["cpus"])
		}
		if cores == 0 {
			quota := capacityNumber(service["cpu_quota"])
			period := capacityNumber(service["cpu_period"])
			if period == 0 {
				period = 100000
			}
			if quota > 0 {
				cores = quota / period
			}
		}
		bytes := capacityNumber(service["mem_limit"])
		if bytes == 0 {
			bytes = capacityNumber(limits["memory"])
		}
		if replicas > 0 {
			unknownCPU = unknownCPU || cores <= 0
			unknownMemory = unknownMemory || bytes <= 0
		}
		cpu += max(0, cores) * replicas
		memory += max(0, bytes) * replicas
	}
	if math.IsNaN(cpu) || math.IsInf(cpu, 0) || math.IsNaN(memory) || math.IsInf(memory, 0) || memory/(1<<20) >= math.MaxInt64 {
		return 0, 0, false, false, fmt.Errorf("declared capacity exceeds accounting limits")
	}
	return cpu, memory, unknownCPU, unknownMemory, nil
}

func capacityChecks(config map[string]any, sample *NodeMetrics) []PreflightCheck {
	if sample == nil || sample.MemoryTotal == 0 || sample.CPUs < 1 {
		return []PreflightCheck{{Name: "Capacity", State: "warning", Message: "Worker free resources could not be measured."}}
	}
	cpu, memory, unknownCPU, unknownMemory, err := declaredCapacity(config)
	if err != nil {
		return []PreflightCheck{{Name: "Capacity", State: "warning", Message: "Replica capacity could not be estimated."}}
	}
	freeCPU := float64(sample.CPUs) * max(0, 1-sample.CPUPercent/100)
	cpuState, memoryState := "pass", "pass"
	if cpu > freeCPU || unknownCPU {
		cpuState = "warning"
	}
	if memory > float64(sample.MemoryAvailable) || unknownMemory {
		memoryState = "warning"
	}
	cpuMessage := fmt.Sprintf("Declared CPU caps total %.2f cores; approximately %.2f cores are currently idle of %d. Caps are upper bounds, not reservations.", cpu, freeCPU, sample.CPUs)
	memoryMessage := fmt.Sprintf("Declared memory caps total %.0f MiB; %.0f MiB is currently available. Existing containers, candidate overlap and build overhead may require more.", memory/(1<<20), float64(sample.MemoryAvailable)/(1<<20))
	if unknownCPU {
		cpuMessage += " Some active services have no declared CPU cap."
	}
	if unknownMemory {
		memoryMessage += " Some active services have no declared memory cap."
	}
	return []PreflightCheck{{Name: "CPU capacity", State: cpuState, Message: cpuMessage}, {Name: "Memory capacity", State: memoryState, Message: memoryMessage}}
}

func (e *Engine) Capacity(ctx context.Context, r Request) (report PreflightReport) {
	report = PreflightReport{Checks: []PreflightCheck{}}
	if r.Action != "deploy" || ValidateRequest(r) != nil {
		return PreflightReport{Checks: []PreflightCheck{{Name: "Capacity", State: "error", Message: "Invalid deployment configuration."}}}
	}
	if !e.preflightMu.TryLock() {
		return PreflightReport{Checks: []PreflightCheck{{Name: "Capacity", State: "warning", Message: "Another worker inspection is running."}}}
	}
	defer e.preflightMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp(e.Root, "capacity-")
	if err != nil {
		return PreflightReport{Checks: []PreflightCheck{{Name: "Capacity", State: "error", Message: "Cannot stage capacity inspection."}}}
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			report.Checks = append(report.Checks, PreflightCheck{Name: "Capacity cleanup", State: "warning", Message: "Temporary capacity files need worker inspection."})
		}
	}()
	compose := filepath.Join(dir, "compose.yaml")
	if r.Source != nil {
		compose, report.Revision, err = e.checkout(ctx, r, dir, io.Discard)
	} else {
		err = os.WriteFile(compose, []byte(r.Compose), 0600)
	}
	if err != nil {
		return PreflightReport{Checks: []PreflightCheck{{Name: "Capacity", State: "warning", Message: "Cannot read the selected Compose source."}}}
	}
	env := []string{}
	for key, value := range r.Environment {
		env = append(env, key+"="+value)
	}
	compose, err = e.prepareComposeBundle(ctx, r, dir, compose, env)
	if err != nil {
		return PreflightReport{Checks: []PreflightCheck{{Name: "Capacity", State: "warning", Message: "Cannot resolve the ordered Compose selection."}}}
	}
	out := &limitedBuffer{Limit: 2 * 1024 * 1024}
	err = e.Run(ctx, dir, []string{"docker", "compose", "--project-name", e.composeProject(r.Workload), "--file", compose, "config", "--format", "json"}, env, out)
	var config map[string]any
	if err != nil || json.Unmarshal(out.data.Bytes(), &config) != nil || applyDefaultLimits(config, r.DefaultLimits) != nil || applyLimits(config, r.Limits) != nil {
		return PreflightReport{Checks: []PreflightCheck{{Name: "Capacity", State: "warning", Message: "Cannot resolve Compose resource limits."}}}
	}
	applyCapacityReplicas(config, r.Rollout)
	if cpu, memory, _, _, err := declaredCapacity(config); err == nil {
		report.Usage = &ResourceUsage{CPUs: cpu, MemoryMB: int64(math.Ceil(memory / (1 << 20)))}
	}
	report.Checks = capacityChecks(config, inspectMetrics(ctx, e.Root))
	return report
}

func applyCapacityReplicas(config map[string]any, rollout *RolloutOptions) {
	if rollout == nil || rollout.Strategy == "disabled" {
		return
	}
	services, _ := config["services"].(map[string]any)
	for name, count := range rollout.Replicas {
		if service, ok := services[name].(map[string]any); ok {
			service["scale"] = float64(count)
		}
	}
}

func (e *Engine) warnDeploymentCapacity(ctx context.Context, dir string, args, env []string, out io.Writer, rollout *RolloutOptions) {
	data := &limitedBuffer{Limit: 2 * 1024 * 1024}
	if err := e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, data); err != nil {
		fmt.Fprintln(out, "Capacity warning: Compose demand could not be inspected.")
		return
	}
	var config map[string]any
	if json.Unmarshal(data.data.Bytes(), &config) != nil {
		fmt.Fprintln(out, "Capacity warning: Compose demand could not be read.")
		return
	}
	applyCapacityReplicas(config, rollout)
	for _, check := range capacityChecks(config, inspectMetrics(ctx, e.Root)) {
		if check.State != "pass" {
			fmt.Fprintln(out, "Capacity warning:", check.Message)
		}
	}
}
