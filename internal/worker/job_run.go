package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const DefaultJobTimeoutSeconds = 1800

type JobOptions struct {
	TimeoutSeconds int `json:"timeout_seconds"`
}

type JobResult struct {
	ExitCode          *int `json:"exit_code,omitempty"`
	TimedOut          bool `json:"timed_out"`
	ContainersStopped bool `json:"containers_stopped"`
	LogsUnavailable   bool `json:"logs_unavailable,omitempty"`
}

type jobRun struct {
	Started         time.Time     `json:"started"`
	Deadline        time.Time     `json:"deadline"`
	State           deployedState `json:"state"`
	Legacy          bool          `json:"legacy,omitempty"`
	Finished        bool          `json:"finished"`
	ExitCode        *int          `json:"exit_code,omitempty"`
	TimedOut        bool          `json:"timed_out"`
	Cleaned         bool          `json:"cleaned"`
	LogsCaptured    bool          `json:"logs_captured"`
	LogsUnavailable bool          `json:"logs_unavailable,omitempty"`
}

var errJobUnresolved = errors.New("job execution or container cleanup is still being reconciled")

func jobTimeout(options *JobOptions) int {
	if options == nil || options.TimeoutSeconds == 0 {
		return DefaultJobTimeoutSeconds
	}
	return options.TimeoutSeconds
}

func (e *Engine) saveJobRun(id string, run *jobRun) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err := e.read(id)
	if err != nil {
		return err
	}
	j.JobRun = run
	j.Operation.Job = &JobResult{ExitCode: run.ExitCode, TimedOut: run.TimedOut, ContainersStopped: run.Cleaned, LogsUnavailable: run.LogsUnavailable}
	return writeJSON(e.path(id), j)
}

func (e *Engine) readJobRun(id string) (journal, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.read(id)
}

func (e *Engine) jobContainers(ctx context.Context, j journal, main bool) ([]string, error) {
	args := []string{"docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-" + j.Request.Workload, "--filter", "label=com.docker.compose.oneoff=False"}
	if !j.JobRun.Legacy {
		args = append(args, "--filter", "label=dev.cove.job-run="+j.Request.ID)
	}
	if main {
		args = append(args, "--filter", "label=com.docker.compose.service="+j.Request.JobService)
	}
	text, err := e.output(ctx, args...)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(text)
	for _, id := range ids {
		if !containerIdentity.MatchString(id) {
			return nil, errors.New("invalid job container identity")
		}
	}
	return ids, nil
}

func (e *Engine) jobExit(ctx context.Context, j journal) (*int, bool, error) {
	ids, err := e.jobContainers(ctx, j, true)
	if err != nil || len(ids) == 0 {
		return nil, false, err
	}
	var code *int
	for _, id := range ids {
		var state struct {
			Running bool   `json:"running"`
			Exit    int    `json:"exit"`
			Status  string `json:"status"`
		}
		if err := e.imageJSON(ctx, nil, &state, "docker", "inspect", "--format", `{"running":{{json .State.Running}},"exit":{{json .State.ExitCode}},"status":{{json .State.Status}}}`, id); err != nil {
			return nil, false, err
		}
		if state.Running || state.Status == "restarting" || state.Status == "paused" {
			return nil, true, nil
		}
		if state.Status != "exited" && state.Status != "dead" {
			return nil, false, nil
		}
		if code == nil || state.Exit != 0 {
			value := state.Exit
			code = &value
		}
	}
	return code, false, nil
}

