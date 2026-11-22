package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"net/http"
	"time"
)

func (s *Server) jobSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "GET" {
		var expression, timezone string
		var next *time.Time
		if err := s.DB.QueryRow(r.Context(), `SELECT job_cron,next_job_at,job_timezone FROM workloads WHERE id=$1`, id).Scan(&expression, &next, &timezone); err != nil {
			http.Error(w, "schedule unavailable", 503)
			return
		}
		respond(w, 200, map[string]any{"cron": expression, "timezone": timezone, "next_run_at": next})
		return
	}
	var in struct {
		Cron     string  `json:"cron"`
		Timezone *string `json:"timezone"`
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
	var timezone string
	if err = tx.QueryRow(r.Context(), `SELECT configuration,job_timezone FROM workloads WHERE id=$1 FOR UPDATE`, id).Scan(&encrypted, &timezone); err != nil {
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
	if _, err = tx.Exec(r.Context(), `UPDATE workloads SET job_cron=$2,next_job_at=$3,job_timezone=$4 WHERE id=$1`, id, in.Cron, next, timezone); err != nil {
		http.Error(w, "could not save schedule", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save schedule", 503)
		return
	}
	respond(w, 200, map[string]any{"cron": in.Cron, "timezone": timezone, "next_run_at": next})
}

func (s *Server) scheduleJobs(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id,configuration,job_cron,manually_stopped,job_timezone FROM workloads WHERE job_cron<>'' AND next_job_at<=now() ORDER BY next_job_at LIMIT 16 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return err
	}
	type due struct {
		id, cron      string
		configuration []byte
		stopped       bool
		timezone      string
	}
	jobs := []due{}
	for rows.Next() {
		var j due
		if err = rows.Scan(&j.id, &j.configuration, &j.cron, &j.stopped, &j.timezone); err != nil {
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
		if config.Kind == "job" && !j.stopped {
			request := worker.Request{ID: resourceID(), Workload: j.id, Action: "deploy", Kind: config.Kind, JobService: config.JobService, Compose: config.Compose, Source: config.Source, Environment: config.Environment, Limits: config.Limits, Hooks: config.Hooks, FileSecrets: config.FileSecrets, GuardedDeploy: config.GuardedDeploy}
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
				if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status) VALUES('scheduler','scheduled job',$1,202)`, j.id); err != nil {
					return err
				}
			}
		}
		// Missed ticks and overlapping runs are skipped, never replayed in a burst after downtime.
		if _, err = tx.Exec(ctx, `UPDATE workloads SET next_job_at=$2 WHERE id=$1`, j.id, next); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
