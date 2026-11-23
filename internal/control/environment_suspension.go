package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

type suspendedResource struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Node         string `json:"node_id"`
	Supported    bool   `json:"supported"`
	Phase        string `json:"phase"`
	Operation    string `json:"operation_id,omitempty"`
	ResumeWanted bool   `json:"resume_wanted"`
	Error        string `json:"error,omitempty"`
}

var errEnvironmentPaused = errors.New("resume the environment before starting or replacing its resources")

func environmentOperationGate(ctx context.Context, tx pgx.Tx, id, action string) (bool, error) {
	switch action {
	case "deploy", "rollback", "start", "restart", "rollout_promote", "restore", "drill":
	default:
		return false, nil
	}
	var paused bool
	err := tx.QueryRow(ctx, `SELECT e.paused FROM environments e JOIN workloads w ON w.environment_id=e.id WHERE w.id=$1 FOR SHARE OF e`, id).Scan(&paused)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if paused {
		return false, errEnvironmentPaused
	}
	var failure string
	err = tx.QueryRow(ctx, `SELECT error FROM environment_suspensions WHERE workload_id=$1`, id).Scan(&failure)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if failure != "" && (action == "deploy" || action == "start" || action == "restart" || action == "restore") {
		return true, nil
	}
	return false, errors.Join(errEnvironmentPaused, errors.New("wait for the pending environment action; failed resumes can be retried from the environment"))
}

