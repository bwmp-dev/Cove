package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type SuspensionOptions struct {
	Reason string `json:"reason"`
	Scope  string `json:"scope"`
	Start  bool   `json:"start"`
}

type suspensionContainer struct {
	ID         string `json:"id"`
	Restart    string `json:"restart"`
	Retries    int    `json:"retries"`
	Paused     bool   `json:"paused,omitempty"`
	WasRunning bool   `json:"was_running"`
	Completed  bool   `json:"expected_completed,omitempty"`
}

func (c suspensionContainer) restartArgument() (string, error) {
	if c.Restart != "no" && c.Restart != "always" && c.Restart != "unless-stopped" && c.Restart != "on-failure" || c.Retries < 0 {
		return "", errors.New("invalid suspended restart policy")
	}
	policy := c.Restart
	if policy == "on-failure" && c.Retries > 0 {
		policy += ":" + strconv.Itoa(c.Retries)
	}
	return policy, nil
}

type workloadSuspension struct {
	Operation  string                `json:"operation"`
	Options    SuspensionOptions     `json:"options"`
	Phase      string                `json:"phase"`
	Created    time.Time             `json:"created"`
	Deadline   time.Time             `json:"deadline,omitempty"`
	Containers []suspensionContainer `json:"containers"`
	ManualStop bool                  `json:"manual_stop,omitempty"`
}

type SuspensionStatus struct {
	Operation         string `json:"operation"`
	Scope             string `json:"scope"`
	Reason            string `json:"reason"`
	Phase             string `json:"phase"`
	Captured          int    `json:"captured"`
	PreviouslyRunning int    `json:"previously_running"`
}

func (s workloadSuspension) status() *SuspensionStatus {
	status := &SuspensionStatus{Operation: s.Operation, Scope: s.Options.Scope, Reason: s.Options.Reason, Phase: s.Phase, Captured: len(s.Containers)}
	for _, container := range s.Containers {
		if container.WasRunning {
			status.PreviouslyRunning++
		}
	}
	return status
}

func (e *Engine) suspensionStatus(workload string) (*SuspensionStatus, error) {
	state, err := e.readSuspension(workload)
	if err != nil || state == nil {
		return nil, err
	}
	if state.Options.Reason == "manual_stop" {
		return nil, nil
	}
	return state.status(), nil
}

