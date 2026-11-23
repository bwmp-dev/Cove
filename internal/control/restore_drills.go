package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
)

func (s *Server) scheduleRestoreDrills(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT w.id,w.configuration,COALESCE((SELECT o.id FROM operations o WHERE o.workload_id=w.id AND o.action='backup' AND o.state='succeeded' ORDER BY o.updated_at DESC LIMIT 1),'') FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE NOT EXISTS(SELECT 1 FROM environments e WHERE e.id=w.environment_id AND e.paused) AND NOT EXISTS(SELECT 1 FROM environment_suspensions c WHERE c.workload_id=w.id) AND w.drill_interval>0 AND w.next_drill_at<=now() AND COALESCE((n.capabilities->>'restore_drills')::boolean,false) ORDER BY w.next_drill_at LIMIT 16 FOR UPDATE OF w SKIP LOCKED`)
	if err != nil {
		return err
	}
	type due struct {
		id, backup    string
		configuration []byte
	}
	jobs := []due{}
	for rows.Next() {
		var job due
		if err = rows.Scan(&job.id, &job.configuration, &job.backup); err != nil {
			break
		}
		jobs = append(jobs, job)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.backup == "" {
			if _, err = tx.Exec(ctx, "UPDATE workloads SET next_drill_at=now()+interval '1 hour' WHERE id=$1", job.id); err != nil {
				return err
			}
			continue
		}
		raw, err := unseal(s.Key, job.configuration)
		if err != nil {
			return err
		}
		var config workloadInput
		if err = json.Unmarshal(raw, &config); err != nil {
			return err
		}
		request := worker.Request{ID: resourceID(), Workload: job.id, Action: "drill", Kind: config.Kind, Backup: config.Backup, BackupID: job.backup}
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
		result, err := tx.Exec(ctx, "INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'drill',$3) ON CONFLICT DO NOTHING", request.ID, job.id, encrypted)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE workloads SET next_drill_at=CASE WHEN $2 THEN now()+make_interval(hours=>drill_interval) ELSE now()+interval '1 minute' END WHERE id=$1", job.id, result.RowsAffected() > 0); err != nil {
			return err
		}
		if result.RowsAffected() > 0 {
			if _, err = tx.Exec(ctx, "INSERT INTO audit_events(actor,action,subject,status) VALUES('scheduler','restore drill',$1,202)", job.id); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}
