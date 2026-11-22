package worker

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

type AutoscaleService struct {
	MinReplicas      int `json:"min_replicas"`
	MaxReplicas      int `json:"max_replicas"`
	TargetCPUPercent int `json:"target_cpu_percent"`
}

type AutoscalePolicy struct {
	Enabled  bool                        `json:"enabled"`
	Services map[string]AutoscaleService `json:"services"`
}

func (p AutoscalePolicy) Normalized() AutoscalePolicy {
	next := AutoscalePolicy{Enabled: p.Enabled, Services: map[string]AutoscaleService{}}
	for name, service := range p.Services {
		if service.MinReplicas == 0 {
			service.MinReplicas = 1
		}
		if service.MaxReplicas == 0 {
			service.MaxReplicas = 4
		}
		if service.TargetCPUPercent == 0 {
			service.TargetCPUPercent = 70
		}
		next.Services[name] = service
	}
	return next
}

func (p AutoscalePolicy) Validate() error {
	p = p.Normalized()
	if p.Enabled && len(p.Services) == 0 {
		return errors.New("select at least one service for CPU scaling")
	}
	maximum := map[string]int{}
	for name, service := range p.Services {
		if service.MinReplicas < 1 || service.MinReplicas > service.MaxReplicas || service.TargetCPUPercent < 10 || service.TargetCPUPercent > 90 {
			return errors.New("CPU scaling requires ordered replica bounds and a CPU target from 10 to 90 percent")
		}
		maximum[name] = service.MaxReplicas
	}
	return validateRollout(Request{Kind: "service", Rollout: &RolloutOptions{Strategy: "rolling", Replicas: maximum}})
}

type CPUScaleObservation struct {
	SampledAt  time.Time          `json:"sampled_at"`
	Generation string             `json:"generation"`
	Replicas   map[string]int     `json:"replicas"`
	CPUPercent map[string]float64 `json:"cpu_percent"`
}

func ServiceCPUPercent(containers []Container, service string, replicas int, limitCPUs float64) (float64, error) {
	if replicas < 1 || limitCPUs <= 0 || math.IsNaN(limitCPUs) || math.IsInf(limitCPUs, 0) {
		return 0, errors.New("CPU scaling requires an explicit positive CPU limit")
	}
	count := 0
	total := 0.0
	for _, container := range containers {
		if container.Service != service {
			continue
		}
		if container.State != "running" || container.Health != "healthy" || container.Metrics == nil || container.MetricsUnavailable {
			return 0, errors.New("CPU scaling requires current metrics from every healthy replica")
		}
		text := strings.TrimSpace(container.Metrics.CPU)
		if !strings.HasSuffix(text, "%") {
			return 0, errors.New("Docker CPU sample is unavailable")
		}
		value, err := strconv.ParseFloat(strings.TrimSuffix(text, "%"), 64)
		if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, errors.New("Docker CPU sample is invalid")
		}
		count++
		total += value
	}
	if math.IsNaN(total) || math.IsInf(total, 0) {
		return 0, errors.New("Docker CPU aggregate is invalid")
	}
	if count != replicas {
		return 0, errors.New("CPU sample does not match the deployed replica count")
	}
	return total / (float64(count) * limitCPUs), nil
}

func CPUScaleDecision(policy AutoscalePolicy, samples []CPUScaleObservation, now time.Time, lastAttempt *time.Time) (map[string]int, string) {
	if !policy.Enabled {
		return nil, "disabled"
	}
	if policy.Validate() != nil {
		return nil, "invalid_policy"
	}
	if lastAttempt != nil && now.Sub(*lastAttempt) < 5*time.Minute {
		return nil, "cooldown"
	}
	if len(samples) < 3 {
		return nil, "waiting_for_samples"
	}
	samples = samples[len(samples)-3:]
	latest := samples[2]
	if !identifier.MatchString(latest.Generation) || latest.SampledAt.After(now) || now.Sub(latest.SampledAt) > 90*time.Second {
		return nil, "stale_sample"
	}
	for index, sample := range samples {
		if sample.Generation != latest.Generation || sample.SampledAt.After(now) || now.Sub(sample.SampledAt) > 4*time.Minute {
			return nil, "stale_sample"
		}
		if index > 0 {
			delta := sample.SampledAt.Sub(samples[index-1].SampledAt)
			if delta < 30*time.Second || delta > 90*time.Second {
				return nil, "waiting_for_samples"
			}
		}
	}
	policy = policy.Normalized()
	next := map[string]int{}
	for name, service := range policy.Services {
		current := latest.Replicas[name]
		if current < 1 {
			return nil, "replicas_unknown"
		}
		high, low := true, true
		for _, sample := range samples {
			value, ok := sample.CPUPercent[name]
			if !ok || sample.Replicas[name] != current || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, "partial_sample"
			}
			high = high && value >= float64(service.TargetCPUPercent)
			low = low && value <= float64(service.TargetCPUPercent)/2
		}
		wanted := current
		switch {
		case current < service.MinReplicas:
			wanted = service.MinReplicas
		case current > service.MaxReplicas:
			wanted = service.MaxReplicas
		case high && current < service.MaxReplicas:
			wanted++
		case low && current > service.MinReplicas:
			wanted--
		}
		if wanted != current {
			next[name] = wanted
		}
	}
	if len(next) == 0 {
		return nil, "within_target"
	}
	return next, "scale"
}
