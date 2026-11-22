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

type idleChange struct {
	PriorSuspension *workloadSuspension `json:"prior_suspension,omitempty"`
	Previous        *idleWorkloadState  `json:"previous,omitempty"`
	Next            *idleWorkloadState  `json:"next,omitempty"`
	Committed       bool                `json:"committed"`
	Complete        bool                `json:"complete"`
}

func (e *Engine) saveIdleChange(id string, change *idleChange) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err := e.read(id)
	if err != nil {
		return err
	}
	j.IdleChange = change
	return writeJSON(e.path(id), j)
}

func (e *Engine) refreshIdleGateway(ctx context.Context, workload string, routes []Route) (*idleWorkloadState, error) {
	e.idleMu.Lock()
	defer e.idleMu.Unlock()
	state, err := e.readIdle(workload)
	if err != nil || state == nil {
		return state, err
	}
	suspension, err := e.readSuspension(workload)
	if err != nil {
		return nil, err
	}
	state.Config.Mode, state.Config.Episode = "awake", ""
	if suspension != nil && suspension.Phase == "awake" {
		state.Config.ActivityEpoch = suspension.Operation
	}
	if suspension != nil && suspension.Phase != "awake" {
		if suspension.Options.Reason != "idle" || suspension.ManualStop {
			return state, nil
		}
		state.Config.Mode, state.Config.Episode = "asleep", suspension.idleEpisode()
	}
	if err = e.prepareIdleGateway(ctx, workload, state, routes); err != nil {
		return nil, err
	}
	if err = e.writeIdle(workload, state); err != nil {
		return nil, err
	}
	return state, nil
}

func (e *Engine) removeIdleGateway(ctx context.Context, workload string) error {
	networks, err := e.helperNetworks(ctx, "idle")
	if err != nil {
		return err
	}
	ids, err := e.output(ctx, "docker", "ps", "-aq", "--no-trunc", "--filter", "name=^"+idleContainer(workload)+"$")
	if err != nil {
		return err
	}
	if ids != "" {
		var labels map[string]string
		if err = e.imageJSON(ctx, nil, &labels, "docker", "inspect", "--format", "{{json .Config.Labels}}", ids); err != nil {
			return err
		}
		if labels["cove.managed"] != "idle" || labels["cove.idle-workload"] != workload {
			return errors.New("cannot remove an unowned idle helper")
		}
		drain := time.NewTimer(30 * time.Second)
	draining:
		for {
			var status idleGatewayState
			code, readErr := e.idleControl(ctx, workload, "GET", "/status", nil, &status)
			if readErr != nil || code != 200 || status.Active == 0 {
				break
			}
			select {
			case <-ctx.Done():
				drain.Stop()
				return ctx.Err()
			case <-drain.C:
				break draining
			case <-time.After(100 * time.Millisecond):
			}
		}
		drain.Stop()
		if err = e.Run(ctx, e.Root, []string{"docker", "stop", "--timeout", "30", ids}, nil, io.Discard); err != nil {
			return err
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "rm", ids}, nil, io.Discard); err != nil {
			return err
		}
	}
	for _, role := range []string{"front", "back"} {
		network := networks[idleNetwork(workload, role)]
		if network == nil {
			continue
		}
		if network.Labels["cove.idle-workload"] != workload || network.Labels["cove.idle-role"] != role {
			return errors.New("cannot remove an unowned idle network")
		}
		for id := range network.Containers {
			if id == ids {
				continue
			}
			var labels map[string]string
			if err = e.imageJSON(ctx, nil, &labels, "docker", "inspect", "--format", "{{json .Config.Labels}}", id); err != nil {
				return err
			}
			if role == "front" && labels["cove.managed"] != "ingress" || role == "back" && labels["com.docker.compose.project"] != "cove-"+workload {
				return errors.New("idle network contains an unexpected endpoint")
			}
			if err = e.Run(ctx, e.Root, []string{"docker", "network", "disconnect", network.Name, id}, nil, io.Discard); err != nil {
				return err
			}
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "network", "rm", network.Name}, nil, io.Discard); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) settleIdlePolicy(ctx context.Context, r Request, change *idleChange) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Minute)
	defer cancel()
	wanted := change.Previous
	if change.Committed {
		wanted = change.Next
	}
	if !change.Committed && change.PriorSuspension != nil {
		if err := writeJSON(filepath.Join(e.Root, "workloads", r.Workload, "suspension.json"), change.PriorSuspension); err != nil {
			return errors.Join(errSuspensionUnresolved, err)
		}
	}
	e.idleMu.Lock()
	err := e.writeIdle(r.Workload, wanted)
	e.idleMu.Unlock()
	if err != nil {
		return errors.Join(errSuspensionUnresolved, err)
	}
	if err := e.confirmSuspensionIngress(cleanup, r.Workload, io.Discard); err != nil {
		return errors.Join(errSuspensionUnresolved, err)
	}
	if wanted == nil {
		if err := e.removeIdleGateway(cleanup, r.Workload); err != nil {
			return errors.Join(errSuspensionUnresolved, err)
		}
	}
	change.Complete = true
	return e.saveIdleChange(r.ID, change)
}

