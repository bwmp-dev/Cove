package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
)

func (e *Engine) restartUnhealthy(ctx context.Context, workload string, out io.Writer) error {
	if _, pending, err := e.pendingRollout(workload); err != nil {
		return err
	} else if pending != nil && pending.Phase == "waiting" {
		return errors.New("resolve the staged rollout before automatically restarting containers")
	}
	containers, err := e.HealthContainers(ctx, workload)
	if err != nil {
		return err
	}
	ids := []string{}
	for _, container := range containers {
		if container.State != "running" || container.Health != "unhealthy" {
			continue
		}
		if !containerIdentity.MatchString(container.ID) {
			return errors.New("invalid unhealthy container identity")
		}
		ids = append(ids, container.ID)
		if len(ids) > 64 {
			return errors.New("at most 64 unhealthy running containers can be restarted in one operation")
		}
	}
	if len(ids) == 0 {
		fmt.Fprintln(out, "No running containers currently report unhealthy Docker health.")
		return nil
	}
	if err := e.Run(ctx, e.Root, append([]string{"docker", "restart", "--timeout", "10"}, ids...), nil, out); err != nil {
		return err
	}
	return e.refreshTrafficIngress(ctx, workload, out)
}
