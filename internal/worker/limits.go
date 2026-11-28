package worker

import (
	"context"
	"errors"
	"io"
	"math"
	"path/filepath"
	"strconv"
)

type ServiceLimits struct {
	CPUs     float64   `json:"cpus"`
	MemoryMB int64     `json:"memory_mb"`
	GPUs     *[]string `json:"gpus,omitempty"`
}

func applyDefaultLimits(config map[string]any, defaults *ServiceLimits) error {
	if defaults == nil {
		return nil
	}
	if err := ValidateLimits(map[string]ServiceLimits{"default": *defaults}); err != nil {
		return err
	}
	services, ok := config["services"].(map[string]any)
	if !ok || len(services) > 64 {
		return errors.New("resource defaults require at most 64 Compose services")
	}
	limits := map[string]ServiceLimits{}
	for name, raw := range services {
		service, ok := raw.(map[string]any)
		if !ok {
			return errors.New("invalid Compose service")
		}
		deploy, _ := service["deploy"].(map[string]any)
		resources, _ := deploy["resources"].(map[string]any)
		caps, _ := resources["limits"].(map[string]any)
		limit := ServiceLimits{}
		if service["cpus"] == nil && service["cpu_quota"] == nil && caps["cpus"] == nil {
			limit.CPUs = defaults.CPUs
		}
		if service["mem_limit"] == nil && caps["memory"] == nil {
			limit.MemoryMB = defaults.MemoryMB
		}
		if limit.CPUs != 0 || limit.MemoryMB != 0 {
			limits[name] = limit
		}
	}
	return applyLimits(config, limits)
}

func ValidateLimits(limits map[string]ServiceLimits) error {
	if len(limits) > 64 {
		return errors.New("at most 64 service limits are allowed")
	}
	for service, limit := range limits {
		if err := validateGPUSelection(limit.GPUs); err != nil {
			return err
		}
		if !serviceIdentifier.MatchString(service) || math.IsNaN(limit.CPUs) || math.IsInf(limit.CPUs, 0) || limit.CPUs < 0 || limit.CPUs > 1024 || (limit.CPUs > 0 && limit.CPUs < 0.01) || limit.MemoryMB < 0 || limit.MemoryMB > 16777216 || (limit.MemoryMB > 0 && limit.MemoryMB < 6) {
			return errors.New("limits require a service name, CPU between 0.01 and 1024, and memory between 6 and 16777216 MiB; zero inherits Compose")
		}
	}
	return nil
}

func applyLimits(config map[string]any, limits map[string]ServiceLimits) error {
	if err := ValidateLimits(limits); err != nil {
		return err
	}
	services, ok := config["services"].(map[string]any)
	if !ok {
		return errors.New("Compose has no services")
	}
	for name, limit := range limits {
		service, ok := services[name].(map[string]any)
		if !ok {
			return errors.New("resource limit service does not exist in Compose")
		}
		deploy, ok := service["deploy"].(map[string]any)
		if !ok {
			deploy = map[string]any{}
			service["deploy"] = deploy
		}
		resources, ok := deploy["resources"].(map[string]any)
		if !ok {
			resources = map[string]any{}
			deploy["resources"] = resources
		}
		caps, ok := resources["limits"].(map[string]any)
		if !ok {
			caps = map[string]any{}
			resources["limits"] = caps
		}
		// Compose requires service-level and deploy-level limits to agree.
		if limit.CPUs > 0 {
			service["cpus"] = limit.CPUs
			caps["cpus"] = limit.CPUs
			delete(service, "cpu_quota")
			delete(service, "cpu_period")
		}
		if limit.MemoryMB > 0 {
			service["mem_limit"] = limit.MemoryMB * 1024 * 1024
			caps["memory"] = strconv.FormatInt(limit.MemoryMB*1024*1024, 10)
		}
		if err := applyServiceGPUs(service, resources, limit.GPUs); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) prepareLimits(ctx context.Context, r Request, dir string, args, env []string, out io.Writer) (string, error) {
	if err := e.validateGPUs(ctx, r.Limits); err != nil {
		return "", err
	}
	resolved := &limitedBuffer{Limit: 2 * 1024 * 1024}
	if err := e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, resolved); err != nil {
		return "", err
	}
	var config map[string]any
	if err := unmarshalCompose(resolved.data.Bytes(), &config); err != nil {
		return "", err
	}
	if err := applyDefaultLimits(config, r.DefaultLimits); err != nil {
		return "", err
	}
	if err := applyLimits(config, r.Limits); err != nil {
		return "", err
	}
	path := filepath.Join(dir, r.ID+".limits.compose.json")
	return path, writeJSON(path, literalCompose(config))
}
