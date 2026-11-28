package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

func validateAutoscale(config workloadInput) error {
	if config.Autoscale == nil {
		return nil
	}
	policy := config.Autoscale.Normalized()
	if err := policy.Validate(); err != nil {
		return err
	}
	if !policy.Enabled {
		return nil
	}
	if config.Kind == "job" || config.Rollout == nil || config.Rollout.Strategy != "rolling" || len(config.Routes) == 0 || worker.HasTransportRoutes(config.Routes) || config.Private != nil || config.Hooks.Enabled() {
		return errors.New("CPU scaling requires a rolling HTTP service without private networking or deployment hooks")
	}
	maximum := map[string]int{}
	for name, count := range config.Rollout.Replicas {
		maximum[name] = count
	}
	for name, service := range policy.Services {
		limit := config.Limits[name]
		if limit.CPUs <= 0 || limit.MemoryMB <= 0 {
			return errors.New("each CPU-scaled service needs explicit CPU and memory limits")
		}
		maximum[name] = max(maximum[name], service.MaxReplicas)
	}
	options := *config.Rollout
	options.Replicas = maximum
	return worker.ValidateRequest(worker.Request{ID: "validation", Workload: "validation", Action: "deploy", Kind: "service", Compose: "services: {}", Rollout: &options})
}

