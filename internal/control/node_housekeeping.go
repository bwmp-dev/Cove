package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

func diskThresholdValue(sample *worker.NodeMetrics, path string, threshold float64) (float64, bool) {
	if path == "" {
		if sample.DiskTotal == 0 || sample.DiskAvailable > sample.DiskTotal {
			return 0, false
		}
		return 100 * (1 - float64(sample.DiskAvailable)/float64(sample.DiskTotal)), true
	}
	worst, found, unknown := float64(0), false, sample.MountsLimited && path == "*"
	for _, m := range sample.Mounts {
		if path != "*" && m.Path != path {
			continue
		}
		if m.Error != "" {
			unknown = true
		}
		if !m.Known || m.Total == 0 || m.Available > m.Total {
			continue
		}
		found = true
		value := 100 * (1 - float64(m.Available)/float64(m.Total))
		worst = max(worst, value)
	}
	return worst, found && (!unknown || worst >= threshold)
}

func (s *Server) notificationMounts(w http.ResponseWriter, r *http.Request) {
	var data []byte
	err := s.DB.QueryRow(r.Context(), `SELECT n.capabilities FROM nodes n JOIN team_nodes t ON t.node_id=n.id WHERE n.id=$1 AND t.team_id=$2`, r.URL.Query().Get("node_id"), actor(r).Team).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "node metrics unavailable", 503)
		return
	}
	var p worker.Probe
	if json.Unmarshal(data, &p) != nil || p.Metrics == nil {
		http.Error(w, "node metrics unavailable; upgrade and verify the worker", 409)
		return
	}
	respond(w, 200, map[string]any{"mounts": p.Metrics.Mounts, "limited": p.Metrics.MountsLimited, "sampled_at": p.Metrics.SampledAt})
}

func (s *Server) nodeCleanupControl(w http.ResponseWriter, r *http.Request) {
	var supported bool
	err := s.DB.QueryRow(r.Context(), `SELECT state='ready' AND COALESCE((capabilities->>'scheduled_cleanup')::boolean,false) FROM nodes WHERE id=$1`, r.PathValue("id")).Scan(&supported)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "node unavailable", 503)
		return
	}
	if !supported {
		http.Error(w, "upgrade and verify a ready worker for cleanup policies", 409)
		return
	}
	in := worker.CleanupControl{Action: "get"}
	switch {
	case r.Method == http.MethodPut:
		var policy worker.CleanupPolicy
		if !decode(w, r, &policy) {
			return
		}
		if policy.IntervalHours < 1 || policy.IntervalHours > 168 || policy.MinimumAgeHours < 24 || policy.MinimumAgeHours > 8760 {
			http.Error(w, "cleanup requires interval 1-168 hours and minimum age 24-8760 hours", 400)
			return
		}
		in.Action = "save"
		in.Policy = &policy
	case strings.HasSuffix(r.URL.Path, "/cleanup-plan"):
		in.Action = "preview"
	case strings.HasSuffix(r.URL.Path, "/cleanup-run"):
		var body struct {
			Revision string `json:"revision"`
		}
		if !decode(w, r, &body) {
			return
		}
		if len(body.Revision) != 64 {
			http.Error(w, "refresh cleanup preview before applying", 400)
			return
		}
		in.Action = "run"
		in.Revision = body.Revision
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Minute))
	response, err := s.nodeRPC(ctx, r.PathValue("id"), worker.RPCRequest{Method: "cleanup_control", CleanupControl: &in})
	if err != nil {
		http.Error(w, "worker cleanup unavailable", 502)
		return
	}
	if response.Error != "" {
		http.Error(w, "cleanup could not be confirmed; worker may be busy or preview changed; refresh policy and preview", 409)
		return
	}
	respond(w, 200, map[string]any{"state": response.CleanupState, "plan": response.CleanupPlan})
}

func (s *Server) operationRetentionStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method == http.MethodPost {
		if _, err := s.DB.Exec(ctx, `UPDATE operation_retention_runs SET checked_at=now()-interval '6 minutes'`); err != nil {
			http.Error(w, "retention unavailable", 503)
			return
		}
		var config telemetryRetention
		if err := s.DB.QueryRow(ctx, `SELECT configuration FROM telemetry_settings WHERE id=1`).Scan(&config); err != nil {
			http.Error(w, "retention unavailable", 503)
			return
		}
		if config.OperationDays == 0 {
			config.OperationDays = 90
		}
		if config.OperationRows == 0 {
			config.OperationRows = 100000
		}
		if err := s.pruneOperationHistory(ctx, config.OperationDays, config.OperationRows); err != nil {
			http.Error(w, "retention pass unavailable; inspect worker status", 503)
			return
		}
	}
	rows, err := s.DB.Query(ctx, `SELECT n.id,n.name,r.checked_at,COALESCE(r.error,''),COALESCE(r.pruned,0),COALESCE(r.protected,0) FROM nodes n LEFT JOIN operation_retention_runs r ON r.node_id=n.id ORDER BY n.name,n.id LIMIT 10000`)
	if err != nil {
		http.Error(w, "retention status unavailable", 503)
		return
	}
	defer rows.Close()
	type item struct {
		NodeID    string     `json:"node_id"`
		Name      string     `json:"name"`
		CheckedAt *time.Time `json:"checked_at"`
		Error     string     `json:"error"`
		Pruned    int        `json:"pruned"`
		Protected int        `json:"protected"`
	}
	out := []item{}
	for rows.Next() {
		var i item
		if rows.Scan(&i.NodeID, &i.Name, &i.CheckedAt, &i.Error, &i.Pruned, &i.Protected) != nil {
			http.Error(w, "retention status unavailable", 503)
			return
		}
		out = append(out, i)
	}
	if rows.Err() != nil {
		http.Error(w, "retention status unavailable", 503)
		return
	}
	respond(w, 200, out)
}
