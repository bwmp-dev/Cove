package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func (e *Engine) guardedRolloutFallback(ctx context.Context, r Request, dir string, args, env []string, out io.Writer, reason string) (result error) {
	if r.Rollout != nil && len(r.Rollout.Replicas) > 0 {
		reason += " Replica overrides apply only to eligible parallel replacements; Compose scaling is used in guarded mode."
	}
	fmt.Fprintln(out, "Guarded replacement:", reason)
	resolved := &limitedBuffer{Limit: 2 << 20}
	if err := e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, resolved); err != nil {
		return err
	}
	var config map[string]any
	if err := decodeResolvedCompose(resolved.data.Bytes(), &config); err != nil {
		return err
	}
	for _, raw := range config["services"].(map[string]any) {
		service := raw.(map[string]any)
		labels, _ := service["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
		}
		labels["dev.cove.generation"] = r.ID
		service["labels"] = labels
	}
	path := filepath.Join(dir, r.ID+".guarded-replacement.compose.json")
	if err := writeJSON(path, literalCompose(config)); err != nil {
		return err
	}
	previous, released, err := readCurrent(filepath.Join(dir, "current.json"))
	if err != nil {
		return err
	}
	priorTraffic, err := e.loadTraffic(r.Workload)
	if err != nil {
		return err
	}
	options := RolloutOptions{Strategy: "guarded", TimeoutSeconds: 120, CanaryPercent: 10}
	change := rolloutChange{Origin: r.ID, Phase: "replacing", PreviousTraffic: priorTraffic, Options: options, Next: deployedState{ComposePath: path, Environment: r.Environment, Routes: r.Routes}}
	if released {
		change.Previous = &previous
	}
	if err = e.updateRolloutJournal(r.ID, change); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(dir, "rollout.json"), r.ID); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		if err := e.finishRollout(cleanup, r.Workload, r.ID, &change, out); err != nil {
			result = errors.Join(result, errRolloutUnresolved, err)
		}
	}()
	old, err := e.Containers(ctx, r.Workload)
	if err != nil {
		return err
	}
	for _, container := range old {
		if err = e.Run(ctx, dir, []string{"docker", "stop", "--time", "30", container.ID}, nil, out); err != nil {
			return err
		}
	}
	command := []string{"docker", "compose", "--project-name", "cove-" + r.Workload, "--file", path}
	if err = e.Run(ctx, dir, append(append([]string{}, command...), "up", "--detach", "--no-build", "--wait", "--wait-timeout", "120", "--remove-orphans"), env, out); err != nil {
		return err
	}
	if r.Rollout != nil && len(r.Rollout.HTTPChecks) > 0 {
		checks := r.Rollout.Normalized()
		checks.Replicas = map[string]int{}
		targets := trafficState{}
		for name, raw := range config["services"].(map[string]any) {
			if _, ok := checks.HTTPChecks[name]; !ok {
				continue
			}
			service := raw.(map[string]any)
			if scale, ok := service["scale"].(float64); ok {
				checks.Replicas[name] = int(scale)
			} else if deploy, ok := service["deploy"].(map[string]any); ok {
				if count, ok := deploy["replicas"].(float64); ok {
					checks.Replicas[name] = int(count)
				}
			}
			targets[name] = []trafficTarget{{Service: name, Weight: 100}}
		}
		if err := e.waitRolloutHealth(ctx, Request{ID: r.ID, Workload: r.Workload, Rollout: &checks}, targets); err != nil {
			return err
		}
	}
	state, err := e.captureRelease(ctx, r, dir, command, env)
	if err != nil {
		return err
	}
	state.Routes, state.Networks, state.Private, state.Rollout = r.Routes, r.Networks, r.Private, r.Rollout
	change.Next = state
	if r.Hooks != nil {
		if err = e.runHook(ctx, dir, r.ID, command, env, r.Hooks.After, false, out); err != nil {
			return err
		}
	}
	if err = e.updateRolloutJournal(r.ID, change); err != nil {
		return err
	}
	if err = e.commitRollout(ctx, r.Workload, r.ID, &change, out); err != nil {
		return err
	}
	return e.recordRolloutResult(r.ID, RolloutStatus{Operation: r.ID, Strategy: "guarded", Phase: "completed", CandidatePercent: 100, Reason: reason})
}

func (e *Engine) performRolloutAction(ctx context.Context, r Request, out io.Writer) (result error) {
	_, pending, err := e.pendingRollout(r.Workload)
	if err != nil {
		return err
	}
	if pending == nil || pending.Phase != "waiting" {
		return errors.New("there is no staged rollout to promote or abort")
	}
	change := *pending
	change.Staged = false
	current, released, err := readCurrent(filepath.Join(e.Root, "workloads", r.Workload, "current.json"))
	if err != nil {
		return err
	}
	if !released {
		return errors.New("current rollout release is unavailable")
	}
	change.Previous = &current
	change.PreviousTraffic = current.Traffic
	change.Next.Routes = current.Routes
	change.Phase = "promoting"
	if r.Action == "rollout_abort" {
		change.Phase = "aborting"
	}
	if err = e.updateRolloutJournal(r.ID, change); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(e.Root, "workloads", r.Workload, "rollout.json"), r.ID); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		if err := e.finishRollout(cleanup, r.Workload, r.ID, &change, out); err != nil {
			result = errors.Join(result, errRolloutUnresolved, err)
		} else if !change.Committed {
			result = errors.Join(result, e.recordRolloutResult(r.ID, RolloutStatus{Operation: r.ID, Release: change.Origin, Strategy: change.Options.Strategy, Phase: "aborted", CandidatePercent: 0}))
		}
	}()
	if r.Action == "rollout_abort" {
		return nil
	}
	health := Request{ID: r.ID, Workload: r.Workload, Rollout: &change.Options}
	if err = e.waitRolloutHealth(ctx, health, change.Next.Traffic); err != nil {
		return err
	}
	return e.commitRollout(ctx, r.Workload, r.ID, &change, out)
}

func (e *Engine) settleRollout(ctx context.Context, id, state string) error {
	j, err := e.read(id)
	if err != nil {
		return err
	}
	if j.RolloutChange == nil || j.Operation.State != "running" {
		return nil
	}
	cleanup, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if !j.RolloutChange.Staged {
		err = e.finishRollout(cleanup, j.Request.Workload, id, j.RolloutChange, io.Discard)
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err = e.read(id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	j.Operation.FinishedAt = &now
	if j.RolloutChange.Committed || j.RolloutChange.Staged || j.Request.Action == "rollout_abort" && !j.CancelRequested {
		j.Operation.State, j.Operation.Error = "succeeded", ""
	} else {
		j.Operation.State = state
		if j.CancelRequested {
			j.Operation.State = "cancelled"
		}
		j.Operation.Error = "Deployment did not commit; previous traffic was restored. Inspect the operation log."
	}
	if !j.RolloutChange.Staged && !j.RolloutChange.Committed {
		j.Operation.Rollout = &RolloutStatus{Operation: id, Release: j.RolloutChange.Origin, Strategy: j.RolloutChange.Options.Strategy, Phase: "aborted"}
	}
	return writeJSON(e.path(id), j)
}
