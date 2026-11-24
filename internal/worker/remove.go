package worker

import (
	"context"
	"errors"
	"io"
	"strings"
)

func (e *Engine) RemoveWorkload(ctx context.Context, id string) error {
	if !workloadIdentifier.MatchString(id) {
		return errors.New("invalid workload")
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
		return errors.New("workload has an active operation")
	}
	e.active[id] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, id); e.mu.Unlock() }()
	containers, err := e.Containers(ctx, id)
	if err != nil {
		return err
	}
	for _, container := range containers {
		if container.State != "exited" && container.State != "created" && container.State != "dead" {
			return errors.New("stop the workload before removing it")
		}
	}
	if err = e.applyIngress(ctx, id, nil, io.Discard); err != nil {
		return err
	}
	idle, err := e.readIdle(id)
	if err != nil {
		return err
	}
	if idle != nil {
		if err = e.removeIdleGateway(ctx, id); err != nil {
			return err
		}
		if err = e.writeIdle(id, nil); err != nil {
			return err
		}
	}
	if err = e.removeWorkloadLinks(ctx, id); err != nil {
		return err
	}
	for _, container := range containers {
		if err = e.Run(ctx, e.Root, []string{"docker", "rm", container.ID}, nil, io.Discard); err != nil {
			return err
		}
	}
	networks, err := e.output(ctx, "docker", "network", "ls", "-q", "--filter", "label=com.docker.compose.project=cove-"+id)
	if err != nil {
		return err
	}
	for _, network := range strings.Fields(networks) {
		if err = e.Run(ctx, e.Root, []string{"docker", "network", "rm", network}, nil, io.Discard); err != nil {
			return err
		}
	}
	return nil
}