func (e *Engine) cleanJobRun(ctx context.Context, j journal, out io.Writer) error {
	ids, err := e.jobContainers(ctx, j, false)
	if err != nil {
		return err
	}
	for _, id := range ids {
		project, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, id)
		if err != nil {
			return err
		}
		if project != "cove-"+j.Request.Workload {
			return errors.New("job container belongs to another workload")
		}
	}
	if len(ids) > 0 {
		if err := e.Run(ctx, e.Root, append([]string{"docker", "update", "--restart=no"}, ids...), nil, out); err != nil {
			return err
		}
		args := append([]string{"docker", "stop", "--time", "10"}, ids...)
		if err := e.Run(ctx, e.Root, args, nil, out); err != nil {
			return err
		}
		for _, id := range ids {
			var state struct{ Running, Restarting bool }
			if err := e.imageJSON(ctx, nil, &state, "docker", "inspect", "--format", `{"running":{{json .State.Running}},"restarting":{{json .State.Restarting}}}`, id); err != nil {
				return err
			}
			if state.Running || state.Restarting {
				return errors.New("job container cleanup is not confirmed")
			}
		}
	}
	if !j.JobRun.LogsCaptured {
		for _, id := range ids {
			if err := e.Run(ctx, e.Root, []string{"docker", "logs", "--timestamps", "--tail", "200", id}, nil, out); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				j.JobRun.LogsUnavailable = true
				fmt.Fprintln(out, "Container output is unavailable:", err)
			}
		}
		j.JobRun.LogsCaptured = true
	}
	j.JobRun.Cleaned = true
	return e.saveJobRun(j.Request.ID, j.JobRun)
}

func (e *Engine) performJob(prepare, parent context.Context, r Request, dir string, args, env []string, out io.Writer) error {
	resolved := &limitedBuffer{Limit: 2 << 20}
	if err := e.Run(prepare, dir, append(append([]string{}, args...), "config", "--format", "json"), env, resolved); err != nil {
		return err
	}
	var config map[string]any
	if err := decodeResolvedCompose(resolved.data.Bytes(), &config); err != nil {
		return err
	}
	services, ok := config["services"].(map[string]any)
	if !ok || services[r.JobService] == nil {
		return errors.New("job service is absent from Compose")
	}
	for _, raw := range services {
		service, ok := raw.(map[string]any)
		if !ok {
			return errors.New("invalid job Compose service")
		}
		labels, _ := service["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
		}
		labels["dev.cove.job-run"] = r.ID
		service["labels"] = labels
		service["restart"] = "no"
		if deploy, ok := service["deploy"].(map[string]any); ok {
			delete(deploy, "restart_policy")
		}
	}
	path := filepath.Join(dir, r.ID+".job.compose.json")
	if err := writeJSON(path, literalCompose(config)); err != nil {
		return err
	}
	now := time.Now().UTC()
	run := &jobRun{Started: now, Deadline: now.Add(time.Duration(jobTimeout(r.Job)) * time.Second), State: deployedState{ComposePath: path, Environment: r.Environment, Networks: r.Networks}}
	if err := e.saveJobRun(r.ID, run); err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(parent, run.Deadline)
	command := []string{"docker", "compose", "--project-name", "cove-" + r.Workload, "--file", path, "up", "--detach", "--no-build", "--remove-orphans"}
	executionErr := e.Run(ctx, dir, command, env, out)
	for executionErr == nil {
		j, err := e.readJobRun(r.ID)
		if err != nil {
			executionErr = err
			break
		}
		_, running, err := e.jobExit(ctx, j)
		if err != nil || !running {
			executionErr = err
			break
		}
		select {
		case <-ctx.Done():
			executionErr = ctx.Err()
		case <-time.After(time.Second):
		}
	}
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	cancel()
	j, err := e.readJobRun(r.ID)
	if err != nil {
		return errors.Join(errJobUnresolved, err)
	}
	if parent.Err() != nil && !j.CancelRequested && !timedOut {
		return errJobUnresolved
	}
	cleanup, done := context.WithTimeout(context.WithoutCancel(parent), 3*time.Minute)
	defer done()
	code, running, err := e.jobExit(cleanup, j)
	if err != nil || running && !j.CancelRequested && !timedOut {
		return errors.Join(errJobUnresolved, err)
	}
	run.Finished, run.ExitCode, run.TimedOut = true, code, timedOut
	if err := e.saveJobRun(r.ID, run); err != nil {
		return errors.Join(errJobUnresolved, err)
	}
	j.JobRun = run
	if err := e.cleanJobRun(cleanup, j, out); err != nil {
		return errors.Join(errJobUnresolved, err)
	}
	if timedOut {
		return errors.New("job execution timed out; owned run containers were stopped")
	}
	if j.CancelRequested {
		return errors.New("job cancelled; owned run containers were stopped")
	}
	if code == nil || *code != 0 {
		return errors.Join(errors.New("job did not complete successfully"), executionErr)
	}
	return e.commitJobState(cleanup, r.Workload, run.State, out)
}

