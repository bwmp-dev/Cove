package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"time"
)

func (s *Server) jobSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "GET" {
		var expression, timezone, overlap string
		var timeout int
		var next, pending *time.Time
		if err := s.DB.QueryRow(r.Context(), `SELECT job_cron,next_job_at,job_timezone,job_overlap_policy,job_timeout_seconds,job_pending_at FROM workloads WHERE id=$1`, id).Scan(&expression, &next, &timezone, &overlap, &timeout, &pending); err != nil {
			http.Error(w, "schedule unavailable", 503)
			return
		}
		respond(w, 200, map[string]any{"cron": expression, "timezone": timezone, "next_run_at": next, "overlap_policy": overlap, "timeout_seconds": timeout, "pending_since": pending})
		return
	}
	var in struct {
		Cron           string  `json:"cron"`
		Timezone       *string `json:"timezone"`
		OverlapPolicy  *string `json:"overlap_policy"`
		TimeoutSeconds *int    `json:"timeout_seconds"`
	}
	if !decode(w, r, &in) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "schedule unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var encrypted []byte
	var timezone, overlap string
	var timeout int
	if err = tx.QueryRow(r.Context(), `SELECT configuration,job_timezone,job_overlap_policy,job_timeout_seconds FROM workloads WHERE id=$1 FOR UPDATE`, id).Scan(&encrypted, &timezone, &overlap, &timeout); err != nil {
		http.Error(w, "schedule unavailable", 503)
		return
	}
	raw, err := unseal(s.Key, encrypted)
	var config workloadInput
	if err != nil || json.Unmarshal(raw, &config) != nil {
		http.Error(w, "configuration unavailable", 503)
		return
	}
	if in.Cron != "" && config.Kind != "job" {
		http.Error(w, "schedules require a one-shot job resource", 400)
		return
	}
	if in.Timezone != nil {
		timezone = *in.Timezone
	}
	if in.OverlapPolicy != nil {
		overlap = *in.OverlapPolicy
	}
	if in.TimeoutSeconds != nil {
		timeout = *in.TimeoutSeconds
	}
	if overlap != "skip" && overlap != "queue_one" && overlap != "replace" || timeout < 5 || timeout > 86400 {
		http.Error(w, "use skip, queue_one or replace and a timeout from 5 to 86400 seconds", 400)
		return
	}
	if config.Kind != "job" && (overlap != "skip" || timeout != worker.DefaultJobTimeoutSeconds) {
		http.Error(w, "execution settings require a one-shot job resource", 400)
		return
	}
	if overlap != "skip" || timeout != worker.DefaultJobTimeoutSeconds {
		var supported bool
		if err = tx.QueryRow(r.Context(), `SELECT COALESCE((n.capabilities->>'job_runs')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, id).Scan(&supported); err != nil {
			http.Error(w, "worker capabilities unavailable", 503)
			return
		}
		if !supported {
			http.Error(w, "upgrade and verify the worker before configuring job execution", 409)
			return
		}
	}
	if timezone == "" || timezone == "Local" || len(timezone) > 128 {
		http.Error(w, "use UTC or an IANA timezone such as America/Los_Angeles", 400)
		return
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		http.Error(w, "unknown schedule timezone", 400)
		return
	}
	var next *time.Time
	if in.Cron != "" {
		schedule, err := parseCron(in.Cron)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		schedule.location = location
		value, err := schedule.next(time.Now())
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		next = &value
	}
	if _, err = tx.Exec(r.Context(), `UPDATE workloads SET job_cron=$2,next_job_at=$3,job_timezone=$4,job_overlap_policy=$5,job_timeout_seconds=$6,job_pending_at=NULL WHERE id=$1`, id, in.Cron, next, timezone, overlap, timeout); err != nil {
		http.Error(w, "could not save schedule", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save schedule", 503)
		return
	}
	respond(w, 200, map[string]any{"cron": in.Cron, "timezone": timezone, "next_run_at": next, "overlap_policy": overlap, "timeout_seconds": timeout, "pending_since": nil})
}

var errJobWorkerUnsupported = errors.New("upgrade and verify the worker before running this job")

func jobExecutionOptions(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (*worker.JobOptions, error) {
	var timeout int
	var supported bool
	if err := db.QueryRow(ctx, `SELECT w.job_timeout_seconds,COALESCE((n.capabilities->>'job_runs')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, id).Scan(&timeout, &supported); err != nil {
		return nil, err
	}
	if supported {
		return &worker.JobOptions{TimeoutSeconds: timeout}, nil
	}
	if timeout != worker.DefaultJobTimeoutSeconds {
		return nil, errJobWorkerUnsupported
	}
	return nil, nil
}

func (s *Server) scheduleJobs(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT w.id,w.configuration,w.job_cron,(w.manually_stopped OR EXISTS(SELECT 1 FROM environments e WHERE e.id=w.environment_id AND e.paused) OR EXISTS(SELECT 1 FROM environment_suspensions c WHERE c.workload_id=w.id)),w.job_timezone,w.job_overlap_policy,w.job_pending_at,w.next_job_at<=now(),COALESCE((n.capabilities->>'job_runs')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.job_cron<>'' AND (w.next_job_at<=now() OR w.job_pending_at IS NOT NULL) ORDER BY COALESCE(w.job_pending_at,w.next_job_at) LIMIT 16 FOR UPDATE OF w SKIP LOCKED`)
	if err != nil {
		return err
	}
	type due struct {
		id, cron        string
		configuration   []byte
		stopped         bool
		timezone        string
		overlap         string
		pending         *time.Time
		tick, supported bool
	}
	jobs := []due{}
	for rows.Next() {
		var j due
		if err = rows.Scan(&j.id, &j.configuration, &j.cron, &j.stopped, &j.timezone, &j.overlap, &j.pending, &j.tick, &j.supported); err != nil {
			break
		}
		jobs = append(jobs, j)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		cron, err := parseCron(j.cron)
		if err != nil {
			return err
		}
		cron.location, err = time.LoadLocation(j.timezone)
		if err != nil {
			return err
		}
		next, err := cron.next(time.Now())
		if err != nil {
			return err
		}
		raw, err := unseal(s.Key, j.configuration)
		if err != nil {
			return err
		}
		var config workloadInput
		if err = json.Unmarshal(raw, &config); err != nil {
			return err
		}
		pending := j.pending
		if config.Kind != "job" || j.stopped {
			pending = nil
		} else {
			var activeID, action, state string
			err := tx.QueryRow(ctx, `SELECT id,action,state FROM operations WHERE workload_id=$1 AND state IN ('queued','running')`, j.id).Scan(&activeID, &action, &state)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if activeID != "" {
				// Metadata work must not consume a scheduled execution, even under Skip.
				if j.overlap != "skip" || action == "image_check" || action == "image_scan" {
					if pending == nil {
						value := time.Now().UTC()
						pending = &value
					}
					if j.overlap == "replace" && j.supported && action == "deploy" && state == "running" {
						result, err := tx.Exec(ctx, `UPDATE operations SET cancel_requested=true,updated_at=now() WHERE id=$1 AND NOT cancel_requested`, activeID)
						if err != nil {
							return err
						}
						if result.RowsAffected() > 0 {
							if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status) VALUES('scheduler','replace overlapping job',$1,202)`, j.id); err != nil {
								return err
							}
						}
					}
				}
			} else {
				request := worker.Request{ID: resourceID(), Workload: j.id, Action: "deploy", Kind: config.Kind, JobService: config.JobService, Compose: config.Compose, Source: config.Source, Environment: config.Environment, Limits: config.Limits, Hooks: config.Hooks, FileSecrets: config.FileSecrets, GuardedDeploy: config.GuardedDeploy, Rollout: config.Rollout}
				request.Job, err = jobExecutionOptions(ctx, tx, j.id)
				if err != nil {
					return err
				}
				if err = worker.ValidateRequest(request); err != nil {
					return err
				}
				raw, err = json.Marshal(request)
				if err != nil {
					return err
				}
				encrypted, err := seal(s.Key, raw)
				if err != nil {
					return err
				}
				result, err := tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'deploy',$3) ON CONFLICT DO NOTHING`, request.ID, j.id, encrypted)
				if err != nil {
					return err
				}
				if result.RowsAffected() > 0 {
					pending = nil
					if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status) VALUES('scheduler','scheduled job',$1,202)`, j.id); err != nil {
						return err
					}
				}
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE workloads SET next_job_at=CASE WHEN $4 THEN $2 ELSE next_job_at END,job_pending_at=$3 WHERE id=$1`, j.id, next, pending, j.tick); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