func (e *Engine) readSuspension(workload string) (*workloadSuspension, error) {
	if !workloadIdentifier.MatchString(workload) {
		return nil, errors.New("invalid suspension workload")
	}
	data, err := os.ReadFile(filepath.Join(e.Root, "workloads", workload, "suspension.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state workloadSuspension
	if err = json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if !identifier.MatchString(state.Operation) || !identifier.MatchString(state.Options.Scope) {
		return nil, errors.New("invalid suspension identity")
	}
	if state.Options.Reason != "environment" && state.Options.Reason != "manual_stop" {
		return nil, errors.New("invalid suspension reason")
	}
	if state.Created.IsZero() || state.Phase != "pausing" && state.Phase != "paused" && state.Phase != "resuming" && state.Phase != "awake" {
		return nil, errors.New("invalid suspension phase")
	}
	seen := map[string]bool{}
	for _, container := range state.Containers {
		if !containerIdentity.MatchString(container.ID) || seen[container.ID] {
			return nil, errors.New("invalid suspended container identity")
		}
		seen[container.ID] = true
		if _, err := container.restartArgument(); err != nil {
			return nil, err
		}
	}
	return &state, nil
}

func (e *Engine) captureSuspension(ctx context.Context, workload string) ([]suspensionContainer, error) {
	containers, err := e.HealthContainers(ctx, workload)
	if err != nil {
		return nil, err
	}
	captured := []suspensionContainer{}
	for _, container := range containers {
		if !containerIdentity.MatchString(container.ID) {
			return nil, errors.New("invalid suspension container")
		}
		var detail struct {
			ID, Project string
			State       struct{ Running, Paused bool }
			Restart     struct {
				Name              string
				MaximumRetryCount int
			}
		}
		if err := e.imageJSON(ctx, nil, &detail, "docker", "inspect", "--format", `{"id":{{json .Id}},"project":{{json (index .Config.Labels "com.docker.compose.project")}},"state":{{json .State}},"restart":{{json .HostConfig.RestartPolicy}}}`, container.ID); err != nil {
			return nil, err
		}
		if detail.Project != "cove-"+workload || !containerIdentity.MatchString(detail.ID) {
			return nil, errors.New("suspension container belongs to another workload")
		}
		policy := detail.Restart.Name
		if policy == "" {
			policy = "no"
		}
		entry := suspensionContainer{ID: detail.ID, Restart: policy, Retries: detail.Restart.MaximumRetryCount, Paused: detail.State.Paused, WasRunning: detail.State.Running, Completed: container.ExpectedCompleted}
		if _, err := entry.restartArgument(); err != nil {
			return nil, err
		}
		captured = append(captured, entry)
	}
	return captured, nil
}

func (e *Engine) stopSuspensionContainers(ctx context.Context, workload string, containers []suspensionContainer, out io.Writer) error {
	current, err := e.Containers(ctx, workload)
	if err != nil {
		return err
	}
	ids := []string{}
	paused := []string{}
	for _, container := range containers {
		for _, now := range current {
			if !containerIdentity.MatchString(now.ID) {
				return errors.New("invalid live suspension container identity")
			}
			if !strings.HasPrefix(container.ID, now.ID) {
				continue
			}
			if !containerIdentity.MatchString(container.ID) {
				return errors.New("invalid suspension container identity")
			}
			project, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, container.ID)
			if err != nil {
				return err
			}
			if project != "cove-"+workload {
				return errors.New("suspension container belongs to another workload")
			}
			ids = append(ids, container.ID)
			if now.State == "paused" {
				paused = append(paused, container.ID)
			}
			break
		}
	}
	if len(ids) > 0 {
		if err := e.Run(ctx, e.Root, append([]string{"docker", "update", "--restart=no"}, ids...), nil, out); err != nil {
			return err
		}
		if len(paused) > 0 {
			if err := e.Run(ctx, e.Root, append([]string{"docker", "unpause"}, paused...), nil, out); err != nil {
				return err
			}
		}
		if err := e.Run(ctx, e.Root, append([]string{"docker", "stop", "--timeout", "30"}, ids...), nil, out); err != nil {
			return err
		}
	}
	remaining, err := e.Containers(ctx, workload)
	if err != nil {
		return err
	}
	for _, container := range remaining {
		if container.State == "running" || container.State == "restarting" || container.State == "paused" {
			return errors.New("suspended workload still has running containers")
		}
	}
	return nil
}

func (e *Engine) restoreSuspensionPolicies(ctx context.Context, workload string, containers []suspensionContainer, out io.Writer) error {
	for _, container := range containers {
		if !containerIdentity.MatchString(container.ID) {
			return errors.New("invalid suspended container identity")
		}
		project, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, container.ID)
		if err != nil {
			return err
		}
		if project != "cove-"+workload {
			return errors.New("suspended container belongs to another workload")
		}
		policy, err := container.restartArgument()
		if err != nil {
			return err
		}
		if err := e.Run(ctx, e.Root, []string{"docker", "update", "--restart=" + policy, container.ID}, nil, out); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) survivingSuspension(ctx context.Context, workload string, containers []suspensionContainer) ([]suspensionContainer, error) {
	current, err := e.Containers(ctx, workload)
	if err != nil {
		return nil, err
	}
	present := []suspensionContainer{}
	for _, container := range containers {
		if !containerIdentity.MatchString(container.ID) {
			return nil, errors.New("invalid suspended container identity")
		}
		for _, now := range current {
			if !containerIdentity.MatchString(now.ID) {
				return nil, errors.New("invalid live suspension container identity")
			}
			if strings.HasPrefix(container.ID, now.ID) {
				present = append(present, container)
				break
			}
		}
	}
	return present, nil
}