func (s *Server) environmentSuspension(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "PUT" {
		var in struct {
			Paused *bool `json:"paused"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Paused == nil {
			http.Error(w, "paused is required", 400)
			return
		}
		tx, err := s.DB.Begin(r.Context())
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		var preview, unsupported bool
		err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM environment_previews WHERE environment_id=e.id),EXISTS(SELECT 1 FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.environment_id=e.id AND NOT COALESCE((n.capabilities->>'suspensions')::boolean,false)) FROM environments e WHERE e.id=$1 FOR UPDATE OF e`, id).Scan(&preview, &unsupported)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		if preview {
			http.Error(w, "preview environments are managed by their preview", 409)
			return
		}
		if unsupported {
			http.Error(w, "upgrade and verify every worker in the environment before changing its suspension", 409)
			return
		}
		if _, err = tx.Exec(r.Context(), `UPDATE environments SET paused=$2 WHERE id=$1`, id, *in.Paused); err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE environment_suspensions c SET operation_id=NULL,action='',error='' WHERE environment_id=$1 AND (error<>'' OR EXISTS(SELECT 1 FROM operations o WHERE o.id=c.operation_id AND o.state IN ('failed','cancelled','interrupted')))`, id)
		}
		if err == nil && *in.Paused {
			_, err = tx.Exec(r.Context(), `UPDATE operations o SET cancel_requested=true,updated_at=now() FROM workloads w WHERE o.workload_id=w.id AND w.environment_id=$1 AND o.state IN ('queued','running') AND o.action NOT IN ('suspend','resume')`, id)
		}
		if err == nil && *in.Paused {
			_, err = tx.Exec(r.Context(), `DELETE FROM webhook_deliveries d USING workloads w WHERE d.workload_id=w.id AND w.environment_id=$1 AND d.operation_id IS NULL`, id)
		}
		if err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			http.Error(w, "could not save environment suspension", 503)
			return
		}
		s.readEnvironmentSuspension(w, r, 202)
		return
	}
	s.readEnvironmentSuspension(w, r, 200)
}

func (s *Server) readEnvironmentSuspension(w http.ResponseWriter, r *http.Request, code int) {
	tx, err := s.DB.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		http.Error(w, "environment state unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var paused bool
	if err := tx.QueryRow(r.Context(), `SELECT paused FROM environments WHERE id=$1`, r.PathValue("id")).Scan(&paused); err != nil {
		http.Error(w, "environment state unavailable", 503)
		return
	}
	rows, err := tx.Query(r.Context(), `SELECT w.id,w.name,w.kind,w.node_id,COALESCE((n.capabilities->>'suspensions')::boolean,false),c.workload_id IS NOT NULL,COALESCE(c.suspended,false),COALESCE(c.resume_wanted,false),COALESCE(c.operation_id,''),COALESCE(c.action,''),COALESCE(o.state,''),COALESCE(c.error,'') FROM workloads w JOIN nodes n ON n.id=w.node_id LEFT JOIN environment_suspensions c ON c.workload_id=w.id LEFT JOIN operations o ON o.id=c.operation_id WHERE w.environment_id=$1 ORDER BY w.name,w.id`, r.PathValue("id"))
	if err != nil {
		http.Error(w, "environment state unavailable", 503)
		return
	}
	defer rows.Close()
	items := []suspendedResource{}
	phase := "active"
	if paused {
		phase = "paused"
	}
	for rows.Next() {
		var v suspendedResource
		var checkpoint, suspended bool
		var action, state string
		if err = rows.Scan(&v.ID, &v.Name, &v.Kind, &v.Node, &v.Supported, &checkpoint, &suspended, &v.ResumeWanted, &v.Operation, &action, &state, &v.Error); err != nil {
			break
		}
		v.Phase = "active"
		if suspended {
			v.Phase = "paused"
		}
		if paused && !suspended || state == "queued" || state == "running" {
			v.Phase = "pausing"
			if action == "resume" {
				v.Phase = "resuming"
			}
		}
		if !paused && checkpoint && v.Phase != "resuming" {
			v.Phase = "resuming"
		}
		if v.Error != "" || state == "failed" || state == "cancelled" || state == "interrupted" {
			v.Phase = "error"
		}
		if v.Phase == "error" {
			phase = "error"
		} else if phase != "error" && (v.Phase == "pausing" || v.Phase == "resuming") {
			phase = "resuming"
			if paused {
				phase = "pausing"
			}
		}
		items = append(items, v)
	}
	if err != nil || rows.Err() != nil {
		http.Error(w, "environment state unavailable", 503)
		return
	}
	rows.Close()
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "environment state unavailable", 503)
		return
	}
	respond(w, code, map[string]any{"paused": paused, "phase": phase, "resources": items})
}

func (s *Server) reconcileEnvironmentSuspensions(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT w.id FROM workloads w LEFT JOIN environments e ON e.id=w.environment_id WHERE COALESCE(e.paused,false) OR EXISTS(SELECT 1 FROM environment_suspensions c WHERE c.workload_id=w.id) ORDER BY w.id`)
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
	for _, id := range ids {
		if err = s.reconcileResourceSuspension(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) reconcileResourceSuspension(ctx context.Context, id string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var env, kind string
	var stopped bool
	var config []byte
	err = tx.QueryRow(ctx, `SELECT COALESCE(environment_id,''),kind,manually_stopped,configuration FROM workloads WHERE id=$1 FOR UPDATE`, id).Scan(&env, &kind, &stopped, &config)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var paused bool
	if env != "" {
		if err = tx.QueryRow(ctx, `SELECT paused FROM environments WHERE id=$1 FOR SHARE`, env).Scan(&paused); err != nil {
			return err
		}
	}
	if paused {
		if _, err = tx.Exec(ctx, `INSERT INTO environment_suspensions(workload_id,environment_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, env); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE workloads SET job_pending_at=NULL WHERE id=$1`, id); err != nil {
			return err
		}
	}
	var scope, op, action, failure string
	var suspended, wanted bool
	err = tx.QueryRow(ctx, `SELECT environment_id,COALESCE(operation_id,''),action,suspended,resume_wanted,error FROM environment_suspensions WHERE workload_id=$1 FOR UPDATE`, id).Scan(&scope, &op, &action, &suspended, &wanted, &failure)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if failure != "" {
		return tx.Commit(ctx)
	}
	if op != "" && action != "" {
		var state, operationAction string
		var raw []byte
		if err = tx.QueryRow(ctx, `SELECT state,result,action FROM operations WHERE id=$1`, op).Scan(&state, &raw, &operationAction); err != nil {
			return err
		}
		if state == "queued" || state == "running" {
			return tx.Commit(ctx)
		}
		if state != "succeeded" {
			_, err = tx.Exec(ctx, `UPDATE environment_suspensions SET error=$2 WHERE workload_id=$1`, id, "The "+action+" operation ended "+state+". Inspect its output and retry the environment action.")
			if err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		if action == "suspend" {
			var result worker.Operation
			if json.Unmarshal(raw, &result) != nil || result.Suspension == nil || result.Suspension.Phase != "paused" {
				_, err = tx.Exec(ctx, `UPDATE environment_suspensions SET error='Worker did not confirm the pause; inspect its output and retry.' WHERE workload_id=$1`, id)
				if err != nil {
					return err
				}
				return tx.Commit(ctx)
			}
			suspended, wanted = true, kind != "job" && !stopped && result.Suspension.PreviouslyRunning > 0
		} else if operationAction == "resume" {
			var result worker.Operation
			if json.Unmarshal(raw, &result) != nil || result.Suspension == nil || (result.Suspension.Phase != "resumed" && result.Suspension.Phase != "stopped") {
				_, err = tx.Exec(ctx, `UPDATE environment_suspensions SET error='Worker did not confirm the resume; inspect its output and retry.' WHERE workload_id=$1`, id)
				if err != nil {
					return err
				}
				return tx.Commit(ctx)
			}
			suspended = false
		} else {
			suspended = false
		}
		op = ""
		if _, err = tx.Exec(ctx, `UPDATE environment_suspensions SET action='',suspended=$2,resume_wanted=$3 WHERE workload_id=$1`, id, suspended, wanted); err != nil {
			return err
		}
	}
	if !paused && !suspended {
		if kind == "job" {
			var expression, zone string
			if err = tx.QueryRow(ctx, `SELECT job_cron,job_timezone FROM workloads WHERE id=$1`, id).Scan(&expression, &zone); err != nil {
				return err
			}
			if expression != "" {
				cron, err := parseCron(expression)
				if err != nil {
					return err
				}
				cron.location, err = time.LoadLocation(zone)
				if err != nil {
					return err
				}
				next, err := cron.next(time.Now())
				if err != nil {
					return err
				}
				if _, err = tx.Exec(ctx, `UPDATE workloads SET next_job_at=$2,job_pending_at=NULL WHERE id=$1`, id, next); err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM environment_suspensions WHERE workload_id=$1`, id)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if paused && suspended {
		return tx.Commit(ctx)
	}
	var active bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))`, id).Scan(&active); err != nil {
		return err
	}
	if active {
		if paused {
			_, err = tx.Exec(ctx, `UPDATE operations SET cancel_requested=true,updated_at=now() WHERE workload_id=$1 AND state IN ('queued','running') AND action NOT IN ('suspend','resume')`, id)
		}
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var supported bool
	if err = tx.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'suspensions')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, id).Scan(&supported); err != nil {
		return err
	}
	if !supported {
		_, err = tx.Exec(ctx, `UPDATE environment_suspensions SET error='Upgrade and verify the worker, then retry the environment action.' WHERE workload_id=$1`, id)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	action = "resume"
	if paused {
		action = "suspend"
	}
	raw, err := unseal(s.Key, config)
	if err != nil {
		return err
	}
	var input workloadInput
	if err = json.Unmarshal(raw, &input); err != nil {
		return err
	}
	request := worker.Request{ID: resourceID(), Workload: id, Action: action, Kind: kind, JobService: input.JobService, Suspension: &worker.SuspensionOptions{Reason: "environment", Scope: scope, Start: !paused && wanted && !stopped && kind != "job"}}
	if err = worker.ValidateRequest(request); err != nil {
		return err
	}
	payload, err := s.sealJSON(request)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,$3,$4)`, request.ID, id, action, payload); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE environment_suspensions SET operation_id=$2,action=$3 WHERE workload_id=$1`, id, request.ID, action); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status) VALUES('environment-controller',$1,$2,202)`, action+" environment resource", id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
