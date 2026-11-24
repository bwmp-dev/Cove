package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

func (e *Engine) RetireBuild(ctx context.Context, request Request) error {
	if request.Action != "build" || ValidateRequest(request) != nil {
		return errors.New("invalid build retirement")
	}
	e.mu.Lock()
	j, err := e.read(request.ID)
	if err == nil {
		if j.Request.Workload != request.Workload || j.Request.Action != "build" {
			e.mu.Unlock()
			return errors.New("build ownership mismatch")
		}
		request = j.Request
	} else if !errors.Is(err, os.ErrNotExist) {
		e.mu.Unlock()
		return err
	}
	e.mu.Unlock()
	// Retain a terminal digest tombstone, including for an unacknowledged submission.
	// Deleting it would let a late delivery execute after its resource was removed.
	if err == nil {
		if j.Operation.State == "queued" || j.Operation.State == "running" {
			if _, err = e.Cancel(request); err != nil {
				return err
			}
		}
	} else if _, err = e.Cancel(request); err != nil {
		return err
	}
	e.mu.Lock()
	if e.busy(request.Workload) {
		e.mu.Unlock()
		return errors.New("build cleanup waits for the executor")
	}
	for _, pending := range e.pending {
		if pending.Workload == request.Workload && pending.ID != request.ID {
			e.mu.Unlock()
			return errors.New("build cleanup waits for pending work")
		}
	}
	j, err = e.read(request.ID)
	if err != nil || j.Operation.State == "queued" || j.Operation.State == "running" {
		e.mu.Unlock()
		return errors.New("build execution has not settled")
	}
	e.active[request.Workload] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, request.Workload); e.mu.Unlock() }()
	e.buildMu.Lock()
	err = e.recoverBuild(ctx, request.Workload)
	e.buildMu.Unlock()
	if err != nil {
		return err
	}
	if err = e.cleanupBuildInputs(request); err != nil {
		return err
	}
	if err = os.Remove(filepath.Join(e.Root, "operations", request.ID+".log")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	j.Request = Request{ID: request.ID, Workload: request.Workload, Action: "build", Kind: request.Kind, JobService: request.JobService}
	return writeJSON(e.path(request.ID), j)
}
