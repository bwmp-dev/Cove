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
	Service            string            `json:"service"`
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
		var docker struct{ ID, Names, Image, State, Status, Ports, Labels string }
		err := decoder.Decode(&docker)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		service := ""
		for _, label := range strings.Split(docker.Labels, ",") {
			key, value, ok := strings.Cut(label, "=")
			if ok && key == "com.docker.compose.service" && serviceIdentifier.MatchString(value) {
				service = value
			}
		}
		containers = append(containers, Container{ID: docker.ID, Name: docker.Names, Image: docker.Image, State: docker.State, Status: docker.Status, Ports: docker.Ports, Service: service})
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
	log.secrets = append(log.secrets, current.AdditionalSecrets...)
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
