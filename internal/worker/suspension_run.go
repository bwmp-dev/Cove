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

var errSuspensionUnresolved = errors.New("workload suspension is still being reconciled")

type suspensionChange struct {
	Action            string             `json:"action"`
	Next              workloadSuspension `json:"next"`
	Manual            bool               `json:"manual,omitempty"`
	Complete          bool               `json:"complete"`
	ExposureCommitted bool               `json:"exposure_committed"`
	Failure           string             `json:"failure,omitempty"`
}

func (c suspensionChange) result() *SuspensionStatus {
	status := c.Next.status()
	if c.Complete && c.Action == "resume" && c.Failure == "" {
		if c.Next.Options.Reason == "manual_stop" && c.Next.Phase == "paused" {
			status.Phase = "stopped"
		} else {
			status.Phase = "resumed"
		}
	}
	return status
}

func validateSuspensionRequest(r Request) error {
	if r.Suspension == nil {
		if r.Action == "suspend" || r.Action == "resume" {
			return errors.New("suspension options required")
		}
		return nil
	}
	if r.Action != "suspend" && r.Action != "resume" || r.Suspension.Reason != "environment" || !identifier.MatchString(r.Suspension.Scope) || r.Action == "suspend" && r.Suspension.Start || r.Kind == "job" && r.Suspension.Start {
		return errors.New("invalid environment suspension request")
	}
	return nil
}

func (e *Engine) saveSuspensionChange(id string, change *suspensionChange) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err := e.read(id)
	if err != nil {
		return err
	}
	j.SuspensionChange, j.Operation.Suspension = change, change.result()
	return writeJSON(e.path(id), j)
}

