package worker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type CPUScaleOptions struct {
	Generation string             `json:"generation"`
	Before     map[string]int     `json:"before"`
	Replicas   map[string]int     `json:"replicas"`
	CPUPercent map[string]float64 `json:"cpu_percent"`
}

func validateCPUScale(r Request) error {
	if r.Scale == nil {
		return nil
	}
	if r.Action != "deploy" || r.Kind == "job" || r.Preview || r.EnvironmentPreview != nil || !identifier.MatchString(r.Scale.Generation) || r.PullLatest || r.Build != nil || r.Rollout == nil || r.Rollout.Strategy != "rolling" {
		return errors.New("CPU scaling requires an immutable rolling service release")
	}
	if len(r.Scale.Replicas) == 0 || len(r.Scale.Before) > 32 || len(r.Scale.CPUPercent) > 32 {
		return errors.New("CPU scaling requires a bounded replica change")
	}
	for name := range r.Scale.Replicas {
		cpu, ok := r.Scale.CPUPercent[name]
		if !ok || cpu < 0 || math.IsNaN(cpu) || math.IsInf(cpu, 0) || r.Scale.Before[name] < 1 || r.Scale.Before[name] > 16 {
			return errors.New("CPU scaling requires complete replica and CPU observations")
		}
	}
	return validateRollout(Request{Kind: "service", Rollout: &RolloutOptions{Strategy: "rolling", Replicas: r.Scale.Replicas}})
}

func (e *Engine) CPUScaleSample(ctx context.Context, workload string) (*CPUScaleObservation, error) {
	if !workloadIdentifier.MatchString(workload) {
		return nil, errors.New("invalid workload ID")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if e.cpuScaleBusy(workload) {
		return nil, errors.New("the resource has an active operation")
	}
	dir := filepath.Join(e.Root, "workloads", workload)
	state, exists, err := readCurrent(filepath.Join(dir, "current.json"))
	if err != nil || !exists || state.Generation == "" || state.Rollout == nil || state.Rollout.Strategy != "rolling" {
		return nil, errors.New("deploy an eligible rolling release before enabling CPU scaling")
	}
	if _, pending, err := e.pendingRollout(workload); err != nil || pending != nil {
		return nil, errors.New("a rollout is still pending")
	}
	if suspension, err := e.readSuspension(workload); err != nil || suspension != nil {
		return nil, errors.New("the resource is suspended or waking")
	}
	raw, err := os.ReadFile(state.ComposePath)
	if err != nil {
		return nil, err
	}
	var config map[string]any
	if err = decodeResolvedCompose(raw, &config); err != nil {
		return nil, err
	}
	if err = restoreServiceNames(config, state.Traffic); err != nil {
		return nil, err
	}
	request := Request{Kind: "service", Rollout: state.Rollout, Routes: state.Routes, Networks: state.Networks, Private: state.Private}
	if reason := rolloutEligibility(request, config); reason != "" {
		return nil, errors.New(reason)
	}
	containers, err := e.HealthContainers(ctx, workload)
	if err != nil {
		return nil, err
	}
	if err = e.containerMetrics(ctx, containers); err != nil {
		return nil, err
	}
	sample := &CPUScaleObservation{Generation: state.Generation, Replicas: map[string]int{}, CPUPercent: map[string]float64{}}
	for name, raw := range config["services"].(map[string]any) {
		count := 1
		if state.Rollout.Replicas[name] > 0 {
			count = state.Rollout.Replicas[name]
		}
		sample.Replicas[name] = count
		limit, err := strconv.ParseFloat(fmt.Sprint(raw.(map[string]any)["cpus"]), 64)
		if err != nil || limit <= 0 {
			continue
		}
		cpu, err := ServiceCPUPercent(containers, name, count, limit)
		if err != nil {
			return nil, err
		}
		sample.CPUPercent[name] = cpu
	}
	current, exists, err := readCurrent(filepath.Join(dir, "current.json"))
	if err != nil || !exists || current.Generation != state.Generation || e.cpuScaleBusy(workload) {
		return nil, errors.New("the deployed generation changed during the CPU sample")
	}
	sample.SampledAt = time.Now().UTC()
	return sample, nil
}

func (e *Engine) cpuScaleBusy(workload string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.draining || e.busy(workload) {
		return true
	}
	for _, operation := range e.pending {
		if operation.Workload == workload {
			return true
		}
	}
	return false
}

func (e *Engine) prepareCPUScale(ctx context.Context, r Request) (Request, error) {
	current, exists, err := readCurrent(filepath.Join(e.Root, "workloads", r.Workload, "current.json"))
	if err != nil || !exists || current.Generation != r.Scale.Generation {
		return r, errors.New("the release changed after CPU scaling was queued")
	}
	if len(current.Traffic) == 0 || current.Rollout == nil {
		return r, errors.New("CPU scaling requires a parallel-generation release")
	}
	for name, count := range r.Scale.Before {
		actual := current.Rollout.Replicas[name]
		if actual == 0 {
			actual = 1
		}
		if actual != count {
			return r, errors.New("the replica count changed after CPU scaling was queued")
		}
	}
	containers, err := e.HealthContainers(ctx, r.Workload)
	if err != nil {
		return r, err
	}
	for name := range r.Scale.Replicas {
		count := 0
		for _, container := range containers {
			if container.Service != name {
				continue
			}
			if container.State != "running" || container.Health != "healthy" {
				return r, errors.New("a replica became unhealthy before CPU scaling started")
			}
			count++
		}
		if count != r.Scale.Before[name] {
			return r, errors.New("the live replica count changed before CPU scaling started")
		}
	}
	// Rollback already reads immutable images and environment from the local release snapshot.
	r.Action, r.Release = "rollback", r.Scale.Generation
	r.Source, r.Hooks, r.FileSecrets = nil, nil, nil
	return r, nil
}

func mergedCPUReplicas(current, overrides map[string]int) map[string]int {
	next := map[string]int{}
	for name, count := range current {
		next[name] = count
	}
	for name, count := range overrides {
		next[name] = count
	}
	return next
}
