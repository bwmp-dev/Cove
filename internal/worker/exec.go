package worker

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type ExecRequest struct {
	Workload       string   `json:"workload"`
	Service        string   `json:"service"`
	Container      string   `json:"container,omitempty"`
	Command        []string `json:"command"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}
type ExecResult struct {
	Output    string `json:"output"`
	ExitCode  int    `json:"exit_code"`
	Truncated bool   `json:"truncated"`
	TimedOut  bool   `json:"timed_out"`
}

func ValidateExec(r ExecRequest) error {
	if !workloadIdentifier.MatchString(r.Workload) || !serviceIdentifier.MatchString(r.Service) || (r.Container != "" && !regexp.MustCompile(`^[a-f0-9]{12,64}$`).MatchString(r.Container)) || len(r.Command) == 0 || len(r.Command) > 64 || r.TimeoutSeconds < 1 || r.TimeoutSeconds > 30 {
		return errors.New("workload, service, command and timeout of 1-30 seconds required")
	}
	size := 0
	for _, arg := range r.Command {
		size += len(arg)
		if strings.ContainsRune(arg, 0) || len(arg) > 4096 {
			return errors.New("invalid command argument")
		}
	}
	if r.Command[0] == "" || size > 16384 {
		return errors.New("command exceeds limit")
	}
	return nil
}

type execOutput struct {
	data      bytes.Buffer
	truncated bool
}

func (b *execOutput) Write(p []byte) (int, error) {
	size := len(p)
	remaining := 65536 - b.data.Len()
	if size > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	b.data.Write(p)
	return size, nil
}
func (e *Engine) Exec(ctx context.Context, r ExecRequest) (ExecResult, error) {
	if err := ValidateExec(r); err != nil {
		return ExecResult{}, err
	}
	e.mu.Lock()
	if e.active[r.Workload] || e.execRunning >= 4 {
		e.mu.Unlock()
		return ExecResult{}, errors.New("resource is busy or command limit reached")
	}
	e.active[r.Workload] = true
	e.execRunning++
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, r.Workload); e.execRunning--; e.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.TimeoutSeconds)*time.Second)
	defer cancel()
	selection := &limitedBuffer{Limit: 8192}
	args := []string{"docker", "ps", "--no-trunc", "--filter", "label=com.docker.compose.project=" + e.composeProject(r.Workload), "--filter", "label=com.docker.compose.service=" + r.Service, "--format", "{{.ID}}"}
	if err := e.Run(ctx, e.Root, args, nil, selection); err != nil {
		return ExecResult{}, errors.New("could not inspect resource containers")
	}
	matches := strings.Fields(selection.data.String())
	container := ""
	count := 0
	for _, id := range matches {
		if !regexp.MustCompile(`^[a-f0-9]{12,64}$`).MatchString(id) {
			return ExecResult{}, errors.New("invalid container identity")
		}
		if r.Container != "" && strings.HasPrefix(id, r.Container) {
			container = id
			count++
		}
	}
	if r.Container == "" && len(matches) == 1 {
		container = matches[0]
	}
	if count > 1 {
		return ExecResult{}, errors.New("container prefix is ambiguous")
	}
	if container == "" {
		return ExecResult{}, errors.New("select a running container in this resource service; replicas require a container ID")
	}
	output := &execOutput{}
	err := e.Run(ctx, e.Root, append([]string{"docker", "exec", container}, r.Command...), nil, output)
	code := 0
	if err != nil {
		code = 1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
	}
	return ExecResult{Output: output.data.String(), ExitCode: code, Truncated: output.truncated, TimedOut: ctx.Err() != nil}, nil
}
