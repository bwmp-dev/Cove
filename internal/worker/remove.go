package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func (e *Engine) RemoveWorkload(ctx context.Context, id string) (*WorkloadRemovalResult, error) {
	if !workloadIdentifier.MatchString(id) {
		return nil, errors.New("invalid workload")
	}
	e.mu.Lock()
	busy := e.busy(id)
	for operation, workload := range e.automaticScans {
		if workload == id {
			if cancel := e.cancels[operation]; cancel != nil {
				cancel()
			}
			busy = true
		}
	}
	for _, op := range e.pending {
		if op.Workload == id {
			busy = true
		}
	}
	if busy {
		e.mu.Unlock()
		return nil, errors.New("workload has an active operation")
	}
	e.active[id] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, id); e.mu.Unlock() }()
	if _, err := os.Lstat(filepath.Join(e.pitrDirectory(id), "state.json")); err == nil {
		return nil, errors.New("disable and delete PITR history before removing this resource")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(e.Root, "workloads", id, "filesystem-snapshot.json")); err == nil {
		return nil, errors.New("recover the pending filesystem snapshot before removing this workload")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	for _, marker := range []string{"storage-recovery.json", "database-snapshot.json", "deploy-guard.json", "pending-candidate.json", "pending-build.json", "pending-hook.json", "hook.json"} {
		if _, err := os.Lstat(filepath.Join(e.Root, "workloads", id, marker)); err == nil {
			return nil, errors.New("resolve pending resource recovery before removing this workload")
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	containers, err := e.Containers(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, container := range containers {
		if container.State != "exited" && container.State != "created" && container.State != "dead" {
			return nil, errors.New("stop the workload before removing it")
		}
	}
	state, err := e.planWorkloadRemoval(ctx, id, containers)
	if err != nil {
		return nil, err
	}
	defer state.root.Close()
	images, err := e.removalImages(ctx, id, state)
	if err != nil {
		return nil, err
	}
	if len(images) > 0 {
		e.mu.Lock()
		if e.draining || e.hasMoveFence() || e.hostChanging || e.proxyRunning || e.customRunning || e.customUnsettled || len(e.active) > 1 || len(e.pending) > 0 || len(e.recovering) > 0 {
			e.mu.Unlock()
			return nil, errors.New("wait for worker operations and recovery before removing generated build images")
		}
		e.active[housekeepingWorkload] = true
		e.mu.Unlock()
		defer func() { e.mu.Lock(); delete(e.active, housekeepingWorkload); e.mu.Unlock() }()
	}
	if err = e.applyIngress(ctx, id, nil, io.Discard); err != nil {
		return nil, err
	}
	idle, err := e.readIdle(id)
	if err != nil {
		return nil, err
	}
	if idle != nil {
		if err = e.removeIdleGateway(ctx, id); err != nil {
			return nil, err
		}
		if err = e.writeIdle(id, nil); err != nil {
			return nil, err
		}
	}
	if err = e.removeWorkloadLinks(ctx, id); err != nil {
		return nil, err
	}
	for _, container := range containers {
		if err = e.Run(ctx, e.Root, []string{"docker", "rm", container.ID}, nil, io.Discard); err != nil {
			return nil, err
		}
	}
	networks, err := e.output(ctx, "docker", "network", "ls", "-q", "--filter", "label=com.docker.compose.project="+e.composeProject(id))
	if err != nil {
		return nil, err
	}
	for _, network := range strings.Fields(networks) {
		if err = e.Run(ctx, e.Root, []string{"docker", "network", "rm", network}, nil, io.Discard); err != nil {
			return nil, err
		}
	}
	if _, err := os.Lstat(filepath.Join(e.Root, "workloads", id, "pitr-image.json")); err == nil {
		owner, err := e.backupHelperOwner(false)
		if err != nil {
			return nil, err
		}
		name := "cove-pitr-image-" + id
		container, err := e.pitrContainerID(ctx, name)
		if err != nil {
			return nil, err
		}
		if container != "" {
			actual, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "dev.cove.pitr.owner"}}`, name)
			if err != nil {
				return nil, err
			}
			if actual != owner {
				return nil, errors.New("PITR copy image ownership changed")
			}
			if err = e.Run(ctx, e.Root, []string{"docker", "rm", "--volumes", name}, nil, io.Discard); err != nil {
				return nil, err
			}
		}
		if err = os.Remove(filepath.Join(e.Root, "workloads", id, "pitr-image.json")); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := e.removeGeneratedImageTags(ctx, images); err != nil {
		return nil, err
	}
	if err := state.apply(ctx); err != nil {
		return nil, err
	}
	return &state.result, nil
}