func (e *Engine) beginSuspension(ctx context.Context, r Request, manual bool) (*suspensionChange, error) {
	e.mu.Lock()
	j, err := e.read(r.ID)
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if j.SuspensionChange != nil {
		return j.SuspensionChange, nil
	}
	prior, err := e.readSuspension(r.Workload)
	if err != nil {
		return nil, err
	}
	change := &suspensionChange{Action: r.Action, Manual: manual}
	if manual {
		change.Action = "resume"
	}
	if change.Action == "suspend" {
		captured, err := e.captureSuspension(ctx, r.Workload)
		if err != nil {
			return nil, err
		}
		change.Next = workloadSuspension{Operation: r.ID, Options: *r.Suspension, Phase: "pausing", Created: time.Now().UTC(), Containers: captured}
		if r.Suspension.Reason == "idle" {
			change.Next.IdleEpisode = r.ID
		}
		if prior != nil {
			if prior.Options.Reason == "idle" && r.Suspension.Reason == "environment" {
				change.Next.Containers = prior.Containers
				change.Next.BeforeIdle = &idleSuspensionOrigin{Operation: prior.idleEpisode(), Created: prior.Created}
				change.Next.ManualStop = prior.ManualStop
			}
			if prior.Options.Reason == "environment" && prior.Options.Scope != r.Suspension.Scope {
				return nil, errors.New("workload is suspended by another environment")
			}
			if prior.Options.Reason == "environment" && prior.Phase == "paused" {
				change.Next = *prior
				change.Next.Operation = r.ID
				change.Next.Phase = "pausing"
			} else {
				for i, container := range change.Next.Containers {
					for _, saved := range prior.Containers {
						if container.ID == saved.ID {
							change.Next.Containers[i].Restart, change.Next.Containers[i].Retries = saved.Restart, saved.Retries
						}
					}
				}
			}
		}
	} else {
		if prior == nil {
			if r.Suspension == nil {
				return nil, errors.New("there is no suspended workload to resume")
			}
			if r.Suspension.Start {
				return nil, errors.New("the suspended container snapshot is unavailable; redeploy the resource to recreate it")
			}
			change.Next = workloadSuspension{Operation: r.ID, Options: *r.Suspension, Phase: "awake", Created: time.Now().UTC()}
			change.Complete, change.ExposureCommitted = true, true
		} else {
			if !manual && prior.Options.Scope != r.Suspension.Scope {
				return nil, errors.New("suspension belongs to another environment")
			}
			change.Next = *prior
			change.Next.Operation = r.ID
			change.Next.Phase = "resuming"
			change.Next.Deadline = time.Now().Add(2 * time.Minute).UTC()
			change.Next.Options.Start = manual || r.Suspension.Start && !prior.ManualStop
			if manual {
				change.Next.ManualStop = false
			}
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err = e.read(r.ID)
	if err != nil {
		return nil, err
	}
	if j.CancelRequested {
		return nil, context.Canceled
	}
	j.SuspensionChange, j.Operation.Suspension = change, change.result()
	if err := writeJSON(e.path(r.ID), j); err != nil {
		return nil, err
	}
	if !change.Complete {
		if err := writeJSON(filepath.Join(e.Root, "workloads", r.Workload, "suspension.json"), change.Next); err != nil {
			return nil, errors.Join(errSuspensionUnresolved, err)
		}
	}
	return change, nil
}

func (e *Engine) suspensionCancelled(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err := e.read(id)
	return err == nil && j.CancelRequested
}

func (e *Engine) confirmSuspensionIngress(ctx context.Context, workload string, out io.Writer) error {
	state, released, err := readCurrent(filepath.Join(e.Root, "workloads", workload, "current.json"))
	if err != nil {
		return err
	}
	if !released {
		return nil
	}
	return e.applyIngress(ctx, workload, state.Routes, out)
}

func (e *Engine) finishSuspensionPause(ctx context.Context, r Request, change *suspensionChange, out io.Writer) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	if err := writeJSON(filepath.Join(e.Root, "workloads", r.Workload, "suspension.json"), change.Next); err != nil {
		return errors.Join(errSuspensionUnresolved, err)
	}
	if err := e.stopSuspensionContainers(cleanup, r.Workload, change.Next.Containers, out); err != nil {
		return errors.Join(errSuspensionUnresolved, err)
	}
	change.Next.Phase = "paused"
	if err := writeJSON(filepath.Join(e.Root, "workloads", r.Workload, "suspension.json"), change.Next); err != nil {
		return errors.Join(errSuspensionUnresolved, err)
	}
	if err := e.confirmSuspensionIngress(cleanup, r.Workload, out); err != nil {
		return errors.Join(errSuspensionUnresolved, err)
	}
	change.Complete = true
	if err := e.saveSuspensionChange(r.ID, change); err != nil {
		return errors.Join(errSuspensionUnresolved, err)
	}
	return nil
}

func (e *Engine) finishSuspensionResume(ctx context.Context, r Request, change *suspensionChange, out io.Writer) error {
	if !change.ExposureCommitted {
		if !change.Next.Options.Start {
			if change.Next.BeforeIdle != nil && !change.Next.ManualStop {
				change.Next.Operation, change.Next.Created = change.Next.BeforeIdle.Operation, change.Next.BeforeIdle.Created
				change.Next.Options = SuspensionOptions{Reason: "idle", Scope: r.Workload}
				change.Next.IdleEpisode = change.Next.Operation
				change.Next.BeforeIdle = nil
				change.ExposureCommitted = true
				return e.finishSuspensionPause(ctx, r, change, out)
			}
			change.Next.Options.Reason, change.Next.ManualStop = "manual_stop", true
			change.ExposureCommitted = true
			return e.finishSuspensionPause(ctx, r, change, out)
		}
		start, cancel := context.WithDeadline(ctx, change.Next.Deadline)
		var startErr error
		if e.suspensionCancelled(r.ID) {
			startErr = context.Canceled
		} else {
			present, err := e.survivingSuspension(ctx, r.Workload, change.Next.Containers)
			startErr = err
			if startErr == nil {
				available := map[string]bool{}
				for _, container := range present {
					available[container.ID] = true
				}
				ids := []string{}
				for _, container := range change.Next.Containers {
					if container.WasRunning || change.Manual && !container.Completed {
						if !available[container.ID] {
							startErr = errors.New("a previously running container was removed; redeploy the resource to recreate it")
							break
						}
						ids = append(ids, container.ID)
					}
				}
				if startErr == nil {
					startErr = e.startBackupContainers(start, ids, out)
				}
				if startErr == nil {
					startErr = e.waitSuspensionHealth(start, r.Workload, ids)
				}
				if startErr == nil {
					startErr = e.restoreSuspensionPolicies(start, r.Workload, present, out)
				}
				if startErr == nil {
					for _, container := range present {
						if container.Paused {
							var paused bool
							if err := e.imageJSON(start, nil, &paused, "docker", "inspect", "--format", "{{json .State.Paused}}", container.ID); err != nil {
								startErr = err
								break
							}
							if !paused {
								if err := e.Run(start, e.Root, []string{"docker", "pause", container.ID}, nil, out); err != nil {
									startErr = err
									break
								}
							}
						}
					}
				}
			}
		}
		deadline := errors.Is(start.Err(), context.DeadlineExceeded)
		cancel()
		if ctx.Err() != nil && !e.suspensionCancelled(r.ID) && !deadline {
			return errSuspensionUnresolved
		}
		if startErr == nil && e.suspensionCancelled(r.ID) {
			startErr = context.Canceled
		}
		if startErr != nil {
			change.Next.Phase = "pausing"
			change.Failure = "Resume failed; the captured containers remain stopped. Inspect the operation output."
			fmt.Fprintln(out, startErr)
			if err := e.saveSuspensionChange(r.ID, change); err != nil {
				return errors.Join(errSuspensionUnresolved, err)
			}
			if err := e.finishSuspensionPause(ctx, r, change, out); err != nil {
				return err
			}
			return errors.New(change.Failure)
		}
		e.mu.Lock()
		j, err := e.read(r.ID)
		if err == nil && j.CancelRequested {
			err = context.Canceled
		}
		if err == nil {
			change.ExposureCommitted = true
			change.Next.Phase = "awake"
			j.SuspensionChange, j.Operation.Suspension = change, change.result()
			err = writeJSON(e.path(r.ID), j)
		}
		e.mu.Unlock()
		if err != nil {
			return errors.Join(errSuspensionUnresolved, err)
		}
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	path := filepath.Join(e.Root, "workloads", r.Workload, "suspension.json")
	if err := writeJSON(path, change.Next); err != nil {
		return errors.Join(errSuspensionUnresolved, err)
	}
	if err := e.confirmSuspensionIngress(cleanup, r.Workload, out); err != nil {
		return errors.Join(errSuspensionUnresolved, err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return errors.Join(errSuspensionUnresolved, err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return errors.Join(errSuspensionUnresolved, err)
	}
	change.Complete = true
	if err := e.saveSuspensionChange(r.ID, change); err != nil {
		return errors.Join(errSuspensionUnresolved, err)
	}
	return nil
}

func (e *Engine) waitSuspensionHealth(ctx context.Context, workload string, ids []string) error {
	for {
		containers, err := e.HealthContainers(ctx, workload)
		if err != nil {
			return err
		}
		ready := true
		for _, id := range ids {
			found := false
			for _, container := range containers {
				if !strings.HasPrefix(id, container.ID) {
					continue
				}
				found = true
				if container.State != "running" && container.State != "paused" {
					return errors.New("a resumed container stopped before readiness")
				}
				if container.Health != "" && container.Health != "healthy" {
					ready = false
				}
			}
			if !found {
				return errors.New("a resumed container disappeared before readiness")
			}
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (e *Engine) performSuspension(ctx context.Context, r Request, manual bool, out io.Writer) error {
	change, err := e.beginSuspension(ctx, r, manual)
	if err != nil {
		return err
	}
	if change.Complete {
		if change.Failure != "" {
			return errors.New(change.Failure)
		}
		return nil
	}
	if change.Failure != "" {
		if err := e.finishSuspensionPause(ctx, r, change, out); err != nil {
			return err
		}
		return errors.New(change.Failure)
	}
	if change.Action == "suspend" {
		return e.finishSuspensionPause(ctx, r, change, out)
	}
	return e.finishSuspensionResume(ctx, r, change, out)
}

func (e *Engine) settleSuspension(ctx context.Context, id string) error {
	e.mu.Lock()
	j, err := e.read(id)
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if j.Operation.State != "running" {
		return nil
	}
	recovery, cancel := context.WithCancel(ctx)
	defer cancel()
	e.mu.Lock()
	e.cancels[id] = cancel
	e.mu.Unlock()
	e.execute(recovery, j)
	status, err := e.Status(id)
	if err != nil {
		return err
	}
	if status.State == "running" {
		return errSuspensionUnresolved
	}
	return nil
}

func (e *Engine) pruneSuspension(workload string, change *rolloutChange) error {
	state, err := e.readSuspension(workload)
	if err != nil || state == nil {
		return err
	}
	retained := []suspensionContainer{}
	for _, container := range state.Containers {
		old := false
		for _, id := range change.OldContainers {
			if strings.HasPrefix(container.ID, id) {
				old = true
				break
			}
		}
		if old != change.Committed {
			retained = append(retained, container)
		}
	}
	state.Containers = retained
	return writeJSON(filepath.Join(e.Root, "workloads", workload, "suspension.json"), state)
}

func (e *Engine) suspensionGate(ctx context.Context, r Request, out io.Writer) (bool, error) {
	e.mu.Lock()
	j, err := e.read(r.ID)
	e.mu.Unlock()
	if err != nil {
		return true, err
	}
	if j.SuspensionChange != nil {
		return true, e.performSuspension(ctx, r, j.SuspensionChange.Manual, out)
	}
	if r.Action == "suspend" || r.Action == "resume" {
		return true, e.performSuspension(ctx, r, false, out)
	}
	state, err := e.readSuspension(r.Workload)
	if err != nil {
		return true, err
	}
	if state == nil || state.Phase == "awake" {
		if r.Action == "stop" {
			idle, err := e.readIdle(r.Workload)
			if err != nil {
				return true, err
			}
			if idle != nil {
				pause := r
				pause.Action = "suspend"
				pause.Suspension = &SuspensionOptions{Reason: "manual_stop", Scope: r.Workload}
				return true, e.performSuspension(ctx, pause, false, out)
			}
		}
		return false, nil
	}
	if r.Action == "stop" {
		state.ManualStop = true
		return false, writeJSON(filepath.Join(e.Root, "workloads", r.Workload, "suspension.json"), state)
	}
	if r.Action == "start" || r.Action == "restart" {
		if state.Options.Reason == "manual_stop" || state.Options.Reason == "idle" || r.ResumeSuspended {
			return true, e.performSuspension(ctx, r, true, out)
		}
	}
	if r.Action == "deploy" && state.Options.Reason == "idle" {
		return true, errors.New("start the sleeping resource before deploying a replacement")
	}
	if r.Action == "deploy" && (state.Options.Reason == "manual_stop" || state.Options.Reason == "idle" || r.ResumeSuspended) {
		present, err := e.survivingSuspension(ctx, r.Workload, state.Containers)
		if err != nil {
			return true, err
		}
		if r.Kind != "job" {
			if err := e.restoreSuspensionPolicies(ctx, r.Workload, present, out); err != nil {
				return true, err
			}
		}
		path := filepath.Join(e.Root, "workloads", r.Workload, "suspension.json")
		if err := os.Remove(path); err != nil {
			return true, err
		}
		return false, syncDirectory(filepath.Dir(path))
	}
	if r.Action == "image_check" || r.Action == "image_scan" || r.Action == "build" || r.Action == "restart_unhealthy" {
		return false, nil
	}
	if r.Action == "backup" || r.Action == "restore" && r.ResumeSuspended {
		return false, nil
	}
	return true, errors.New("resume the environment before starting or replacing this suspended resource")
}
