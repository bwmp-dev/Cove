package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var errIdleAsleep = errors.New("start the sleeping resource before deploying, promoting or restoring its data")

func (s *Server) idleOperationGate(ctx context.Context, tx pgx.Tx, id, action string) error {
	if action != "deploy" && action != "rollback" && action != "rollout_promote" && action != "restore" && action != "drill" {
		return nil
	}
	var enabled bool
	var node string
	if err := tx.QueryRow(ctx, `SELECT node_id,COALESCE((idle_policy->>'enabled')::boolean,false) FROM workloads WHERE id=$1`, id).Scan(&node, &enabled); err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "container_health", ID: id})
	if err != nil || response.Error != "" {
		return errors.New("could not confirm whether the app is asleep")
	}
	if suspension := response.Suspension; suspension != nil && suspension.Reason == "idle" && suspension.Phase != "awake" {
		return errIdleAsleep
	}
	return nil
}

func (s *Server) idlePolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "GET" {
		var policy json.RawMessage
		err := s.DB.QueryRow(r.Context(), "SELECT idle_policy FROM workloads WHERE id=$1", id).Scan(&policy)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "idle policy unavailable", 503)
			return
		}
		respond(w, 200, policy)
		return
	}
	var in struct {
		Enabled       *bool `json:"enabled"`
		AfterSeconds  int   `json:"after_seconds"`
		RequestDriven bool  `json:"request_driven"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Enabled == nil {
		http.Error(w, "enabled is required", 400)
		return
	}
	policy := worker.IdlePolicy{Enabled: *in.Enabled, AfterSeconds: in.AfterSeconds, RequestDriven: in.RequestDriven}
	request := worker.Request{ID: resourceID(), Workload: id, Action: "idle_policy", Idle: &policy}
	if err := worker.ValidateRequest(request); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "idle policy unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var eligible, supported, ready, active, paused bool
	err = tx.QueryRow(r.Context(), `SELECT w.kind='service' AND NOT EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id) AND NOT EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id),COALESCE((n.capabilities->>'idle_sleep')::boolean,false),n.state='ready',EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')),w.manually_stopped OR EXISTS(SELECT 1 FROM environments e WHERE e.id=w.environment_id AND e.paused) OR EXISTS(SELECT 1 FROM environment_suspensions c WHERE c.workload_id=w.id) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w`, id).Scan(&eligible, &supported, &ready, &active, &paused)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "idle policy unavailable", 503)
		return
	}
	if !eligible {
		http.Error(w, "idle sleep requires a service outside managed previews", 400)
		return
	}
	if !supported {
		http.Error(w, "upgrade and verify the worker before changing idle sleep", 409)
		return
	}
	if !ready {
		http.Error(w, "the worker must be ready before changing idle sleep", 409)
		return
	}
	if active {
		http.Error(w, "wait for the active operation before changing idle sleep", 409)
		return
	}
	if policy.Enabled && paused {
		http.Error(w, "start the resource and resume its environment before enabling idle sleep", 409)
		return
	}
	if policy.Enabled {
		var linked bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM resource_connections WHERE source_id=$1 OR target_id=$1)`, id).Scan(&linked); err != nil {
			http.Error(w, "private connection state unavailable", 503)
			return
		}
		if linked {
			http.Error(w, "remove private connections before enabling idle sleep", 409)
			return
		}
	}
	payload, err := s.sealJSON(request)
	if err != nil {
		http.Error(w, "could not protect idle policy operation", 500)
		return
	}
	_, err = tx.Exec(r.Context(), "INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'idle_policy',$3)", request.ID, id, payload)
	var conflict *pgconn.PgError
	if errors.As(err, &conflict) && conflict.Code == "23505" {
		http.Error(w, "wait for the active operation before changing idle sleep", 409)
		return
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not queue idle policy operation", 503)
		return
	}
	respond(w, 202, map[string]string{"id": request.ID, "state": "queued"})
}

func (s *Server) commitIdlePolicy(ctx context.Context, id string, request worker.Request, result []byte) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	policy := *request.Idle
	if policy.AfterSeconds == 0 {
		policy.AfterSeconds = 300
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE workloads SET idle_policy=$2 WHERE id=$1", request.Workload, data); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE operations SET state='succeeded',result=$2,updated_at=now() WHERE id=$1", id, result); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) deferIdlePolicyResult(ctx context.Context, id string) error {
	result, err := json.Marshal(map[string]string{"error": "Waiting for the worker to confirm the idle-policy outcome; the resource remains locked."})
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, "UPDATE operations SET result=$2,updated_at=now() WHERE id=$1", id, result)
	return err
}
