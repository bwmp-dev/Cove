package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"cove/internal/worker"
)

func (s *Server) collectTelemetry(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT w.id,w.node_id,COALESCE(w.telemetry_since,now()-interval '1 minute') FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE n.state='ready' AND COALESCE((n.capabilities->>'telemetry')::boolean,false) AND (w.telemetry_checked_at IS NULL OR w.telemetry_checked_at<now()-interval '1 minute') AND EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.action IN ('deploy','rollback') AND o.state='succeeded') ORDER BY w.telemetry_checked_at NULLS FIRST LIMIT 8`)
	if err != nil {
		return err
	}
	type item struct {
		id, node string
		since    time.Time
	}
	var items []item
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.id, &i.node, &i.since); err != nil {
			break
		}
		items = append(items, i)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	failures := make(chan error, len(items))
	semaphore := make(chan struct{}, 4)
	var group sync.WaitGroup
	for _, i := range items {
		group.Go(func() {
			var err error
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			if _, err = s.DB.Exec(ctx, `UPDATE workloads SET telemetry_checked_at=now() WHERE id=$1`, i.id); err != nil {
				failures <- err
				return
			}
			response, rpcErr := s.nodeRPC(ctx, i.node, worker.RPCRequest{Method: "telemetry", ID: i.id, Telemetry: &worker.TelemetryRequest{Since: i.since.Add(-time.Second)}})
			if rpcErr != nil || response.Error != "" || response.Telemetry == nil {
				if _, err = s.DB.Exec(ctx, `UPDATE workloads SET telemetry_error='Telemetry collection unavailable; check worker connectivity and deployment state.' WHERE id=$1`, i.id); err != nil {
					failures <- err
					return
				}
				return
			}
			if err = s.storeWorkloadTelemetry(ctx, i.node, i.id, response.Telemetry); err != nil {
				failures <- err
				return
			}
		})
	}
	group.Wait()
	close(failures)
	for failure := range failures {
		return failure
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	c, err := telemetryConfig(ctx, tx)
	if err != nil {
		return err
	}
	if err = pruneTelemetry(ctx, tx, c); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) metricHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workload := ""
	node := r.PathValue("id")
	if strings.HasPrefix(r.URL.Path, "/api/nodes/") {
		var exists bool
		if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM nodes WHERE id=$1)`, node).Scan(&exists); err != nil {
			http.Error(w, "node unavailable", 503)
			return
		}
		if !exists {
			http.NotFound(w, r)
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/workloads/") {
		workload = node
		if err := s.DB.QueryRow(ctx, `SELECT node_id FROM workloads WHERE id=$1`, workload).Scan(&node); err != nil {
			http.Error(w, "resource unavailable", 503)
			return
		}
	}
	tier, window := "minute", time.Hour
	switch r.URL.Query().Get("range") {
	case "", "hour":
	case "day":
		window = 24 * time.Hour
	case "week":
		tier = "hour"
		window = 7 * 24 * time.Hour
	case "month":
		tier = "hour"
		window = 30 * 24 * time.Hour
	case "year":
		tier = "day"
		window = 365 * 24 * time.Hour
	default:
		http.Error(w, "select hour, day, week, month or year", 400)
		return
	}
	rows, err := s.DB.Query(ctx, `SELECT series,sampled_at,payload FROM metric_samples WHERE ($2<>'' OR node_id=$1) AND COALESCE(workload_id,'')=$2 AND tier=$3 AND sampled_at>=$4 ORDER BY sampled_at DESC,series LIMIT 24000`, node, workload, tier, time.Now().Add(-window))
	if err != nil {
		http.Error(w, "metrics unavailable", 503)
		return
	}
	defer rows.Close()
	type point struct {
		Series    string          `json:"series"`
		SampledAt time.Time       `json:"sampled_at"`
		Sample    json.RawMessage `json:"sample"`
	}
	out := struct {
		Tier    string           `json:"tier"`
		Points  []point          `json:"points"`
		Markers []map[string]any `json:"deploy_markers"`
		Limited bool             `json:"limited"`
	}{Tier: tier, Points: []point{}, Markers: []map[string]any{}}
	for rows.Next() {
		var p point
		if err = rows.Scan(&p.Series, &p.SampledAt, &p.Sample); err != nil {
			http.Error(w, "metrics unavailable", 503)
			return
		}
		out.Points = append(out.Points, p)
	}
	if rows.Err() != nil {
		http.Error(w, "metrics unavailable", 503)
		return
	}
	rows.Close()
	out.Limited = len(out.Points) == 24000
	if workload != "" {
		markers, err := s.DB.Query(ctx, `SELECT id,action,state,created_at FROM operations WHERE workload_id=$1 AND action IN ('deploy','rollback') AND created_at>=$2 ORDER BY created_at DESC LIMIT 1000`, workload, time.Now().Add(-window))
		if err != nil {
			http.Error(w, "deploy markers unavailable", 503)
			return
		}
		defer markers.Close()
		for markers.Next() {
			var id, action, state string
			var stamp time.Time
			if err = markers.Scan(&id, &action, &state, &stamp); err != nil {
				http.Error(w, "deploy markers unavailable", 503)
				return
			}
			out.Markers = append(out.Markers, map[string]any{"operation_id": id, "action": action, "state": state, "timestamp": stamp})
		}
		if markers.Err() != nil {
			http.Error(w, "deploy markers unavailable", 503)
			return
		}
	}
	respond(w, 200, out)
}

