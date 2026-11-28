package control

import (
	"context"
	"errors"

	"cove/internal/worker"
)

func (s *Server) resourceIntentionallyDown(ctx context.Context, id string) (bool, error) {
	var stopped, idle bool
	var node string
	err := s.DB.QueryRow(ctx, `SELECT manually_stopped OR EXISTS(SELECT 1 FROM environments e WHERE e.id=w.environment_id AND e.paused) OR EXISTS(SELECT 1 FROM environment_suspensions c WHERE c.workload_id=w.id) OR EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state='running' AND o.action IN ('move','pitr_configure')),COALESCE((idle_policy->>'enabled')::boolean,false),node_id FROM workloads w WHERE id=$1`, id).Scan(&stopped, &idle, &node)
	if err != nil || stopped || !idle {
		return stopped, err
	}
	response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "container_health", ID: id})
	if err != nil || response.Error != "" {
		return false, errors.New("could not determine whether the resource is intentionally asleep")
	}
	return response.Suspension != nil && response.Suspension.Phase != "awake", nil
}
