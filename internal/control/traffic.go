package control

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

func (s *Server) nodeTraffic(w http.ResponseWriter, r *http.Request) {
	var supported bool
	err := s.DB.QueryRow(r.Context(), `SELECT state='ready' AND COALESCE((capabilities->>'traffic_logs')::boolean,false) FROM nodes WHERE id=$1`, r.PathValue("id")).Scan(&supported)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "node unavailable", 503)
		return
	}
	if !supported {
		http.Error(w, "upgrade and verify a ready Linux worker for traffic collection", 409)
		return
	}
	control := worker.ProxyTrafficControl{Action: "get"}
	if r.Method == "POST" {
		if !decode(w, r, &control) {
			return
		}
		if control.Action != "preview" && control.Action != "apply" && control.Action != "recover" {
			http.Error(w, "select preview, apply or recover", 400)
			return
		}
	}
	response, err := s.nodeRPC(r.Context(), r.PathValue("id"), worker.RPCRequest{Method: "traffic_control", TrafficControl: &control})
	if err != nil {
		http.Error(w, "proxy traffic settings unavailable", 502)
		return
	}
	if response.Error != "" {
		message := "Proxy change unavailable; refresh its state and inspect worker readiness."
		switch response.Error {
		case "managed proxy unavailable":
			message = "Managed proxy unavailable; deploy a public route first, or repair the existing proxy."
		case "wait for worker operations and proxy recovery", "wait for active worker operations before proxy recovery":
			message = "Wait for active worker operations to finish before changing or recovering the proxy."
		case "proxy preview changed; refresh before applying":
			message = "The proxy changed after the preview; review a fresh preview before applying."
		case "proxy reconfiguration requires the worker's local Unix Docker socket":
			message = "Proxy reconfiguration requires the worker's local Unix Docker socket; remote Docker contexts require manual migration."
		case "existing proxy is not managed by Cove":
			message = "The existing proxy is not managed by Cove; automatic reconfiguration is refused."
		case "traffic logging change failed; the previous proxy was restored", "replacement proxy failed its final check; the previous settings were restored":
			message = "The replacement failed readiness checks; the previous proxy and settings were restored."
		case "disable insecure forwarded-header and proxy-protocol trust before configuring traffic":
			message = "Disable insecure forwarded-header or proxy-protocol trust before configuring traffic logging."
		}
		if response.TrafficState != nil && response.TrafficState.Pending {
			message = "An interrupted proxy change needs recovery; refresh the state and use Recover before another change."
		}
		http.Error(w, message, 409)
		return
	}
	if response.TrafficState == nil {
		http.Error(w, "proxy traffic settings unavailable", 502)
		return
	}
	respond(w, 200, map[string]any{"state": response.TrafficState, "plan": response.TrafficPlan})
}