func (s *Server) logHistory(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	query, container := r.URL.Query().Get("q"), r.URL.Query().Get("container")
	if len(query) > 200 || len(container) > 128 {
		http.Error(w, "invalid log filter", 400)
		return
	}
	before := time.Now().Add(time.Minute)
	beforeID := r.URL.Query().Get("before_id")
	if beforeID == "" {
		beforeID = "~"
	}
	if len(beforeID) > 64 {
		http.Error(w, "invalid log cursor", 400)
		return
	}
	if value := r.URL.Query().Get("before"); value != "" {
		var err error
		before, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			http.Error(w, "invalid log timestamp", 400)
			return
		}
	}
	rows, err := s.DB.Query(ctx, `SELECT id,container_id,container,timestamp,message,clipped FROM workload_log_entries WHERE workload_id=$1 AND (timestamp,id)<($2,$5) AND ($3='' OR strpos(lower(message),lower($3))>0) AND ($4='' OR container_id=$4) ORDER BY timestamp DESC,id DESC LIMIT 500`, id, before, query, container, beforeID)
	if err != nil {
		http.Error(w, "log history unavailable", 503)
		return
	}
	defer rows.Close()
	type entry struct {
		ID          string    `json:"id"`
		ContainerID string    `json:"container_id"`
		Container   string    `json:"container"`
		Timestamp   time.Time `json:"timestamp"`
		Message     string    `json:"message"`
		Clipped     bool      `json:"clipped"`
	}
	entries := []entry{}
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.ID, &e.ContainerID, &e.Container, &e.Timestamp, &e.Message, &e.Clipped); err != nil {
			http.Error(w, "log history unavailable", 503)
			return
		}
		entries = append(entries, e)
	}
	if rows.Err() != nil {
		http.Error(w, "log history unavailable", 503)
		return
	}
	rows.Close()
	if r.URL.Query().Get("download") == "1" {
		s.recordAudit(r, actor(r).ID, 200)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="cove-logs.txt"`)
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			if _, err = fmt.Fprintf(w, "%s [%s] %s\n", e.Timestamp.Format(time.RFC3339Nano), e.Container, e.Message); err != nil {
				return
			}
		}
		return
	}
	var collected *time.Time
	var limited bool
	var collectionError string
	if err = s.DB.QueryRow(ctx, `SELECT telemetry_since,telemetry_limited,telemetry_error FROM workloads WHERE id=$1`, id).Scan(&collected, &limited, &collectionError); err != nil {
		http.Error(w, "collection status unavailable", 503)
		return
	}
	var next any
	if len(entries) == 500 {
		last := entries[len(entries)-1]
		next = map[string]any{"before": last.Timestamp, "before_id": last.ID}
	}
	respond(w, 200, map[string]any{"entries": entries, "collected_at": collected, "limited": limited, "limit": 500, "collection_error": collectionError, "next_cursor": next})
}
