package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
)

type buildpacksMemoryError struct{ limit uint64 }

func (failure *buildpacksMemoryError) Error() string {
	return fmt.Sprintf("CNB exceeded its %d-byte phase memory limit, derived at admission from available memory minus 2 GiB and clamped to 2 to 4 GiB. Reduce build memory use or free node memory before retrying.", failure.limit)
}

var errBuildpacksPressure = errors.New("CNB stopped to preserve node memory, disk space or its monitored work target; free resources before retrying")

func (e *Engine) submitBuildpacks(ctx context.Context, request Request) (Operation, error) {
	if err := ValidateRequest(request); err != nil {
		return Operation{}, err
	}
	e.mu.Lock()
	_, err := e.read(request.ID)
	if err == nil {
		operation, err := e.submitBuildpacksLocked(request, nil)
		e.mu.Unlock()
		return operation, err
	}
	e.mu.Unlock()
	if !errors.Is(err, os.ErrNotExist) {
		return Operation{}, err
	}
	capacity, err := e.buildpacksCapacity(ctx, request.BuildPlatform)
	if err != nil {
		return Operation{}, err
	}
	if err := ValidateBuildpacksCapacity(capacity); err != nil {
		return Operation{}, err
	}
	admission, err := DeriveBuildpacksAdmission(capacity.MemoryAvailable)
	if err != nil {
		return Operation{}, err
	}
	if request.BuildpacksAdmission != nil {
		if err := request.BuildpacksAdmission.Validate(); err != nil {
			return Operation{}, err
		}
		if request.BuildpacksAdmission.PhaseMemoryBytes < admission.PhaseMemoryBytes {
			admission = *request.BuildpacksAdmission
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.submitBuildpacksLocked(request, &admission)
}

func (e *Engine) recordedBuildpacksAdmission(r Request) (BuildpacksAdmission, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	journal, err := e.read(r.ID)
	if err != nil || journal.Operation.BuildRecipe == nil {
		return BuildpacksAdmission{}, errors.New("CNB's recorded memory admission is unavailable")
	}
	admission := BuildpacksAdmission{AvailableMemoryBytes: journal.Operation.BuildRecipe.AdmissionAvailableMemoryBytes, PhaseMemoryBytes: journal.Operation.BuildRecipe.PhaseMemoryBytes}
	return admission, admission.Validate()
}

func (e *Engine) admitBuildpacksPhases(r Request, available uint64) error {
	current, err := DeriveBuildpacksAdmission(available)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	journal, err := e.read(r.ID)
	if err != nil || journal.Operation.BuildRecipe == nil {
		return errors.New("CNB's recorded memory admission is unavailable")
	}
	recorded := BuildpacksAdmission{AvailableMemoryBytes: journal.Operation.BuildRecipe.AdmissionAvailableMemoryBytes, PhaseMemoryBytes: journal.Operation.BuildRecipe.PhaseMemoryBytes}
	if err := recorded.Validate(); err != nil {
		return err
	}
	// Setup may consume memory after the controller reserves its upper budget.
	// Lower the actual phase limit without changing the immutable request or reservation.
	if current.PhaseMemoryBytes < recorded.PhaseMemoryBytes {
		journal.Operation.BuildRecipe.PhaseMemoryBytes = current.PhaseMemoryBytes
		journal.Operation.BuildRecipe.AdmissionAvailableMemoryBytes = current.AvailableMemoryBytes
		return writeJSON(e.path(r.ID), journal)
	}
	return nil
}
