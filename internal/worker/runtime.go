package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Container struct {
	Health             string            `json:"health,omitempty"`
	ExitCode           *int              `json:"exit_code,omitempty"`
	Service            string            `json:"service,omitempty"`
	ExpectedCompleted  bool              `json:"expected_completed,omitempty"`
	Metrics            *ContainerMetrics `json:"metrics,omitempty"`
	MetricsUnavailable bool              `json:"metrics_unavailable,omitempty"`
	ID                 string            `json:"id"`
	Name               string            `json:"name"`
	Image              string            `json:"image"`
	State              string            `json:"state"`
	Status             string            `json:"status"`
	Ports              string            `json:"ports"`
}

func (e *Engine) inspectContainerStates(ctx context.Context, containers []Container) error {
	if len(containers) == 0 {
		return nil
	}
	args := []string{"docker", "inspect", "--format", `{"id":{{json .Id}},"state":{{json .State}},"service":{{json (index .Config.Labels "com.docker.compose.service")}}}`}
	for _, container := range containers {
		args = append(args, container.ID)
	}
	out := &limitedBuffer{Limit: 2 * 1024 * 1024}
	if err := e.Run(ctx, e.Root, args, nil, out); err != nil {
		return err
	}
	decoder := json.NewDecoder(&out.data)
	seen := map[string]bool{}
	for {
		var detail struct {
			ID      string
			Service string
			State   struct {
				Status   string
				ExitCode int
				Health   *struct{ Status string }
			}
		}
		if err := decoder.Decode(&detail); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		for i := range containers {
			if detail.ID == "" || !strings.HasPrefix(detail.ID, containers[i].ID) {
				continue
			}
			containers[i].State, containers[i].Service = detail.State.Status, detail.Service
			if detail.State.Health != nil {
				containers[i].Health = detail.State.Health.Status
			}
			if detail.State.Status == "exited" || detail.State.Status == "dead" {
				code := detail.State.ExitCode
				containers[i].ExitCode = &code
			}
			seen[containers[i].ID] = true
		}
	}
	if len(seen) != len(containers) {
		return errors.New("container state inspection was incomplete")
	}
	return nil
}

type limitedBuffer struct {
	data  bytes.Buffer
	Limit int
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if len(data) > b.Limit-b.data.Len() {
		return 0, errors.New("response exceeds limit")
	}
	return b.data.Write(data)
}
func (e *Engine) Containers(ctx context.Context, id string) ([]Container, error) {
	if !workloadIdentifier.MatchString(id) {
		return nil, errors.New("invalid workload ID")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output := &limitedBuffer{Limit: 1024 * 1024}
	if err := e.Run(ctx, e.Root, []string{"docker", "ps", "--all", "--filter", "label=com.docker.compose.project=cove-" + id, "--format", "{{json .}}"}, nil, output); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(&output.data)
	containers := []Container{}
	for {
		var docker struct{ ID, Names, Image, State, Status, Ports string }
		err := decoder.Decode(&docker)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		containers = append(containers, Container{ID: docker.ID, Name: docker.Names, Image: docker.Image, State: docker.State, Status: docker.Status, Ports: docker.Ports})
	}

	return containers, nil
}
func (e *Engine) ContainerLogs(ctx context.Context, id string) (string, error) {
	if !workloadIdentifier.MatchString(id) {
		return "", errors.New("invalid workload ID")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dir := filepath.Join(e.Root, "workloads", id)
	data, err := os.ReadFile(filepath.Join(dir, "current.json"))
	if err != nil {
		return "", err
	}
	var current deployedState
	if err = json.Unmarshal(data, &current); err != nil {
		return "", err
	}
	var output bytes.Buffer
	log := &boundedLog{file: &output, remaining: 65536}
	var environment []string
	for key, value := range current.Environment {
		environment = append(environment, key+"="+value)
		log.secrets = append(log.secrets, value)
	}
	err = e.Run(ctx, dir, []string{"docker", "compose", "--project-name", "cove-" + id, "--file", current.ComposePath, "logs", "--no-color", "--timestamps", "--tail", "200"}, environment, log)
	if flushErr := log.flush(true); err == nil {
		err = flushErr
	}
	return output.String(), err
}