func (e *Engine) commitJobState(ctx context.Context, workload string, state deployedState, out io.Writer) error {
	if err := e.applyIngress(ctx, workload, nil, out); err != nil {
		return err
	}
	return writeJSON(filepath.Join(e.Root, "workloads", workload, "current.json"), state)
}

func (e *Engine) settleJob(ctx context.Context, id, interrupted string) error {
	j, err := e.readJobRun(id)
	if err != nil || j.JobRun == nil || j.Operation.State != "running" {
		return err
	}
	if !workloadIdentifier.MatchString(j.Request.Workload) || j.Request.ID != id || !serviceIdentifier.MatchString(j.Request.JobService) {
		return errors.New("invalid job recovery identity")
	}
	for !j.JobRun.Finished && !j.JobRun.Legacy {
		if j.CancelRequested || !time.Now().Before(j.JobRun.Deadline) {
			j.JobRun.TimedOut = !j.CancelRequested
			break
		}
		code, running, err := e.jobExit(ctx, j)
		if err != nil {
			return err
		}
		if !running {
			j.JobRun.ExitCode, j.JobRun.Finished = code, true
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		j, err = e.readJobRun(id)
		if err != nil {
			return err
		}
	}
	file, err := os.OpenFile(filepath.Join(e.Root, "operations", id+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	log := &boundedLog{file: file, remaining: max(0, 4<<20-info.Size()), publicIdentifiers: []string{"cove-" + j.Request.Workload}}
	for _, value := range j.Request.Environment {
		log.secrets = append(log.secrets, value)
	}
	if j.Request.Source != nil {
		log.secrets = append(log.secrets, j.Request.Source.Token)
	}
	if j.Request.Registry != nil {
		log.secrets = append(log.secrets, j.Request.Registry.Password)
	}
	cleanup, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := e.saveJobRun(id, j.JobRun); err != nil {
		return err
	}
	if !j.JobRun.Cleaned {
		if err := e.cleanJobRun(cleanup, j, log); err != nil {
			return err
		}
	}
	if err := log.flush(true); err != nil {
		return err
	}
	if !j.CancelRequested && !j.JobRun.TimedOut && !j.JobRun.Legacy && j.JobRun.ExitCode != nil && *j.JobRun.ExitCode == 0 {
		if err := e.commitJobState(cleanup, j.Request.Workload, j.JobRun.State, log); err != nil {
			return err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	persisted, err := e.read(id)
	if err != nil {
		return err
	}
	finish := time.Now().UTC()
	persisted.Operation.FinishedAt = &finish
	persisted.Operation.State, persisted.Operation.Error = "failed", "Job failed; inspect the operation output."
	if persisted.CancelRequested {
		persisted.Operation.State, persisted.Operation.Error = "cancelled", "Job cancelled; owned run containers were stopped."
	} else if persisted.JobRun.TimedOut {
		persisted.Operation.Error = "Job exceeded its execution timeout; its containers were stopped."
	} else if persisted.JobRun.Legacy || persisted.JobRun.ExitCode == nil {
		persisted.Operation.State, persisted.Operation.Error = interrupted, "The previous job outcome is unknown; its containers were stopped before releasing the workload lock."
	} else if *persisted.JobRun.ExitCode == 0 {
		persisted.Operation.State, persisted.Operation.Error = "succeeded", ""
	}
	fmt.Fprintln(log, "Recovered job:", persisted.Operation.State)
	if err := log.flush(true); err != nil {
		return err
	}
	return writeJSON(e.path(id), persisted)
}
