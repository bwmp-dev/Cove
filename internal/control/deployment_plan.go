package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5"
)

func (s *Server) queueDeploymentPlan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Workloads []string `json:"workload_ids"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Workloads) < 2 || len(in.Workloads) > 32 {
		http.Error(w, "select 2-32 resources in deployment order", 400)
		return
	}
	seen := map[string]bool{}
	for _, id := range in.Workloads {
		if seen[id] {
			http.Error(w, "resources cannot repeat in a deployment plan", 400)
			return
		}
		seen[id] = true
		if !s.requireAccess(w, r, "workload", id, 2) {
			return
		}
		if !s.requirePersistentResource(w, r, id) {
			return
		}
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	// Lock in a stable order so overlapping plans cannot deadlock or interleave.
	ids := append([]string(nil), in.Workloads...)
	sort.Strings(ids)
	configs := map[string]workloadInput{}
	for _, id := range ids {
		var encrypted []byte
		if err = tx.QueryRow(r.Context(), "SELECT configuration FROM workloads WHERE id=$1 AND project_id=$2 AND team_id=$3 FOR UPDATE", id, r.PathValue("id"), actor(r).Team).Scan(&encrypted); err != nil {
			http.Error(w, "resource unavailable in this project", 400)
			return
		}
		var active bool
		if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))", id).Scan(&active); err != nil {
			http.Error(w, "operations unavailable", 503)
			return
		}
		if active {
			http.Error(w, "a selected resource has an active operation", 409)
			return
		}
		raw, e := unseal(s.Key, encrypted)
		var c workloadInput
		if e != nil || json.Unmarshal(raw, &c) != nil {
			http.Error(w, "resource configuration unavailable", 500)
			return
		}
		configs[id] = c
	}
	previous := ""
	operations := []string{}
	for _, id := range in.Workloads {
		c := configs[id]
		op := resourceID()
		request := worker.Request{ID: op, Workload: id, Action: "deploy", Compose: c.Compose, Environment: c.Environment, Source: c.Source, Kind: c.Kind, JobService: c.JobService, Routes: c.Routes, Private: c.Private, Limits: c.Limits, Hooks: c.Hooks, FileSecrets: c.FileSecrets, GuardedDeploy: c.GuardedDeploy, Rollout: c.Rollout}
		if err = loadNetworkRequest(r.Context(), tx, &request); err != nil {
			http.Error(w, "network configuration unavailable", 409)
			return
		}
		if err = worker.ValidateRequest(request); err != nil {
			http.Error(w, "invalid deployment configuration", 400)
			return
		}
		raw, e := json.Marshal(request)
		if e != nil {
			http.Error(w, "invalid deployment configuration", 500)
			return
		}
		encrypted, e := seal(s.Key, raw)
		if e != nil {
			http.Error(w, "could not protect deployment", 500)
			return
		}
		if _, err = tx.Exec(r.Context(), "INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'deploy',$3)", op, id, encrypted); err != nil {
			http.Error(w, "could not queue deployment plan", 409)
			return
		}
		if previous != "" {
			if _, err = tx.Exec(r.Context(), "INSERT INTO operation_dependencies(operation_id,prerequisite_id) VALUES($1,$2)", op, previous); err != nil {
				http.Error(w, "could not queue deployment dependency", 500)
				return
			}
		}
		previous = op
		if _, err = tx.Exec(r.Context(), "UPDATE workloads SET manually_stopped=false WHERE id=$1", id); err != nil {
			http.Error(w, "could not save deployment intent", 503)
			return
		}
		operations = append(operations, op)
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not queue deployment plan", 503)
		return
	}
	respond(w, 202, map[string]any{"operation_ids": operations})
}

func (s *Server) dependencyReady(ctx context.Context, id string) (bool, error) {
	var state string
	err := s.DB.QueryRow(ctx, "SELECT p.state FROM operation_dependencies d JOIN operations p ON p.id=d.prerequisite_id WHERE d.operation_id=$1", id).Scan(&state)
	if err == pgx.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	switch state {
	case "succeeded":
		return true, nil
	case "failed", "cancelled", "interrupted":
		_, err = s.DB.Exec(ctx, `UPDATE operations SET state='cancelled',result='{"error":"Deployment skipped because its prerequisite did not succeed."}',updated_at=now() WHERE id=$1 AND state='queued'`, id)
		return false, err
	default:
		_, err = s.DB.Exec(ctx, `UPDATE operations SET state='cancelled',result='{"error":"Deployment cancelled before its prerequisite completed."}',updated_at=now() WHERE id=$1 AND state='queued' AND cancel_requested`, id)
		return false, err
	}
}
