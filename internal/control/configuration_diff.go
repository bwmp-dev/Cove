package control

import (
	"encoding/json"
	"errors"
	"net/http"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

func deploymentConfiguration(request worker.Request) map[string]json.RawMessage {
	if request.Scale != nil && request.Rollout != nil {
		options := *request.Rollout
		options.Replicas = map[string]int{}
		for service, count := range request.Rollout.Replicas {
			options.Replicas[service] = count
		}
		for service, count := range request.Scale.Replicas {
			options.Replicas[service] = count
		}
		request.Rollout = &options
	}
	config := workloadInput{Compose: request.Compose, ComposeBundle: request.ComposeBundle, Environment: request.Environment, Source: request.Source, Kind: request.Kind, JobService: request.JobService, Routes: request.Routes, Private: request.Private, Limits: request.Limits, Hooks: request.Hooks, FileSecrets: request.FileSecrets, GuardedDeploy: request.GuardedDeploy, Rollout: request.Rollout}
	if config.Source != nil {
		source := *config.Source
		source.Token, source.SSHPrivateKey, source.Revision = "", "", ""
		config.Source = &source
	}
	if config.Kind == "" {
		config.Kind = "service"
	}
	raw, _ := json.Marshal(config)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	return fields
}

func (s *Server) workloadConfigurationDiff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAccess(w, r, "workload", id, 1) {
		return
	}
	var encrypted []byte
	var config workloadInput
	if s.DB.QueryRow(r.Context(), `SELECT configuration FROM workloads WHERE id=$1`, id).Scan(&encrypted) != nil || s.unsealJSON(encrypted, &config) != nil {
		http.Error(w, "configuration unavailable", 503)
		return
	}
	var operation, revision string
	err := s.DB.QueryRow(r.Context(), `SELECT d.declaration_id,o.declared_payload,d.revision FROM cove_committed_deployments d JOIN operations o ON o.id=d.declaration_id WHERE d.workload_id=$1 ORDER BY d.updated_at DESC,d.operation_id DESC LIMIT 1`, id).Scan(&operation, &encrypted, &revision)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "deployment baseline unavailable", 503)
		return
	}
	hasBaseline := err == nil && len(encrypted) > 0
	before := map[string]json.RawMessage{}
	if hasBaseline {
		var request worker.Request
		seen := map[string]bool{}
		for step := 0; ; step++ {
			if s.unsealJSON(encrypted, &request) != nil {
				http.Error(w, "deployment baseline unavailable", 503)
				return
			}
			if request.Action != "rollback" {
				before = deploymentConfiguration(request)
				break
			}
			if step >= 32 || seen[request.Release] {
				http.Error(w, "rollback declaration chain is unavailable", 409)
				return
			}
			seen[request.Release] = true
			err = s.DB.QueryRow(r.Context(), `SELECT declared_payload FROM operations WHERE id=$1 AND workload_id=$2 AND state='succeeded' AND action IN ('deploy','rollback','adopt')`, request.Release, id).Scan(&encrypted)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && len(encrypted) == 0 {
				hasBaseline = false
				break
			}
			if err != nil {
				http.Error(w, "rollback declaration unavailable", 503)
				return
			}
		}
	}
	after := deploymentConfiguration(worker.Request{Compose: config.Compose, ComposeBundle: config.ComposeBundle, Environment: config.Environment, Source: config.Source, Kind: config.Kind, JobService: config.JobService, Routes: config.Routes, Private: config.Private, Limits: config.Limits, Hooks: config.Hooks, FileSecrets: config.FileSecrets, GuardedDeploy: config.GuardedDeploy, Rollout: config.Rollout})
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, map[string]any{"has_baseline": hasBaseline, "operation_id": operation, "revision": revision, "fields": configurationChanges(before, after), "diffs": safeConfigurationDiffs(before, after)})
}

func (s *Server) workloadDrift(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAccess(w, r, "workload", id, 1) {
		return
	}
	var node string
	var available bool
	if err := s.DB.QueryRow(r.Context(), `SELECT w.node_id,COALESCE((n.capabilities->>'drift')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, id).Scan(&node, &available); err != nil {
		http.Error(w, "worker metadata unavailable", 503)
		return
	}
	if !available {
		http.Error(w, "upgrade and verify the worker before checking configuration drift", 409)
		return
	}
	result, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "drift", ID: id})
	if err != nil || result.Drift == nil {
		http.Error(w, "runtime drift inspection unavailable; check worker connectivity and container limits", 502)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, result.Drift)
}
