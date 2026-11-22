package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"sync"
)

func (s *Server) unhealthyRestartPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "PUT" {
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Enabled == nil {
			http.Error(w, "enabled is required", 400)
			return
		}
		enabled := *in.Enabled
		tx, err := s.DB.Begin(r.Context())
		if err != nil {
			http.Error(w, "restart policy unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		var supported, eligible bool
		if err = tx.QueryRow(r.Context(), `SELECT COALESCE((n.capabilities->>'unhealthy_restart')::boolean,false),w.kind='service' AND NOT EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id) AND NOT EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w`, id).Scan(&supported, &eligible); err != nil {
			http.Error(w, "restart policy unavailable", 503)
			return
		}
		if enabled && !eligible {
			http.Error(w, "automatic health restarts require a service outside managed previews", 400)
			return
		}
		if enabled && !supported {
			http.Error(w, "upgrade and verify the worker before enabling health restarts", 409)
			return
		}
		if _, err = tx.Exec(r.Context(), `UPDATE workloads SET restart_unhealthy=$2,next_unhealthy_check_at=now() WHERE id=$1`, id, enabled); err != nil {
			http.Error(w, "could not save restart policy", 503)
			return
		}
		if !enabled {
			if _, err = tx.Exec(r.Context(), `UPDATE operations SET cancel_requested=true,updated_at=now() WHERE workload_id=$1 AND action='restart_unhealthy' AND state='queued'`, id); err != nil {
				http.Error(w, "could not cancel pending health restarts", 503)
				return
			}
		}
		if err = tx.Commit(r.Context()); err != nil {
			http.Error(w, "could not save restart policy", 503)
			return
		}
	}
	var value json.RawMessage
	if err := s.DB.QueryRow(r.Context(), `SELECT jsonb_build_object('enabled',restart_unhealthy,'last_queued_at',last_unhealthy_restart_at,'attempts_in_window',(SELECT count(*) FROM operations o WHERE o.workload_id=w.id AND o.action='restart_unhealthy' AND o.created_at>now()-interval '1 hour'),'window_started_at',(SELECT min(o.created_at) FROM operations o WHERE o.workload_id=w.id AND o.action='restart_unhealthy' AND o.created_at>now()-interval '1 hour'),'operation_id',unhealthy_restart_operation_id,'last_error',unhealthy_restart_error) FROM workloads w WHERE id=$1`, id).Scan(&value); err != nil {
		http.Error(w, "restart policy unavailable", 503)
		return
	}
	respond(w, 200, value)
}

func (s *Server) scheduleUnhealthyRestarts(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `UPDATE workloads w SET next_unhealthy_check_at=now()+interval '1 minute' WHERE w.id IN (SELECT w.id FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.restart_unhealthy AND w.kind='service' AND NOT w.manually_stopped AND NOT EXISTS(SELECT 1 FROM environments e WHERE e.id=w.environment_id AND e.paused) AND NOT EXISTS(SELECT 1 FROM environment_suspensions c WHERE c.workload_id=w.id) AND n.state='ready' AND COALESCE((n.capabilities->>'unhealthy_restart')::boolean,false) AND w.next_unhealthy_check_at<=now() AND (w.last_unhealthy_restart_at IS NULL OR w.last_unhealthy_restart_at<now()-interval '5 minutes') AND (SELECT count(*) FROM operations o WHERE o.workload_id=w.id AND o.action='restart_unhealthy' AND o.created_at>now()-interval '1 hour')<3 AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) ORDER BY w.next_unhealthy_check_at LIMIT 8 FOR UPDATE OF w SKIP LOCKED) RETURNING w.id,w.node_id`)
	if err != nil {
		return err
	}
	type candidate struct{ id, node string }
	items := []candidate{}
	for rows.Next() {
		var v candidate
		if err = rows.Scan(&v.id, &v.node); err != nil {
			break
		}
		items = append(items, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	failures := make(chan error, len(items))
	for _, v := range items {
		wg.Go(func() {
			if err := s.checkUnhealthyRestart(ctx, v.id, v.node); err != nil {
				failures <- err
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		return err
	}
	return nil
}

func (s *Server) checkUnhealthyRestart(ctx context.Context, id, node string) error {
	response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "container_health", ID: id})
	if err != nil || response.Error != "" {
		_, err = s.DB.Exec(ctx, `UPDATE workloads SET unhealthy_restart_error='Could not inspect Docker health; no automatic restart was queued.' WHERE id=$1`, id)
		return err
	}
	unhealthy := false
	for _, v := range response.Containers {
		if v.State == "running" && v.Health == "unhealthy" {
			unhealthy = true
			break
		}
	}
	if !unhealthy {
		_, err = s.DB.Exec(ctx, `UPDATE workloads SET unhealthy_restart_error='' WHERE id=$1`, id)
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var eligible bool
	if err = tx.QueryRow(ctx, `SELECT w.restart_unhealthy AND w.kind='service' AND NOT w.manually_stopped AND NOT EXISTS(SELECT 1 FROM environments e WHERE e.id=w.environment_id AND e.paused) AND NOT EXISTS(SELECT 1 FROM environment_suspensions c WHERE c.workload_id=w.id) AND w.node_id=$2 AND n.state='ready' AND COALESCE((n.capabilities->>'unhealthy_restart')::boolean,false) AND NOT EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id) AND NOT EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id) AND (w.last_unhealthy_restart_at IS NULL OR w.last_unhealthy_restart_at<now()-interval '5 minutes') AND (SELECT count(*) FROM operations o WHERE o.workload_id=w.id AND o.action='restart_unhealthy' AND o.created_at>now()-interval '1 hour')<3 AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.result->'rollout'->>'phase'='waiting') FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w`, id, node).Scan(&eligible); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if !eligible {
		return nil
	}
	request := worker.Request{ID: resourceID(), Workload: id, Kind: "service", Action: "restart_unhealthy"}
	if err = worker.ValidateRequest(request); err != nil {
		return err
	}
	payload, err := s.sealJSON(request)
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'restart_unhealthy',$3) ON CONFLICT DO NOTHING`, request.ID, id, payload)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return nil
	}
	if _, err = tx.Exec(ctx, `UPDATE workloads SET last_unhealthy_restart_at=now(),unhealthy_restart_operation_id=$2,unhealthy_restart_error='' WHERE id=$1`, id, request.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status) VALUES('health-monitor','restart unhealthy containers',$1,202)`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
