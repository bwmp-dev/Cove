package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type NodeMetrics struct {
	SampledAt       time.Time `json:"sampled_at"`
	CPUPercent      float64   `json:"cpu_percent"`
	MemoryTotal     uint64    `json:"memory_total"`
	MemoryAvailable uint64    `json:"memory_available"`
	DiskTotal       uint64    `json:"disk_total"`
	DiskAvailable   uint64    `json:"disk_available"`
}

type SharedHost struct {
	CoolifyDetected bool `json:"coolify_detected"`
	CleanupVerified bool `json:"cleanup_verified"`
}

func inspectSharedHost(ctx context.Context, run CommandRunner, root string) *SharedHost {
	out := &limitedBuffer{Limit: 65536}
	if err := run(ctx, root, []string{"docker", "ps", "-a", "--filter", "label=coolify.managed=true", "--format", "{{.ID}}"}, nil, out); err != nil {
		return nil
	}
	return &SharedHost{CoolifyDetected: strings.TrimSpace(out.data.String()) != ""}
}

func (e *Engine) containerMetrics(ctx context.Context, containers []Container) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if len(containers) == 0 {
		return nil
	}
	args := []string{"docker", "stats", "--no-stream", "--no-trunc", "--format", "{{json .}}"}
	for _, container := range containers {
		args = append(args, container.ID)
	}
	out := &limitedBuffer{Limit: 1024 * 1024}
	if err := e.Run(ctx, e.Root, args, nil, out); err != nil {
		return err
	}
	decoder := json.NewDecoder(&out.data)
	for {
		var sample struct{ ID, CPUPerc, MemUsage, MemPerc, NetIO, BlockIO, PIDs string }
		err := decoder.Decode(&sample)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		for i := range containers {
			if strings.HasPrefix(sample.ID, containers[i].ID) {
				containers[i].Metrics = &ContainerMetrics{CPU: sample.CPUPerc, Memory: sample.MemUsage, MemoryPercent: sample.MemPerc, Network: sample.NetIO, DiskIO: sample.BlockIO, PIDs: sample.PIDs}
			}
		}
	}
}

type ContainerMetrics struct {
	CPU           string `json:"cpu"`
	Memory        string `json:"memory"`
	MemoryPercent string `json:"memory_percent"`
	Network       string `json:"network"`
	DiskIO        string `json:"disk_io"`
	PIDs          string `json:"pids"`
}

func (e *Engine) HealthContainers(ctx context.Context, workload string) ([]Container, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	containers, err := e.Containers(ctx, workload)
	if err != nil {
		return nil, err
	}
	if err = e.inspectContainerStates(ctx, containers); err != nil {
		return nil, err
	}
	if len(containers) == 0 {
		return containers, nil
	}
	data, err := os.ReadFile(filepath.Join(e.Root, "workloads", workload, "current.json"))
	if os.IsNotExist(err) {
		return containers, nil
	}
	if err != nil {
		return nil, err
	}
	var state deployedState
	if err = json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	data, err = os.ReadFile(state.ComposePath)
	if err != nil {
		return nil, err
	}
	var config struct {
		Services map[string]struct {
			DependsOn any `yaml:"depends_on"`
		} `yaml:"services"`
	}
	if err = yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	completed := map[string]bool{}
	for _, service := range config.Services {
		dependencies, _ := service.DependsOn.(map[string]any)
		for name, raw := range dependencies {
			dependency, _ := raw.(map[string]any)
			if dependency["condition"] == "service_completed_successfully" {
				completed[name] = true
			}
		}
	}
	if len(completed) == 0 {
		return containers, nil
	}
	for i, container := range containers {
		if container.State != "exited" {
			continue
		}
		if completed[container.Service] && container.ExitCode != nil && *container.ExitCode == 0 {
			containers[i].ExpectedCompleted = true
		}
	}
	return containers, nil
}
