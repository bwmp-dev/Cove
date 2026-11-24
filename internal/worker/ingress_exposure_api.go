package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type IngressExposureRequest struct {
	Settings           IngressExposureSettings `json:"settings"`
	Revision           string                  `json:"revision,omitempty"`
	Digest             string                  `json:"digest,omitempty"`
	AcceptInterruption bool                    `json:"accept_interruption,omitempty"`
}

type IngressExposureChange struct {
	ID         string     `json:"id"`
	Phase      string     `json:"phase"`
	Committed  bool       `json:"committed"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type IngressExposureStatus struct {
	ProxyPresent   bool                    `json:"proxy_present"`
	Running        bool                    `json:"running"`
	PublicBindings *IngressPublicBindings  `json:"public_bindings,omitempty"`
	Settings       IngressExposureSettings `json:"settings"`
	Ports          map[string]int          `json:"ports"`
	Change         *IngressExposureChange  `json:"change,omitempty"`
}

func exposureStatus(state ingressExposureState) IngressExposureStatus {
	status := IngressExposureStatus{Settings: state.Settings, Ports: state.Ports}
	if c := state.Change; c != nil {
		status.Change = &IngressExposureChange{ID: c.ID, Phase: c.Phase, Committed: c.Committed, StartedAt: c.StartedAt, FinishedAt: c.FinishedAt}
	}
	return status
}

func (e *Engine) ingressExposureRuntimeStatus(ctx context.Context, client *http.Client, state ingressExposureState) (IngressExposureStatus, error) {
	status := exposureStatus(state)
	snapshot, err := captureIngressProxy(ctx, client)
	if errors.Is(err, errIngressDockerMissing) {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	bindings, err := proxyPublicBindings(snapshot)
	if err != nil {
		return status, err
	}
	status.ProxyPresent, status.Running, status.PublicBindings = true, snapshot.Running, &bindings
	return status, nil
}

func (e *Engine) IngressExposures(ctx context.Context) (IngressExposureStatus, error) {
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	state, err := e.loadIngressExposures()
	if err != nil {
		return exposureStatus(state), err
	}
	client := e.ingressDockerClient()
	defer client.CloseIdleConnections()
	return e.ingressExposureRuntimeStatus(ctx, client, state)
}

func (e *Engine) InitializeIngressExposures(ctx context.Context) (IngressExposureStatus, error) {
	unlock, err := e.lockNodeChange(false)
	if err != nil {
		return IngressExposureStatus{}, err
	}
	defer unlock()
	if err := e.requireInterlockedHostSchedule(); err != nil {
		return IngressExposureStatus{}, err
	}
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	e.idleMu.Lock()
	defer e.idleMu.Unlock()
	state, err := e.loadIngressExposures()
	if err != nil {
		return exposureStatus(state), err
	}
	if err := e.ingressExposureReady(); err != nil {
		return exposureStatus(state), err
	}
	client := e.ingressDockerClient()
	defer client.CloseIdleConnections()
	_, err = captureIngressProxy(ctx, client)
	if err == nil {
		return e.ingressExposureRuntimeStatus(ctx, client, state)
	}
	if !errors.Is(err, errIngressDockerMissing) {
		return exposureStatus(state), err
	}
	if restored, err := restoreSavedIngressProxy(ctx, client, state); err != nil {
		return exposureStatus(state), err
	} else if restored {
		return e.ingressExposureRuntimeStatus(ctx, client, state)
	}
	ctx = context.WithValue(ctx, nodeLeaseContextKey{}, e)
	if err := e.ensureIngress(ctx, io.Discard); err != nil {
		return exposureStatus(state), errors.New("the initial managed proxy could not be created; check node resources and Docker")
	}
	return e.ingressExposureRuntimeStatus(ctx, client, state)
}

func restoreSavedIngressProxy(ctx context.Context, client *http.Client, state ingressExposureState) (bool, error) {
	var saved *ingressProxySnapshot
	if state.Change != nil {
		if state.Change.Committed {
			saved = state.Change.Installed
		} else {
			saved = state.Change.Previous
		}
	}
	if saved != nil {
		body, err := ingressProxyCreateBody(*saved, state.Change.ID)
		if err != nil {
			return false, err
		}
		var created struct{ ID string }
		if err := ingressDockerJSON(ctx, client, "POST", "/containers/create?name=cove-ingress", body, &created); err != nil {
			return false, err
		}
		return true, nil
	}
	if len(state.Ports) > 0 || state.Settings.HTTP3 || state.Settings.Public != nil {
		return false, errors.New("the proxy is missing and has no installed recovery snapshot; restore its saved container before initializing")
	}
	return false, nil
}

func (e *Engine) PlanIngressExposures(ctx context.Context, desired IngressExposureSettings) (IngressExposurePlan, error) {
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	state, err := e.loadIngressExposures()
	if err != nil {
		return IngressExposurePlan{}, err
	}
	client := e.ingressDockerClient()
	defer client.CloseIdleConnections()
	prepared, err := e.prepareIngressExposure(ctx, client, desired, state)
	return prepared.Plan, err
}

func (e *Engine) ChangeIngressExposures(ctx context.Context, request IngressExposureRequest) (IngressExposureStatus, error) {
	unlock, err := e.lockNodeChange(false)
	if err != nil {
		return IngressExposureStatus{}, err
	}
	defer unlock()
	if err := e.requireInterlockedHostSchedule(); err != nil {
		return IngressExposureStatus{}, err
	}
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	e.idleMu.Lock()
	defer e.idleMu.Unlock()
	state, err := e.loadIngressExposures()
	if err != nil {
		return exposureStatus(state), err
	}
	if state.Change != nil && state.Change.ID == request.Digest[:min(len(request.Digest), 32)] {
		if request.Digest == "" || request.Digest != state.Change.Digest || canonicalIngressExposures(request.Settings) != canonicalIngressExposures(state.Change.Desired) {
			return exposureStatus(state), errors.New("saved listener change input does not match")
		}
		return exposureStatus(state), nil
	}
	if !request.AcceptInterruption {
		return exposureStatus(state), errors.New("confirm the shared proxy interruption before applying listener changes")
	}
	client := e.ingressDockerClient()
	defer client.CloseIdleConnections()
	prepared, err := e.prepareIngressExposure(ctx, client, request.Settings, state)
	if err != nil {
		return exposureStatus(state), err
	}
	if request.Revision != prepared.Plan.Revision || request.Digest != prepared.Plan.Digest {
		return exposureStatus(state), errors.New("the listener plan changed; review a fresh plan before applying")
	}
	e.mu.Lock()
	if len(e.active) > 0 || len(e.recovering) > 0 || e.draining || e.proxyUnsettled || e.hostChanging || e.customRunning || e.customUnsettled {
		e.mu.Unlock()
		return exposureStatus(state), errors.New("worker operations or proxy recovery must finish before changing listeners")
	}
	e.proxyUnsettled, e.proxyRunning = true, true
	e.mu.Unlock()
	defer func() {
		durable, readErr := e.loadIngressExposures()
		e.mu.Lock()
		e.proxyRunning = false
		e.proxyUnsettled = readErr != nil || durable.Change != nil && durable.Change.FinishedAt == nil
		e.mu.Unlock()
	}()
	err = e.applyIngressExposure(ctx, client, prepared, &state)
	return exposureStatus(state), err
}

func (e *Engine) ingressExposureReady() error {
	state, err := e.loadIngressExposures()
	if err != nil {
		return err
	}
	if state.Change != nil && state.Change.FinishedAt == nil {
		return errors.New("a shared proxy listener change is recovering; ingress mutations are held")
	}
	return nil
}

func (e *Engine) runProxyRecovery(ctx context.Context) {
	recovery, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	e.ingressMu.Lock()
	e.idleMu.Lock()
	state, err := e.loadIngressExposures()
	if err == nil && state.Change != nil && state.Change.FinishedAt == nil {
		var unlock func()
		unlock, err = e.lockNodeChange(true)
		if err == nil {
			defer unlock()
			err = e.requireInterlockedHostSchedule()
		}
		if err == nil {
			client := e.ingressDockerClient()
			err = e.recoverIngressExposure(recovery, client, &state)
			client.CloseIdleConnections()
		}
	}
	e.idleMu.Unlock()
	e.ingressMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.proxyRunning = false
	e.proxyUnsettled = err != nil
	if err != nil {
		fmt.Fprintln(os.Stderr, "Shared proxy listener recovery failed; worker remains available for repairs:", err)
		e.proxyRetry = time.Now().Add(routeRecoveryRetry)
	}
}
