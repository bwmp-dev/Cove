package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type DeploymentHook struct {
	Service string `json:"service"`
	Command string `json:"command"`
}
type DeploymentHooks struct {
	Before      *DeploymentHook       `json:"before,omitempty"`
	After       *DeploymentHook       `json:"after,omitempty"`
	BeforeSteps []DeploymentHook      `json:"before_steps,omitempty"`
	Assertions  *DeploymentAssertions `json:"assertions,omitempty"`
}

func validateHooks(hooks *DeploymentHooks, kind string) error {
	if hooks == nil {
		return nil
	}
	if kind == "job" && (hooks.Before != nil || hooks.After != nil || len(hooks.BeforeSteps) > 0 || hooks.Assertions != nil) {
		return errors.New("deployment hooks require a service resource")
	}
	for _, hook := range []*DeploymentHook{hooks.Before, hooks.After} {
		if hook != nil && (!serviceIdentifier.MatchString(hook.Service) || strings.TrimSpace(hook.Command) == "" || len(hook.Command) > 8192 || strings.ContainsRune(hook.Command, 0)) {
			return errors.New("hooks require a Compose service and a command of at most 8192 characters")
		}
	}
	if len(hooks.BeforeSteps) > 8 {
		return errors.New("at most eight pre-deployment steps are supported")
	}
	for i := range hooks.BeforeSteps {
		if err := validateHooks(&DeploymentHooks{Before: &hooks.BeforeSteps[i]}, kind); err != nil {
			return err
		}
	}
	if err := validateDeploymentAssertions(hooks.Assertions); err != nil {
		return err
	}
	return nil
}

func (e *Engine) runBeforeHooks(ctx context.Context, dir, operation string, args, env []string, hooks *DeploymentHooks, out io.Writer) error {
	if err := e.checkDeploymentAssertions(ctx, dir, args, env, hooks.Assertions); err != nil {
		return err
	}
	for i := range hooks.BeforeSteps {
		fmt.Fprintf(out, "Running pre-deployment check %d of %d.\n", i+1, len(hooks.BeforeSteps))
		if err := e.runHook(ctx, dir, operation, args, env, &hooks.BeforeSteps[i], true, out); err != nil {
			return err
		}
	}
	return e.runHook(ctx, dir, operation, args, env, hooks.Before, true, out)
}

func (e *Engine) runHook(ctx context.Context, dir, operation string, args, env []string, hook *DeploymentHook, before bool, out io.Writer) (result error) {
	if hook == nil {
		return nil
	}
	if err := writeJSON(filepath.Join(dir, "pending-hook.json"), operation); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	command := append([]string{}, args...)
	phase := "post"
	if before {
		phase = "pre"
	}
	fmt.Fprintf(out, "Running %s-deployment command in a temporary container from the new image; dependencies are not started.\n", phase)
	command = append(command, "run", "--rm", "--no-deps", "--no-TTY", "--name", "cove-hook-"+operation, "--label", "dev.cove.hook="+operation, "--entrypoint", "sh", hook.Service, "-ec", hook.Command)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		result = errors.Join(result, e.recoverHook(cleanup, dir))
	}()
	if err := e.Run(ctx, dir, command, env, out); err != nil {
		if before {
			return errors.New("pre-deployment command failed; the application replacement was not started")
		}
		return errors.New("post-deployment command failed; the new release remains deployed; database changes are not rolled back")
	}
	return nil
}

func (e *Engine) recoverHook(ctx context.Context, dir string) error {
	path := filepath.Join(dir, "pending-hook.json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var id string
	if json.Unmarshal(raw, &id) != nil || !identifier.MatchString(id) {
		return errors.New("invalid hook recovery marker")
	}
	if err = e.cleanupHook(ctx, id); err != nil {
		return err
	}
	return os.Remove(path)
}

func (e *Engine) cleanupHook(ctx context.Context, id string) error {
	raw, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "label=dev.cove.hook="+id)
	if err != nil {
		return err
	}
	ids := strings.Fields(raw)
	if len(ids) == 0 {
		return nil
	}
	return e.Run(ctx, e.Root, append([]string{"docker", "rm", "--force", "--volumes"}, ids...), nil, io.Discard)
}
