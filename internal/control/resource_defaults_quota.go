package control

import (
	"context"
	"errors"
	"math"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5/pgconn"
)

func (s *Server) reserveInheritedLimits(ctx context.Context, node string, request worker.Request) error {
	if request.Action != "deploy" || request.Scale != nil || request.DefaultLimits == nil {
		return nil
	}
	var bounded bool
	var encrypted []byte
	if err := s.DB.QueryRow(ctx, `SELECT w.configuration,EXISTS(SELECT 1 FROM team_quotas q WHERE q.team_id=w.team_id AND (q.cpus>0 OR q.memory_mb>0)) FROM workloads w WHERE w.id=$1`, request.Workload).Scan(&encrypted, &bounded); err != nil {
		return errors.New("inherited resource quota unavailable; retry after the database recovers")
	}
	if !bounded {
		return nil
	}
	var config workloadInput
	if err := s.unsealJSON(encrypted, &config); err != nil {
		return errors.New("inherited resource quota configuration unavailable")
	}
	if config.Autoscale != nil && config.Autoscale.Enabled && request.Rollout != nil {
		options := *request.Rollout
		options.Replicas = map[string]int{}
		for service, count := range request.Rollout.Replicas {
			options.Replicas[service] = count
		}
		for service, policy := range config.Autoscale.Normalized().Services {
			options.Replicas[service] = max(options.Replicas[service], policy.MaxReplicas)
		}
		request.Rollout = &options
	}
	reply, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "capacity", Request: &request})
	if err != nil || reply.Error != "" || reply.Preflight == nil || reply.Preflight.Usage == nil {
		return errors.New("could not confirm inherited CPU and memory quota; upgrade and verify the worker, then retry the deployment")
	}
	usage := reply.Preflight.Usage
	if math.IsNaN(usage.CPUs) || math.IsInf(usage.CPUs, 0) || usage.CPUs < 0 || usage.MemoryMB < 0 {
		return errors.New("worker returned invalid inherited quota totals")
	}
	cpu, memory := configuredUsage(config)
	_, err = s.DB.Exec(ctx, `UPDATE workloads SET configured_cpu=$2,configured_memory_mb=$3 WHERE id=$1`, request.Workload, max(cpu, usage.CPUs), max(memory, usage.MemoryMB))
	if err != nil {
		var database *pgconn.PgError
		if errors.As(err, &database) && database.ConstraintName == "cove_team_quota" {
			return errors.New("inherited CPU or memory limits exceed the team quota; reduce the defaults or ask an instance administrator to adjust the budget")
		}
		return errors.New("could not reserve inherited resource quota; retry after the database recovers")
	}
	return nil
}