func (s *Server) autoscalePolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "PUT" {
		var input struct {
			Enabled  *bool                              `json:"enabled"`
			Services map[string]worker.AutoscaleService `json:"services"`
		}
		if !decode(w, r, &input) {
			return
		}
		if input.Enabled == nil {
			http.Error(w, "enabled is required", 400)
			return
		}
		policy := (worker.AutoscalePolicy{Enabled: *input.Enabled, Services: input.Services}).Normalized()
		if err := policy.Validate(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		tx, err := s.DB.Begin(r.Context())
		if err != nil {
			http.Error(w, "CPU scaling unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		var encrypted []byte
		var supported, eligible, active bool
		err = tx.QueryRow(r.Context(), `SELECT w.configuration,COALESCE((n.capabilities->>'cpu_autoscale')::boolean,false),w.kind='service' AND NOT EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id) AND NOT EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id),EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w`, id).Scan(&encrypted, &supported, &eligible, &active)
		if err != nil {
			http.Error(w, "CPU scaling unavailable", 503)
			return
		}
		if !eligible {
			http.Error(w, "CPU scaling requires a service outside managed previews", 400)
			return
		}
		if policy.Enabled && !supported {
			http.Error(w, "upgrade and verify the worker before enabling CPU scaling", 409)
			return
		}
		if active {
			http.Error(w, "wait for the active operation before changing CPU scaling", 409)
			return
		}
		raw, err := unseal(s.Key, encrypted)
		var config workloadInput
		if err != nil || json.Unmarshal(raw, &config) != nil {
			http.Error(w, "configuration unavailable", 503)
			return
		}
		config.Autoscale = &policy
		if err = validateAutoscale(config); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if policy.Enabled {
			response, callErr := s.nodeRPC(r.Context(), config.NodeID, worker.RPCRequest{Method: "cpu_scale_sample", ID: id})
			if callErr != nil || response.Error != "" || response.CPUScale == nil {
				http.Error(w, "deploy a healthy eligible rolling release with CPU limits before enabling scaling", 409)
				return
			}
			for name := range policy.Services {
				if _, ok := response.CPUScale.CPUPercent[name]; !ok || response.CPUScale.Replicas[name] < 1 {
					http.Error(w, "a selected service has no deployed CPU limit or complete replica metrics", 409)
					return
				}
			}
		}
		encrypted, err = s.sealJSON(config)
		if err != nil {
			http.Error(w, "could not protect configuration", 503)
			return
		}
		cpu, memory := configuredUsage(config)
		_, err = tx.Exec(r.Context(), `UPDATE workloads SET configuration=$2,configured_cpu=$3,configured_memory_mb=$4,cpu_samples='[]',cpu_scale_error='',cpu_scale_status=CASE WHEN $5 THEN 'waiting_for_samples' ELSE 'disabled' END,next_cpu_check_at=CASE WHEN $5 THEN now() ELSE NULL END WHERE id=$1`, id, encrypted, cpu, memory, policy.Enabled)
		if quotaError(w, err) {
			return
		}
		if err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			http.Error(w, "could not save CPU scaling", 503)
			return
		}
	}
	var encrypted []byte
	var samples json.RawMessage
	var queued *time.Time
	var operation, status, message string
	err := s.DB.QueryRow(r.Context(), `SELECT configuration,cpu_samples,last_cpu_scale_at,cpu_scale_operation_id,cpu_scale_status,cpu_scale_error FROM workloads WHERE id=$1`, id).Scan(&encrypted, &samples, &queued, &operation, &status, &message)
	if err != nil {
		http.Error(w, "CPU scaling unavailable", 503)
		return
	}
	raw, err := unseal(s.Key, encrypted)
	var config workloadInput
	if err != nil || json.Unmarshal(raw, &config) != nil {
		http.Error(w, "configuration unavailable", 503)
		return
	}
	policy := worker.AutoscalePolicy{}.Normalized()
	if config.Autoscale != nil {
		policy = config.Autoscale.Normalized()
	}
	respond(w, 200, map[string]any{"policy": policy, "samples": samples, "last_queued_at": queued, "operation_id": operation, "status": status, "last_error": message})
}

func (s *Server) scheduleCPUScales(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `UPDATE workloads SET next_cpu_check_at=now()+interval '1 minute' WHERE id IN (SELECT w.id FROM workloads w WHERE w.kind='service' AND w.next_cpu_check_at<=now() ORDER BY w.next_cpu_check_at LIMIT 4 FOR UPDATE SKIP LOCKED) RETURNING id`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	var jobs sync.WaitGroup
	failures := make(chan error, len(ids))
	for _, id := range ids {
		jobs.Go(func() {
			if err := s.checkCPUScale(ctx, id); err != nil {
				failures <- err
			}
		})
	}
	jobs.Wait()
	close(failures)
	for err := range failures {
		return err
	}
	return nil
}

func (s *Server) checkCPUScale(ctx context.Context, id string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var encrypted []byte
	var node string
	var samples []worker.CPUScaleObservation
	var rawSamples []byte
	var last *time.Time
	var available bool
	err = tx.QueryRow(ctx, `SELECT w.configuration,w.node_id,w.cpu_samples,w.last_cpu_scale_at,n.state='ready' AND COALESCE((n.capabilities->>'cpu_autoscale')::boolean,false) AND NOT w.manually_stopped AND NOT EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id) AND NOT EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id) AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) AND NOT EXISTS(SELECT 1 FROM environments e WHERE e.id=w.environment_id AND e.paused) AND NOT EXISTS(SELECT 1 FROM environment_suspensions c WHERE c.workload_id=w.id) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w`, id).Scan(&encrypted, &node, &rawSamples, &last, &available)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	raw, err := unseal(s.Key, encrypted)
	if err != nil {
		return err
	}
	var config workloadInput
	if err = json.Unmarshal(raw, &config); err != nil {
		return err
	}
	status, message := "disabled", ""
	if config.Autoscale != nil && config.Autoscale.Enabled {
		status = "waiting"
		if available {
			response, callErr := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "cpu_scale_sample", ID: id})
			if callErr != nil || response.Error != "" || response.CPUScale == nil {
				status, message = "unavailable", "Could not read a complete healthy CPU sample; no scaling was queued."
			} else {
				if err = json.Unmarshal(rawSamples, &samples); err != nil {
					return err
				}
				samples = append(samples, *response.CPUScale)
				if len(samples) > 3 {
					samples = samples[len(samples)-3:]
				}
				next, reason := worker.CPUScaleDecision(*config.Autoscale, samples, time.Now().UTC(), last)
				status = reason
				if next != nil {
					request := worker.Request{ID: resourceID(), Workload: id, Action: "deploy", Kind: "service", ComposeBundle: config.ComposeBundle, Compose: config.Compose, Source: config.Source, Environment: config.Environment, Routes: config.Routes, Private: config.Private, Limits: config.Limits, Rollout: config.Rollout, Scale: &worker.CPUScaleOptions{Generation: response.CPUScale.Generation, Before: response.CPUScale.Replicas, Replicas: next, CPUPercent: response.CPUScale.CPUPercent}}
					if err = loadNetworkRequest(ctx, tx, &request); err != nil {
						return err
					}
					if err = worker.ValidateRequest(request); err != nil {
						return err
					}
					payload, err := s.sealJSON(request)
					if err != nil {
						return err
					}
					if _, err = tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'deploy',$3)`, request.ID, id, payload); err != nil {
						return err
					}
					if _, err = tx.Exec(ctx, `UPDATE workloads SET last_cpu_scale_at=now(),cpu_scale_operation_id=$2 WHERE id=$1`, id, request.ID); err != nil {
						return err
					}
					if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status) VALUES('cpu-scaler','scale service replicas',$1,202)`, id); err != nil {
						return err
					}
					status = "queued"
					samples = nil
				}
			}
		}
	}
	if samples == nil {
		samples = []worker.CPUScaleObservation{}
	}
	saved, err := json.Marshal(samples)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE workloads SET cpu_samples=$2,cpu_scale_status=$3,cpu_scale_error=$4 WHERE id=$1`, id, saved, status, message); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) commitCPUScale(ctx context.Context, id string, request worker.Request, result []byte) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var encrypted []byte
	if err = tx.QueryRow(ctx, `SELECT configuration FROM workloads WHERE id=$1 FOR UPDATE`, request.Workload).Scan(&encrypted); err != nil {
		return err
	}
	raw, err := unseal(s.Key, encrypted)
	if err != nil {
		return err
	}
	var config workloadInput
	if err = json.Unmarshal(raw, &config); err != nil {
		return err
	}
	if config.Rollout == nil || !reflect.DeepEqual(config.Rollout, request.Rollout) {
		return errors.New("CPU scaling configuration changed while its operation was active")
	}
	options := *config.Rollout
	options.Replicas = map[string]int{}
	for name, count := range config.Rollout.Replicas {
		options.Replicas[name] = count
	}
	for name, count := range request.Scale.Replicas {
		options.Replicas[name] = count
	}
	config.Rollout = &options
	encrypted, err = s.sealJSON(config)
	if err != nil {
		return err
	}
	cpu, memory := configuredUsage(config)
	if _, err = tx.Exec(ctx, `UPDATE workloads SET configuration=$2,configured_cpu=GREATEST(configured_cpu,$3),configured_memory_mb=GREATEST(configured_memory_mb,$4),cpu_samples='[]',cpu_scale_status='cooldown',cpu_scale_error='' WHERE id=$1`, request.Workload, encrypted, cpu, memory); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE operations SET state='succeeded',result=$2,updated_at=now() WHERE id=$1`, id, result); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func confirmedCPUScale(request *worker.CPUScaleOptions, result *worker.Operation) bool {
	if request == nil || result == nil || !reflect.DeepEqual(request, result.Scale) || result.Rollout == nil || result.Rollout.Phase != "completed" {
		return false
	}
	for name, count := range request.Replicas {
		if result.Rollout.Replicas[name] != count {
			return false
		}
	}
	return true
}
