package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"math"
	"net/http"
)

type teamQuota struct {
	Resources   int     `json:"resources"`
	CPUs        float64 `json:"cpus"`
	MemoryMB    int64   `json:"memory_mb"`
	Deployments int     `json:"deployments"`
}

func configuredUsage(config workloadInput) (float64, int64) {
	var cpus float64
	var memory int64
	var scaling map[string]worker.AutoscaleService
	if config.Autoscale != nil && config.Autoscale.Enabled {
		scaling = config.Autoscale.Normalized().Services
	}
	for service, limit := range config.Limits {
		count := 1
		if config.Rollout != nil && config.Rollout.Strategy != "disabled" && config.Rollout.Replicas[service] > 0 {
			count = config.Rollout.Replicas[service]
		}
		count = max(count, scaling[service].MaxReplicas)
		cpus += limit.CPUs * float64(count)
		memory += limit.MemoryMB * int64(count)
	}
	return cpus, memory
}

func quotaError(w http.ResponseWriter, err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.ConstraintName == "cove_team_quota" {
		http.Error(w, "team quota exceeded; ask an instance administrator to adjust resource or configured-limit budgets", 409)
		return true
	}
	return false
}

func (s *Server) initializeQuotaUsage(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT id,configuration FROM workloads WHERE configured_cpu IS NULL OR configured_memory_mb IS NULL`)
	if err != nil {
		return err
	}
	type usage struct {
		id     string
		cpu    float64
		memory int64
	}
	items := []usage{}
	for rows.Next() {
		var id string
		var encrypted []byte
		if err = rows.Scan(&id, &encrypted); err != nil {
			break
		}
		raw, e := unseal(s.Key, encrypted)
		if e != nil {
			err = e
			break
		}
		var config workloadInput
		if err = json.Unmarshal(raw, &config); err != nil {
			break
		}
		cpu, memory := configuredUsage(config)
		items = append(items, usage{id, cpu, memory})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range items {
		if _, err = s.DB.Exec(ctx, `UPDATE workloads SET configured_cpu=$2,configured_memory_mb=$3 WHERE id=$1`, item.id, item.cpu, item.memory); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) teamQuota(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("id")
	if team != actor(r).Team && !actor(r).Admin {
		http.NotFound(w, r)
		return
	}
	if r.Method == "GET" {
		var quota, usage teamQuota
		if err := s.DB.QueryRow(r.Context(), `SELECT COALESCE(q.resources,0),COALESCE(q.cpus,0),COALESCE(q.memory_mb,0),COALESCE(q.deployments,0) FROM teams t LEFT JOIN team_quotas q ON q.team_id=t.id WHERE t.id=$1`, team).Scan(&quota.Resources, &quota.CPUs, &quota.MemoryMB, &quota.Deployments); err != nil {
			http.Error(w, "quota unavailable", 503)
			return
		}
		if err := s.DB.QueryRow(r.Context(), `SELECT count(*),COALESCE(sum(configured_cpu),0),COALESCE(sum(configured_memory_mb),0) FROM workloads WHERE team_id=$1`, team).Scan(&usage.Resources, &usage.CPUs, &usage.MemoryMB); err != nil {
			http.Error(w, "usage unavailable", 503)
			return
		}
		if err := s.DB.QueryRow(r.Context(), `SELECT count(*) FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE w.team_id=$1 AND o.admitted_at IS NOT NULL AND o.state IN ('queued','running') AND o.action IN ('deploy','rollback')`, team).Scan(&usage.Deployments); err != nil {
			http.Error(w, "usage unavailable", 503)
			return
		}
		respond(w, 200, map[string]any{"limits": quota, "usage": usage})
		return
	}
	if !actor(r).Admin {
		http.Error(w, "instance administrator required", 403)
		return
	}
	var quota teamQuota
	if !decode(w, r, &quota) {
		return
	}
	if quota.Resources < 0 || quota.Resources > 100000 || math.IsNaN(quota.CPUs) || math.IsInf(quota.CPUs, 0) || quota.CPUs < 0 || quota.CPUs > 1000000 || quota.MemoryMB < 0 || quota.MemoryMB > 1<<40 || quota.Deployments < 0 || quota.Deployments > 64 {
		http.Error(w, "invalid quotas; zero means unlimited", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "quota unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended('cove-quota:'||$1,0))`, team); err != nil {
		http.Error(w, "quota unavailable", 503)
		return
	}
	var usage teamQuota
	if err = tx.QueryRow(r.Context(), `SELECT count(*),COALESCE(sum(configured_cpu),0),COALESCE(sum(configured_memory_mb),0) FROM workloads WHERE team_id=$1`, team).Scan(&usage.Resources, &usage.CPUs, &usage.MemoryMB); err != nil {
		http.Error(w, "usage unavailable", 503)
		return
	}
	if quota.Resources > 0 && usage.Resources > quota.Resources || quota.CPUs > 0 && usage.CPUs > quota.CPUs+0.000001 || quota.MemoryMB > 0 && usage.MemoryMB > quota.MemoryMB {
		http.Error(w, "new quotas are below current configured usage", 409)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO team_quotas(team_id,resources,cpus,memory_mb,deployments) VALUES($1,$2,$3,$4,$5) ON CONFLICT(team_id) DO UPDATE SET resources=$2,cpus=$3,memory_mb=$4,deployments=$5`, team, quota.Resources, quota.CPUs, quota.MemoryMB, quota.Deployments); err != nil {
		http.Error(w, "could not save quota", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save quota", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) admitDeployment(ctx context.Context, id string) (bool, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var team string
	var admitted, cancelled bool
	var action string
	if err = tx.QueryRow(ctx, `SELECT w.team_id,o.admitted_at IS NOT NULL,o.cancel_requested,o.action FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE o.id=$1`, id).Scan(&team, &admitted, &cancelled, &action); err != nil {
		return false, err
	}
	if admitted || cancelled || action != "deploy" && action != "rollback" && action != "rollout_promote" && action != "pitr_configure" && action != "move" {
		return true, nil
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cove-quota:'||$1,0))`, team); err != nil {
		return false, err
	}
	var maximum, used int
	if err = tx.QueryRow(ctx, `SELECT admitted_at IS NOT NULL,cancel_requested FROM operations WHERE id=$1 FOR UPDATE`, id).Scan(&admitted, &cancelled); err != nil {
		return false, err
	}
	if admitted || cancelled {
		return true, nil
	}
	reason, err := deploymentGate(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if reason != "" {
		return holdDeployment(ctx, tx, id, reason)
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT deployments FROM team_quotas WHERE team_id=$1),0),(SELECT count(*) FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE w.team_id=$1 AND o.admitted_at IS NOT NULL AND o.state IN ('queued','running') AND o.action IN ('deploy','rollback','rollout_promote','pitr_configure','move'))`, team).Scan(&maximum, &used); err != nil {
		return false, err
	}
	if maximum > 0 && used >= maximum {
		return holdDeployment(ctx, tx, id, "Waiting for a team deployment slot.")
	}
	if _, err = tx.Exec(ctx, `UPDATE operations SET admitted_at=now(),result=NULL WHERE id=$1 AND admitted_at IS NULL`, id); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
