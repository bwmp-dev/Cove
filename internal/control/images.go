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

func (s *Server) workloadImages(w http.ResponseWriter, r *http.Request) {
	var kind string
	if err := s.DB.QueryRow(r.Context(), "SELECT kind FROM workloads WHERE id=$1", r.PathValue("id")).Scan(&kind); errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, "image checks unavailable", 503)
		return
	}
	var data []byte
	var operation string
	var updated time.Time
	err := s.DB.QueryRow(r.Context(), `SELECT id,result,updated_at FROM operations WHERE workload_id=$1 AND action='image_check' AND state='succeeded' ORDER BY updated_at DESC LIMIT 1`, r.PathValue("id")).Scan(&operation, &data, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		respond(w, 200, map[string]any{"images": []worker.ImageStatus{}, "stale": true, "checked_at": nil})
		return
	}
	if err != nil {
		http.Error(w, "image checks unavailable", 503)
		return
	}
	var result worker.Operation
	if json.Unmarshal(data, &result) != nil || result.ImageCheck == nil {
		http.Error(w, "image check result unavailable", 503)
		return
	}
	var stale bool
	if err = s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND action IN ('deploy','rollback') AND (state='succeeded' OR $3='job') AND updated_at>$2)`, r.PathValue("id"), updated, kind).Scan(&stale); err != nil {
		http.Error(w, "image checks unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"images": result.ImageCheck.Images, "checked_at": result.ImageCheck.CheckedAt, "operation_id": operation, "stale": stale})
}

func (s *Server) scheduleImageChecks(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT w.id,w.kind,w.configuration FROM workloads w JOIN nodes n ON n.id=w.node_id
WHERE (w.kind='service' OR (w.kind='job' AND COALESCE((n.capabilities->>'job_image_lifecycle')::boolean,false))) AND NOT w.manually_stopped AND n.state='ready'
AND COALESCE((n.capabilities->>'image_lifecycle')::boolean,false) AND w.next_image_check_at<=now()
AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running'))
AND EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.action IN ('deploy','rollback') AND (o.state='succeeded' OR (w.kind='job' AND o.state IN ('failed','cancelled','interrupted'))))
AND NOT EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id)
AND NOT EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id)
ORDER BY w.next_image_check_at LIMIT GREATEST(0,4-(SELECT count(*) FROM operations WHERE action='image_check' AND state IN ('queued','running'))) FOR UPDATE OF w SKIP LOCKED`)
	if err != nil {
		return err
	}
	type candidate struct {
		id, kind      string
		configuration []byte
	}
	candidates := []candidate{}
	for rows.Next() {
		var item candidate
		if err = rows.Scan(&item.id, &item.kind, &item.configuration); err != nil {
			break
		}
		candidates = append(candidates, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range candidates {
		request := worker.Request{ID: resourceID(), Workload: item.id, Action: "image_check", Kind: item.kind}
		if item.kind == "job" {
			data, err := unseal(s.Key, item.configuration)
			var config workloadInput
			if err != nil || json.Unmarshal(data, &config) != nil {
				return errors.New("job configuration unavailable for image check")
			}
			request.JobService = config.JobService
		}
		if err := worker.ValidateRequest(request); err != nil {
			return err
		}
		payload, err := s.sealJSON(request)
		if err != nil {
			return err
		}
		inserted, err := tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'image_check',$3) ON CONFLICT DO NOTHING`, request.ID, item.id, payload)
		if err != nil {
			return err
		}
		if inserted.RowsAffected() == 0 {
			continue
		}
		if _, err = tx.Exec(ctx, `UPDATE workloads SET next_image_check_at=now()+interval '1 hour' WHERE id=$1`, item.id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status) VALUES('scheduler','image digest check',$1,202)`, item.id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