func (s *Server) collectTraffic(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT n.id,COALESCE(t.cursor_at,now()-interval '1 minute') FROM nodes n LEFT JOIN traffic_collection t ON t.node_id=n.id WHERE n.state='ready' AND COALESCE((n.capabilities->>'traffic_logs')::boolean,false) AND (t.checked_at IS NULL OR t.checked_at<now()-interval '1 minute') ORDER BY t.checked_at NULLS FIRST,n.id LIMIT 8`)
	if err != nil {
		return err
	}
	type node struct {
		id    string
		since time.Time
	}
	var nodes []node
	for rows.Next() {
		var n node
		if err = rows.Scan(&n.id, &n.since); err != nil {
			break
		}
		nodes = append(nodes, n)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, n := range nodes {
		response, rpcErr := s.nodeRPC(ctx, n.id, worker.RPCRequest{Method: "traffic", Telemetry: &worker.TelemetryRequest{Since: n.since.Add(-time.Second)}})
		if rpcErr != nil || response.Error != "" || response.Traffic == nil {
			if _, err = s.DB.Exec(ctx, `INSERT INTO traffic_collection(node_id,checked_at,last_error) VALUES($1,now(),'Traffic collection unavailable; verify proxy settings and worker connectivity.') ON CONFLICT(node_id) DO UPDATE SET checked_at=now(),last_error=EXCLUDED.last_error`, n.id); err != nil {
				return err
			}
			continue
		}
		if err = s.storeTraffic(ctx, n.id, response.Traffic); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) runTrafficCollection(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := s.collectTraffic(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Server) storeTraffic(ctx context.Context, node string, report *worker.TrafficReport) error {
	if report == nil || report.SampledAt.IsZero() || report.SampledAt.After(time.Now().Add(time.Minute)) || len(report.Entries) > 2000 {
		return errors.New("invalid traffic report")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1129272912)`); err != nil {
		return err
	}
	for _, e := range report.Entries {
		if len(e.ID) != 64 || len(e.Domain) > 253 || len(e.ClientIP) > 45 || len(e.Workload) > 128 || e.Status < 100 || e.Status > 599 || e.Bytes < 0 || e.DurationMS < 0 || math.IsNaN(e.DurationMS) || math.IsInf(e.DurationMS, 0) || e.Timestamp.After(report.SampledAt.Add(time.Minute)) || e.Timestamp.Before(time.Now().Add(-24*time.Hour)) {
			return errors.New("invalid traffic entry")
		}
		_, err = tx.Exec(ctx, `INSERT INTO route_traffic(id,workload_id,node_id,domain,timestamp,status,bytes,duration_ms,client_ip) SELECT $1,w.id,$3,$4,$5,$6,$7,$8,$9 FROM workloads w WHERE w.id=$2 AND w.node_id=$3 ON CONFLICT DO NOTHING`, e.ID, e.Workload, node, e.Domain, e.Timestamp, e.Status, e.Bytes, e.DurationMS, e.ClientIP)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO traffic_collection(node_id,cursor_at,checked_at,available,limited,last_error) VALUES($1,$2,now(),$3,$4,'') ON CONFLICT(node_id) DO UPDATE SET cursor_at=EXCLUDED.cursor_at,checked_at=now(),available=EXCLUDED.available,limited=EXCLUDED.limited,last_error=''`, node, report.SampledAt, report.Available, report.Limited)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM route_traffic WHERE id IN(SELECT id FROM(SELECT id,timestamp,row_number() OVER(ORDER BY timestamp DESC,id DESC) AS n,sum(octet_length(id)+octet_length(workload_id)+octet_length(node_id)+octet_length(domain)+octet_length(client_ip)+128) OVER(ORDER BY timestamp DESC,id DESC) AS size FROM route_traffic) x WHERE timestamp<now()-interval '7 days' OR n>200000 OR size>67108864)`)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) workloadTraffic(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if value := r.URL.Query().Get("range"); value != "" {
		switch value {
		case "hour":
			hours = 1
		case "day":
			hours = 24
		case "week":
			hours = 168
		default:
			http.Error(w, "select hour, day or week", 400)
			return
		}
	}
	rows, err := s.DB.Query(r.Context(), `SELECT domain,count(*),count(*) FILTER(WHERE status<200),count(*) FILTER(WHERE status>=200 AND status<300),count(*) FILTER(WHERE status>=300 AND status<400),count(*) FILTER(WHERE status>=400 AND status<500),count(*) FILTER(WHERE status>=500),COALESCE(sum(bytes),0)::bigint,percentile_cont(0.95) WITHIN GROUP(ORDER BY duration_ms) FROM route_traffic WHERE workload_id=$1 AND timestamp>now()-make_interval(hours=>$2) GROUP BY domain ORDER BY domain LIMIT 129`, r.PathValue("id"), hours)
	if err != nil {
		http.Error(w, "traffic history unavailable", 503)
		return
	}
	type route struct {
		Domain        string  `json:"domain"`
		Requests      int64   `json:"requests"`
		Informational int64   `json:"status_1xx"`
		Success       int64   `json:"status_2xx"`
		Redirect      int64   `json:"status_3xx"`
		ClientError   int64   `json:"status_4xx"`
		ServerError   int64   `json:"status_5xx"`
		Bytes         int64   `json:"bytes"`
		P95           float64 `json:"p95_ms"`
	}
	out := []route{}
	for rows.Next() {
		var x route
		if err = rows.Scan(&x.Domain, &x.Requests, &x.Informational, &x.Success, &x.Redirect, &x.ClientError, &x.ServerError, &x.Bytes, &x.P95); err != nil {
			break
		}
		out = append(out, x)
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		http.Error(w, "traffic history unavailable", 503)
		return
	}
	routesLimited := len(out) > 128
	if routesLimited {
		out = out[:128]
	}
	var collected *time.Time
	var available, limited bool
	var message string
	err = s.DB.QueryRow(r.Context(), `SELECT t.checked_at,t.available,t.limited,t.last_error FROM workloads w JOIN traffic_collection t ON t.node_id=w.node_id WHERE w.id=$1`, r.PathValue("id")).Scan(&collected, &available, &limited, &message)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "traffic collection unavailable", 503)
		return
	}
	logs, err := s.DB.Query(r.Context(), `SELECT timestamp,domain,status,bytes,duration_ms,client_ip FROM route_traffic WHERE workload_id=$1 AND timestamp>now()-make_interval(hours=>$2) ORDER BY timestamp DESC,id DESC LIMIT 100`, r.PathValue("id"), hours)
	if err != nil {
		http.Error(w, "access history unavailable", 503)
		return
	}
	defer logs.Close()
	type entry struct {
		Timestamp time.Time `json:"timestamp"`
		Domain    string    `json:"domain"`
		Status    int       `json:"status"`
		Bytes     int64     `json:"bytes"`
		Duration  float64   `json:"duration_ms"`
		ClientIP  string    `json:"client_ip"`
	}
	entries := []entry{}
	for logs.Next() {
		var e entry
		if err = logs.Scan(&e.Timestamp, &e.Domain, &e.Status, &e.Bytes, &e.Duration, &e.ClientIP); err != nil {
			break
		}
		entries = append(entries, e)
	}
	if err != nil || logs.Err() != nil {
		http.Error(w, "access history unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"routes": out, "entries": entries, "collected_at": collected, "available": available, "limited": limited || routesLimited, "collection_error": strings.TrimSpace(message), "hours": hours, "retention_days": 7, "notice": "Statistics cover retained observations only. Proxy log rotation and collection limits can leave gaps."})
}