func (e *Engine) performIdlePolicy(ctx context.Context, r Request, out io.Writer) (result error) {
	e.mu.Lock()
	j, err := e.read(r.ID)
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if j.IdleChange != nil {
		if !j.IdleChange.Complete {
			if err := e.settleIdlePolicy(ctx, r, j.IdleChange); err != nil {
				return err
			}
		}
		if !j.IdleChange.Committed {
			return errors.New("interrupted idle-policy change was rolled back")
		}
		return nil
	}
	previous, err := e.readIdle(r.Workload)
	if err != nil {
		return err
	}
	change := &idleChange{Previous: previous}
	if r.Idle.Enabled {
		e.linkMu.Lock()
		defer e.linkMu.Unlock()
		if suspension, err := e.readSuspension(r.Workload); err != nil {
			return err
		} else if suspension != nil && suspension.Phase != "awake" {
			return errors.New("start the resource before enabling idle sleep")
		}
		if err = e.idleEligibility(ctx, r.Workload); err != nil {
			return err
		}
		change.Next = &idleWorkloadState{Policy: r.Idle.normalized(), Config: idleGatewayConfig{Mode: "awake"}}
		if previous != nil {
			copy := *previous
			change.Next = &copy
			change.Next.Policy = r.Idle.normalized()
		}
	}
	if !r.Idle.Enabled {
		prior, err := e.readSuspension(r.Workload)
		if err != nil {
			return err
		}
		if prior != nil && prior.BeforeIdle != nil {
			change.PriorSuspension = prior
		}
	}
	if err = e.saveIdleChange(r.ID, change); err != nil {
		return err
	}
	defer func() {
		if !change.Complete && !errors.Is(result, errSuspensionUnresolved) {
			if err := e.settleIdlePolicy(ctx, r, change); err != nil {
				result = errors.Join(result, err)
			}
		}
	}()
	state, released, err := readCurrent(filepath.Join(e.Root, "workloads", r.Workload, "current.json"))
	if err != nil {
		return err
	}
	if change.Next != nil {
		if !released {
			return errors.New("idle sleep needs a deployed release")
		}
		e.ingressMu.Lock()
		if err = e.ensureIngress(ctx, io.Discard); err != nil {
			e.ingressMu.Unlock()
			return err
		}
		e.idleMu.Lock()
		err = e.prepareIdleGateway(ctx, r.Workload, change.Next, state.Routes)
		if err == nil {
			err = e.writeIdle(r.Workload, change.Next)
		}
		e.idleMu.Unlock()
		e.ingressMu.Unlock()
		if err != nil {
			return err
		}
	} else {
		e.idleMu.Lock()
		err = e.writeIdle(r.Workload, nil)
		e.idleMu.Unlock()
		if err != nil {
			return err
		}
		suspension, err := e.readSuspension(r.Workload)
		if err != nil {
			return err
		}
		if suspension != nil && suspension.Options.Reason == "idle" && suspension.ManualStop {
			suspension.Options.Reason = "manual_stop"
			if err = writeJSON(filepath.Join(e.Root, "workloads", r.Workload, "suspension.json"), suspension); err != nil {
				return err
			}
		}
		if suspension != nil && suspension.BeforeIdle != nil {
			suspension.BeforeIdle = nil
			if err = writeJSON(filepath.Join(e.Root, "workloads", r.Workload, "suspension.json"), suspension); err != nil {
				return err
			}
		}
		if suspension != nil && suspension.Options.Reason == "idle" && !suspension.ManualStop {
			wake := r
			wake.Action = "resume"
			wake.Suspension = &SuspensionOptions{Reason: "idle", Scope: r.Workload, Start: true}
			if err = e.performSuspension(ctx, wake, false, io.Discard); err != nil {
				return err
			}
			if previous != nil {
				if code, readErr := e.idleControl(ctx, r.Workload, "GET", "/status", nil, nil); readErr == nil && code == 200 {
					prior := *previous
					prior.Config.Mode, prior.Config.Episode = "awake", ""
					e.idleMu.Lock()
					refreshErr := e.prepareIdleGateway(ctx, r.Workload, &prior, state.Routes)
					e.idleMu.Unlock()
					if refreshErr != nil {
						fmt.Fprintln(out, "Direct routing is active, but the previous request gateway could not release waiting requests:", refreshErr)
					}
				}
			}

		}
	}
	if released {
		if err = e.applyIngress(ctx, r.Workload, state.Routes, io.Discard); err != nil {
			return err
		}
	}
	e.mu.Lock()
	j, err = e.read(r.ID)
	if err == nil && j.CancelRequested {
		err = context.Canceled
	}
	if err == nil {
		change.Committed = true
		j.IdleChange = change
		err = writeJSON(e.path(r.ID), j)
	}
	e.mu.Unlock()
	if err != nil {
		return errors.Join(errSuspensionUnresolved, err)
	}
	return nil
}

