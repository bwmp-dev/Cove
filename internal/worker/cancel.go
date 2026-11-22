package worker

import "time"

func (e *Engine) Cancel(request Request) (Operation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	op, err := e.submitLocked(request)
	if err != nil {
		return Operation{}, err
	}
	if op.State != "queued" && op.State != "running" {
		return op, nil
	}
	j, err := e.read(request.ID)
	if err != nil {
		return Operation{}, err
	}
	j.CancelRequested = true
	if op.State == "queued" {
		now := time.Now().UTC()
		j.Operation.State = "cancelled"
		j.Operation.FinishedAt = &now
		j.Operation.Error = "Cancelled before execution."
	}
	if err = writeJSON(e.path(request.ID), j); err != nil {
		return Operation{}, err
	}
	if cancel := e.cancels[request.ID]; cancel != nil {
		cancel()
	}
	return j.Operation, nil
}
