package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

func readBuildpacksSupport(ctx context.Context, query buildNodeQuery, node string) (worker.BuildpacksSupport, error) {
	probe, ready, err := readBuildpacksProbe(ctx, query, node)
	if err != nil {
		return worker.BuildpacksSupport{}, err
	}
	return worker.BuildpacksDetails(probe, ready), nil
}

func readBuildpacksProbe(ctx context.Context, query buildNodeQuery, node string) (worker.Probe, bool, error) {
	var state string
	var data []byte
	if err := query.QueryRow(ctx, `SELECT state,COALESCE(capabilities,'{}') FROM nodes WHERE id=$1`, node).Scan(&state, &data); err != nil {
		return worker.Probe{}, false, err
	}
	var probe worker.Probe
	if json.Unmarshal(data, &probe) != nil {
		return worker.Probe{}, false, errors.New("Cloud Native Buildpacks capabilities are unavailable")
	}
	return probe, state == "ready", nil
}

func requireBuildpacks(ctx context.Context, query buildNodeQuery, node string) error {
	status, err := readBuildpacksSupport(ctx, query, node)
	if err != nil {
		return errors.New("Cloud Native Buildpacks capabilities are unavailable")
	}
	if !status.Supported {
		return errors.New(status.Reason)
	}
	return nil
}

func (s *Server) admitBuildpacks(ctx context.Context, id, node string) (bool, *worker.BuildpacksAdmission, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback(ctx)
	var team string
	if err := tx.QueryRow(ctx, `SELECT w.team_id FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE o.id=$1`, id).Scan(&team); err != nil {
		return false, nil, err
	}
	for _, lock := range []string{"cove-quota:" + team, "cove-buildpacks:" + node} {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
			return false, nil, err
		}
	}
	var admitted, cancelled, reserved bool
	if err := tx.QueryRow(ctx, `SELECT admitted_at IS NOT NULL,cancel_requested,EXISTS(SELECT 1 FROM buildpack_reservations b WHERE b.operation_id=o.id AND b.node_id=$2) FROM operations o WHERE id=$1 FOR UPDATE`, id, node).Scan(&admitted, &cancelled, &reserved); err != nil {
		return false, nil, err
	}
	if !reserved {
		return false, nil, errors.New("Cloud Native Buildpacks reservation does not match its immutable build node")
	}
	if cancelled {
		return false, nil, nil
	}
	if admitted {
		var admission worker.BuildpacksAdmission
		if err := tx.QueryRow(ctx, `SELECT available_memory_bytes,phase_memory_bytes FROM buildpack_reservations WHERE operation_id=$1`, id).Scan(&admission.AvailableMemoryBytes, &admission.PhaseMemoryBytes); err != nil {
			return false, nil, err
		}
		if err := admission.Validate(); err != nil {
			return false, nil, err
		}
		return true, &admission, nil
	}
	var busy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM buildpack_reservations b JOIN operations o ON o.id=b.operation_id WHERE b.node_id=$2 AND o.id<>$1 AND o.admitted_at IS NOT NULL AND o.state IN ('queued','running'))`, id, node).Scan(&busy); err != nil {
		return false, nil, err
	}
	if busy {
		ok, err := holdDeployment(ctx, tx, id, "Waiting for the build node's Cloud Native Buildpacks reservation.")
		return ok, nil, err
	}
	probe, ready, err := readBuildpacksProbe(ctx, tx, node)
	if err != nil {
		return false, nil, err
	}
	support := worker.BuildpacksDetails(probe, ready)
	if !support.Supported {
		ok, err := holdDeployment(ctx, tx, id, support.Reason)
		return ok, nil, err
	}
	admission, err := worker.DeriveBuildpacksAdmission(probe.BuildpacksCapacity.MemoryAvailable)
	if err != nil {
		return false, nil, err
	}
	reservedMemory := admission.PhaseMemoryBytes/(1<<20) + worker.BuildpacksHelperMemoryMB
	var cpuLimit, usedCPU float64
	var memoryLimit, usedMemory int64
	var slots, usedSlots int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(q.cpus,0),COALESCE(q.memory_mb,0),COALESCE(q.deployments,0),COALESCE((SELECT sum(configured_cpu) FROM workloads WHERE team_id=t.id),0)+COALESCE((SELECT sum(b.cpus) FROM buildpack_reservations b JOIN operations o ON o.id=b.operation_id JOIN workloads w ON w.id=o.workload_id WHERE w.team_id=t.id AND o.admitted_at IS NOT NULL AND o.state IN ('queued','running')),0),COALESCE((SELECT sum(configured_memory_mb) FROM workloads WHERE team_id=t.id),0)+COALESCE((SELECT sum(b.memory_mb) FROM buildpack_reservations b JOIN operations o ON o.id=b.operation_id JOIN workloads w ON w.id=o.workload_id WHERE w.team_id=t.id AND o.admitted_at IS NOT NULL AND o.state IN ('queued','running')),0),(SELECT count(*) FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE w.team_id=t.id AND o.admitted_at IS NOT NULL AND o.state IN ('queued','running') AND (o.action IN ('deploy','rollback','rollout_promote','pitr_configure','move','registry_maintenance') OR EXISTS(SELECT 1 FROM buildpack_reservations b WHERE b.operation_id=o.id))) FROM teams t LEFT JOIN team_quotas q ON q.team_id=t.id WHERE t.id=$1`, team).Scan(&cpuLimit, &memoryLimit, &slots, &usedCPU, &usedMemory, &usedSlots); err != nil {
		return false, nil, err
	}
	if cpuLimit > 0 && usedCPU+worker.BuildpacksQuotaCPUs > cpuLimit+0.000001 || memoryLimit > 0 && usedMemory+int64(reservedMemory) > memoryLimit || slots > 0 && usedSlots >= slots {
		ok, err := holdDeployment(ctx, tx, id, "Waiting for a team budget slot for Cloud Native Buildpacks phase memory and helper resources.")
		return ok, nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE buildpack_reservations SET phase_memory_bytes=$2,available_memory_bytes=$3,memory_mb=$4 WHERE operation_id=$1`, id, admission.PhaseMemoryBytes, admission.AvailableMemoryBytes, reservedMemory); err != nil {
		return false, nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE operations SET admitted_at=now(),result=result-'error',updated_at=now() WHERE id=$1`, id); err != nil {
		return false, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, nil, err
	}
	return true, &admission, nil
}

func reserveBuildpacks(ctx context.Context, tx pgx.Tx, id, node string) error {
	_, err := tx.Exec(ctx, `INSERT INTO buildpack_reservations(operation_id,node_id,cpus,memory_mb) VALUES($1,$2,$3,$4)`, id, node, worker.BuildpacksQuotaCPUs, worker.BuildpacksMemoryBytes/(1<<20)+worker.BuildpacksHelperMemoryMB)
	return err
}
