package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type TelemetryRequest struct {
	Since time.Time `json:"since"`
}
type LogEntry struct {
	Clipped     bool      `json:"clipped,omitempty"`
	ContainerID string    `json:"container_id"`
	Container   string    `json:"container"`
	Timestamp   time.Time `json:"timestamp"`
	Message     string    `json:"message"`
}
type Telemetry struct {
	SampledAt   time.Time   `json:"sampled_at"`
	Containers  []Container `json:"containers"`
	Logs        []LogEntry  `json:"logs"`
	LogsLimited bool        `json:"logs_limited"`
}

func (e *Engine) CollectTelemetry(ctx context.Context, id string, since time.Time) (*Telemetry, error) {
	return e.collectTelemetry(ctx, id, since, true)
}
func (e *Engine) CollectLiveLogs(ctx context.Context, id string, since time.Time) (*Telemetry, error) {
	e.mu.Lock()
	if e.tailSlots == nil {
		e.tailSlots = make(chan struct{}, 4)
	}
	slots := e.tailSlots
	e.mu.Unlock()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		return nil, errors.New("live tail concurrency limit reached")
	}
	limited := false
	if since.IsZero() {
		since = time.Now().Add(-30 * time.Second)
	}
	if since.Before(time.Now().Add(-5 * time.Minute)) {
		since = time.Now().Add(-5 * time.Minute)
		limited = true
	}
	result, err := e.collectTelemetry(ctx, id, since, false)
	if err != nil {
		return nil, err
	}
	result.LogsLimited = result.LogsLimited || limited
	for i := range result.Logs {
		if len(result.Logs[i].Message) > 8192 {
			result.Logs[i].Message = strings.ToValidUTF8(result.Logs[i].Message[:8192], "")
			result.Logs[i].Clipped = true
			result.LogsLimited = true
		}
	}
	return result, nil
}
func (e *Engine) collectTelemetry(ctx context.Context, id string, since time.Time, metrics bool) (*Telemetry, error) {
	if !workloadIdentifier.MatchString(id) {
		return nil, errors.New("invalid workload ID")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	now := time.Now().UTC()
	clamped := false
	if since.IsZero() {
		since = now.Add(-time.Minute)
	}
	if since.Before(now.Add(-24 * time.Hour)) {
		since = now.Add(-24 * time.Hour)
		clamped = true
	}
	if since.After(now.Add(time.Minute)) {
		return nil, errors.New("log cursor is in the future")
	}
	containers, err := e.Containers(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(containers) > 32 {
		return nil, errors.New("telemetry supports at most 32 containers per resource")
	}
	if metrics {
		if err = e.containerMetrics(ctx, containers); err != nil {
			for i := range containers {
				containers[i].MetricsUnavailable = true
				containers[i].Metrics = nil
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(e.Root, "workloads", id, "current.json"))
	if err != nil {
		return nil, err
	}
	var current deployedState
	if err = json.Unmarshal(data, &current); err != nil {
		return nil, err
	}
	secrets := []string{}
	for _, value := range current.Environment {
		secrets = append(secrets, value)
	}
	result := &Telemetry{SampledAt: now, Containers: containers, Logs: []LogEntry{}, LogsLimited: clamped}
	for _, container := range containers {
		var output bytes.Buffer
		log := &boundedLog{file: &output, remaining: 65536, secrets: secrets}
		err = e.Run(ctx, e.Root, []string{"docker", "logs", "--timestamps", "--since", since.Format(time.RFC3339Nano), "--until", now.Format(time.RFC3339Nano), "--tail", "501", container.ID}, nil, log)
		if flushErr := log.flush(true); err == nil {
			err = flushErr
		}
		if err != nil {
			return nil, errors.New("container log collection failed")
		}
		if log.remaining == 0 {
			result.LogsLimited = true
		}
		lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
		if len(lines) > 500 {
			result.LogsLimited = true
			lines = lines[len(lines)-500:]
		}
		for _, line := range lines {
			if line == "" {
				continue
			}
			stamp, message, ok := strings.Cut(line, " ")
			timestamp, parseErr := time.Parse(time.RFC3339Nano, stamp)
			if !ok || parseErr != nil {
				result.LogsLimited = true
				continue
			}
			if len(result.Logs) >= 2000 {
				result.LogsLimited = true
				break
			}
			result.Logs = append(result.Logs, LogEntry{ContainerID: container.ID, Container: container.Name, Timestamp: timestamp, Message: message})
		}
	}
	return result, nil
}
