package control

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"cove/internal/worker"
)

var prometheusSlots = make(chan struct{}, 1)

func (s *Server) metricsToken(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		var configured bool
		if err := s.DB.QueryRow(r.Context(), `SELECT metrics_token_hash IS NOT NULL AND metrics_token_creator_id IS NOT NULL FROM control_settings WHERE id=1`).Scan(&configured); err != nil {
			http.Error(w, "metrics settings unavailable", 503)
			return
		}
		respond(w, 200, map[string]bool{"configured": configured})
		return
	}
	var hash []byte
	token := ""
	if r.Method == http.MethodPost {
		token = randomToken()
		hash = tokenHash(token)
	}
	if !s.requirePermission(w, r, "team", "", "read_secrets") {
		return
	}
	if _, err := s.DB.Exec(r.Context(), `UPDATE control_settings SET metrics_token_hash=$1,metrics_token_creator_id=(SELECT id FROM users WHERE id=$2) WHERE id=1`, hash, actor(r).ID); err != nil {
		http.Error(w, "could not update metrics token", 503)
		return
	}
	if token != "" {
		respond(w, 200, map[string]string{"token": token})
		return
	}
	respond(w, 200, map[string]bool{"configured": false})
}

func prometheusLabels(values ...string) string {
	escape := strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"")
	parts := make([]string, 0, len(values)/2)
	for i := 0; i+1 < len(values); i += 2 {
		parts = append(parts, values[i]+"=\""+escape.Replace(values[i+1])+"\"")
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func (s *Server) prometheus(w http.ResponseWriter, r *http.Request) {
	if bearer(r) == "" || len(bearer(r)) > 256 {
		http.Error(w, "metrics token required", 401)
		return
	}
	var hash []byte
	var creator string
	if err := s.DB.QueryRow(r.Context(), `SELECT metrics_token_hash,COALESCE(metrics_token_creator_id,'') FROM control_settings WHERE id=1`).Scan(&hash, &creator); err != nil {
		http.Error(w, "metrics authentication unavailable", 503)
		return
	}
	if creator == "" || len(hash) != 32 || subtle.ConstantTimeCompare(hash, tokenHash(bearer(r))) != 1 {
		http.Error(w, "invalid metrics token", 401)
		return
	}
	p, err := s.identity(r, creator)
	if err != nil || !p.Admin {
		http.Error(w, "metrics token creator is unavailable", 401)
		return
	}
	// A global scrape includes teams beyond the issuer's selected browser team.
	if err = s.DB.QueryRow(r.Context(), `SELECT COALESCE((SELECT id FROM teams WHERE require_second_factor ORDER BY id LIMIT 1),'')`).Scan(&p.Team); err != nil {
		http.Error(w, "metrics policy unavailable", 503)
		return
	}
	checked := r.Clone(context.WithValue(r.Context(), identityKey{}, p))
	checked.URL.Path = "/api/search"
	if !s.authorize(w, checked) {
		return
	}
	select {
	case prometheusSlots <- struct{}{}:
		defer func() { <-prometheusSlots }()
	default:
		http.Error(w, "another scrape is running", 503)
		return
	}
	var b bytes.Buffer
	exportLimited := false
	emit := func(name, labels string, value float64) {
		if !math.IsNaN(value) && !math.IsInf(value, 0) {
			line := fmt.Sprintf("cove_%s%s %s\n", name, labels, strconv.FormatFloat(value, 'g', -1, 64))
			if b.Len()+len(line) > 8*1024*1024 {
				exportLimited = true
				return
			}
			b.WriteString(line)
		}
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	emit("panel_goroutines", "", float64(runtime.NumGoroutine()))
	emit("panel_heap_bytes", "", float64(memory.HeapAlloc))
	rows, err := s.DB.Query(r.Context(), `SELECT state,count(*) FROM operations GROUP BY state`)
	if err != nil {
		http.Error(w, "panel metrics unavailable", 503)
		return
	}
	for rows.Next() {
		var state string
		var count int64
		if err = rows.Scan(&state, &count); err != nil {
			break
		}
		emit("panel_operation_records", prometheusLabels("state", state), float64(count))
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		http.Error(w, "panel metrics unavailable", 503)
		return
	}
	rows, err = s.DB.Query(r.Context(), `SELECT node_id,COALESCE(workload_id,''),series,sampled_at,payload FROM (SELECT DISTINCT ON(node_id,COALESCE(workload_id,''),series) node_id,workload_id,series,sampled_at,payload FROM metric_samples WHERE tier='minute' AND sampled_at>now()-interval '5 minutes' AND sampled_at<=now()+interval '1 minute' ORDER BY node_id,COALESCE(workload_id,''),series,sampled_at DESC) x ORDER BY node_id,workload_id,series LIMIT 10001`)
	if err != nil {
		http.Error(w, "metric samples unavailable", 503)
		return
	}
	count := 0
	for rows.Next() {
		var node, workload, series string
		var sampled time.Time
		var data []byte
		if err = rows.Scan(&node, &workload, &series, &sampled, &data); err != nil {
			break
		}
		count++
		if count > 10000 {
			continue
		}
		labels := prometheusLabels("node", node, "workload", workload, "series", series)
		emit("sample_timestamp_seconds", labels, float64(sampled.Unix()))
		if workload == "" && series == "" {
			var n worker.NodeMetrics
			if json.Unmarshal(data, &n) != nil {
				continue
			}
			emit("node_cpu_percent", labels, n.CPUPercent)
			emit("node_memory_total_bytes", labels, float64(n.MemoryTotal))
			emit("node_memory_available_bytes", labels, float64(n.MemoryAvailable))
			emit("node_disk_total_bytes", labels, float64(n.DiskTotal))
			emit("node_disk_available_bytes", labels, float64(n.DiskAvailable))
			if n.NetworkRX != nil {
				emit("node_network_received_bytes_total", labels, float64(*n.NetworkRX))
			}
			if n.NetworkTX != nil {
				emit("node_network_sent_bytes_total", labels, float64(*n.NetworkTX))
			}
		} else if workload == "" && strings.HasPrefix(series, "mount:") {
			var mount worker.MountMetric
			if json.Unmarshal(data, &mount) != nil || !mount.Known {
				continue
			}
			labels = prometheusLabels("node", node, "mount", mount.Path)
			emit("node_mount_total_bytes", labels, float64(mount.Total))
			emit("node_mount_available_bytes", labels, float64(mount.Available))
		} else if workload != "" {
			var c worker.Container
			if json.Unmarshal(data, &c) != nil || c.Metrics == nil || c.Metrics.Usage == nil || c.MetricsUnavailable {
				continue
			}
			u := c.Metrics.Usage
			emit("container_cpu_percent", labels, u.CPUPercent)
			emit("container_memory_used_bytes", labels, u.MemoryUsed)
			emit("container_memory_limit_bytes", labels, u.MemoryLimit)
			emit("container_network_received_bytes_total", labels, u.NetworkRX)
			emit("container_network_sent_bytes_total", labels, u.NetworkTX)
			emit("container_block_read_bytes_total", labels, u.BlockRead)
			emit("container_block_written_bytes_total", labels, u.BlockWritten)
		}
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		http.Error(w, "metric samples unavailable", 503)
		return
	}
	truncated := 0.0
	if count > 10000 || exportLimited {
		truncated = 1
	}
	fmt.Fprintf(&b, "cove_export_truncated %g\n", truncated)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write(b.Bytes())
}