func (e *Engine) IdleStatus(ctx context.Context, workload string) (*IdleStatus, error) {
	state, err := e.readIdle(workload)
	if err != nil || state == nil {
		return nil, err
	}
	status := &IdleStatus{Enabled: true, AfterSeconds: state.Policy.normalized().AfterSeconds, Phase: "unknown", LastSleep: state.LastSleep, LastWake: state.LastWake, Error: state.LastError}
	suspension, err := e.readSuspension(workload)
	if err != nil {
		return nil, err
	}
	if suspension != nil && suspension.Phase != "awake" {
		if suspension.Options.Reason != "idle" || suspension.ManualStop {
			status.Phase = "paused"
			return status, nil
		}
		status.Phase = "sleeping"
		if suspension.Phase == "pausing" {
			status.Phase = "sleeping_pending"
		}
		if suspension.Phase == "resuming" {
			status.Phase = "waking"
		}
		wake, readErr := e.Status("wake-" + suspension.idleEpisode())
		if readErr == nil {
			if wake.State == "queued" {
				status.Phase = "wake_queued"
			}
			if wake.State == "running" {
				status.Phase = "waking"
			}
			if wake.State == "failed" || wake.State == "cancelled" || wake.State == "interrupted" {
				status.Phase = "wake_failed"
				status.Error = "Automatic wake failed. Repair the app, then use Start or disable idle sleep to retry explicitly."
			}
		} else if !os.IsNotExist(readErr) {
			return nil, readErr
		}

	}
	if suspension != nil && suspension.Phase == "awake" {
		status.Phase = "waking"
	}
	var gateway idleGatewayState
	code, err := e.idleControl(ctx, workload, "GET", "/status", nil, &gateway)
	if err != nil || code != 200 {
		status.Error = "The local request gateway is unavailable."
		return status, nil
	}
	status.ActiveRequests = gateway.Active
	status.LastActivity = &gateway.LastActivity
	if suspension == nil {
		if gateway.Mode == "asleep" {
			status.Phase = "unknown"
			status.Error = "The request gateway is asleep without a confirmed container snapshot."
		} else {
			status.Phase = gateway.Mode
		}
	}
	return status, nil
}

func (e *Engine) idleOperation(ctx context.Context, r Request, out io.Writer) (result error) {
	defer func() {
		if result != nil && !errors.Is(result, errSuspensionUnresolved) {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			current, released, err := readCurrent(filepath.Join(e.Root, "workloads", r.Workload, "current.json"))
			if err == nil && released {
				_, err = e.refreshIdleGateway(cleanup, r.Workload, current.Routes)
			}
			if err != nil {
				result = errors.Join(result, errSuspensionUnresolved, err)
			}
		}
	}()
	state, err := e.readIdle(r.Workload)
	if err != nil {
		return err
	}
	if state == nil {
		return errors.New("idle policy is disabled")
	}
	if r.Action == "idle_sleep" {
		e.mu.Lock()
		j, err := e.read(r.ID)
		e.mu.Unlock()
		if err != nil {
			return err
		}
		if j.SuspensionChange == nil {
			if err = e.idleEligibility(ctx, r.Workload); err != nil {
				return err
			}
			code, err := e.idleControl(ctx, r.Workload, "POST", "/quiesce", map[string]any{"seconds": state.Policy.normalized().AfterSeconds, "episode": r.ID}, nil)
			if err != nil {
				return err
			}
			if code != 204 {
				var gateway idleGatewayState
				if status, err := e.idleControl(ctx, r.Workload, "GET", "/status", nil, &gateway); err != nil || status != 200 || gateway.Mode != "quiescing" || gateway.Episode != r.ID {
					return errors.New("resource became active before idle sleep")
				}
			}
		}
		r.Action = "suspend"
	} else {
		r.Action = "resume"
	}
	r.Suspension = &SuspensionOptions{Reason: "idle", Scope: r.Workload, Start: r.Action == "resume"}
	err = e.performSuspension(ctx, r, false, out)
	if err == nil {
		e.idleMu.Lock()
		state, readErr := e.readIdle(r.Workload)
		if readErr == nil && state != nil {
			now := time.Now().UTC()
			if r.Action == "suspend" {
				state.LastSleep = &now
			} else {
				state.LastWake = &now
			}
			state.LastError = ""
			readErr = e.writeIdle(r.Workload, state)
		}
		e.idleMu.Unlock()
		if readErr != nil {
			return errors.Join(errSuspensionUnresolved, readErr)
		}
	}
	return err
}
