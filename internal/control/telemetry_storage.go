package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

type telemetryRetention struct {
	MetricRows  int   `json:"metric_rows"`
	MetricBytes int64 `json:"metric_bytes"`
	LogRows     int   `json:"log_rows"`
	LogBytes    int64 `json:"log_bytes"`
	LogDays     int   `json:"log_days"`
	AuditRows   int   `json:"audit_rows"`
	AuditDays   int   `json:"audit_days"`
	RawDays     int   `json:"raw_days"`
	HourDays    int   `json:"hour_days"`
	DayDays     int   `json:"day_days"`
}

func telemetryConfig(ctx context.Context, tx pgx.Tx) (telemetryRetention, error) {
	var config telemetryRetention
	err := tx.QueryRow(ctx, `SELECT configuration FROM telemetry_settings WHERE id=1 FOR UPDATE`).Scan(&config)
	return config, err
}

func pruneTelemetry(ctx context.Context, tx pgx.Tx, c telemetryRetention) error {
	_, err := tx.Exec(ctx, `DELETE FROM metric_samples WHERE sampled_at<now()-make_interval(days=>CASE tier WHEN 'minute' THEN $1::integer WHEN 'hour' THEN $2::integer ELSE $3::integer END)`, c.RawDays, c.HourDays, c.DayDays)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM metric_samples WHERE id IN (SELECT id FROM (SELECT id,row_number() OVER(ORDER BY sampled_at DESC,id DESC) AS n,sum(octet_length(payload::text)+256) OVER(ORDER BY sampled_at DESC,id DESC) AS bytes FROM metric_samples) p WHERE n>$1 OR bytes>$2)`, c.MetricRows, c.MetricBytes)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM workload_log_entries WHERE timestamp<now()-make_interval(days=>$1)`, c.LogDays)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM workload_log_entries WHERE id IN (SELECT id FROM (SELECT id,row_number() OVER(ORDER BY timestamp DESC,id DESC) AS n,sum(octet_length(message)+octet_length(container)+256) OVER(ORDER BY timestamp DESC,id DESC) AS bytes FROM workload_log_entries) p WHERE n>$1 OR bytes>$2)`, c.LogRows, c.LogBytes)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM audit_events WHERE created_at<now()-make_interval(days=>$1) OR id IN (SELECT id FROM audit_events ORDER BY id DESC OFFSET $2)`, c.AuditDays, c.AuditRows)
	return err
}

func saveMetric(ctx context.Context, tx pgx.Tx, node, workload, series string, stamp time.Time, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if len(data) > 32768 {
		return fmt.Errorf("metric sample exceeds storage limit")
	}
	for _, tier := range []string{"minute", "hour", "day"} {
		_, err = tx.Exec(ctx, `INSERT INTO metric_samples(node_id,workload_id,series,tier,bucket,sampled_at,payload) VALUES($1,NULLIF($2,''),$3,$4,date_trunc($4,$5::timestamptz),$5,$6) ON CONFLICT(node_id,(COALESCE(workload_id,'')),series,tier,bucket) DO UPDATE SET sampled_at=EXCLUDED.sampled_at,payload=EXCLUDED.payload WHERE metric_samples.sampled_at<=EXCLUDED.sampled_at`, node, workload, series, tier, stamp, data)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) storeNodeMetric(ctx context.Context, node string, sample *worker.NodeMetrics) error {
	if sample == nil || sample.SampledAt.IsZero() {
		return nil
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
	if err = saveMetric(ctx, tx, node, "", "", sample.SampledAt, sample); err != nil {
		return err
	}
	if err = pruneTelemetry(ctx, tx, c); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) storeWorkloadTelemetry(ctx context.Context, node, id string, result *worker.Telemetry) error {
	if result == nil || result.SampledAt.IsZero() || result.SampledAt.After(time.Now().Add(time.Minute)) || len(result.Containers) > 32 || len(result.Logs) > 2000 {
		return fmt.Errorf("invalid telemetry response")
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
	for _, container := range result.Containers {
		if container.Metrics == nil || container.MetricsUnavailable {
			continue
		}
		if err = saveMetric(ctx, tx, node, id, container.ID, result.SampledAt, container); err != nil {
			return err
		}
	}
	occurrences := map[string]int{}
	limited := result.LogsLimited
	for _, entry := range result.Logs {
		entry.Message = strings.ReplaceAll(strings.ToValidUTF8(entry.Message, ""), "\x00", "\\0")
		if entry.Timestamp.IsZero() || entry.Timestamp.After(result.SampledAt.Add(time.Minute)) || len(entry.ContainerID) > 128 || len(entry.Container) > 256 {
			return fmt.Errorf("invalid log entry")
		}
		key := entry.ContainerID + "\x00" + entry.Timestamp.Format(time.RFC3339Nano) + "\x00" + entry.Message
		occurrences[key]++
		hash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", id, key, occurrences[key])))
		clipped := len(entry.Message) > 8192
		if clipped {
			limited = true
			entry.Message = strings.ToValidUTF8(entry.Message[:8192], "")
		}
		_, err = tx.Exec(ctx, `INSERT INTO workload_log_entries(id,workload_id,container_id,container,timestamp,message,clipped) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, hex.EncodeToString(hash[:]), id, entry.ContainerID, entry.Container, entry.Timestamp, entry.Message, clipped)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE workloads SET telemetry_since=$2,telemetry_limited=$3,telemetry_error='' WHERE id=$1 AND node_id=$4`, id, result.SampledAt, limited, node)
	if err != nil {
		return err
	}
	if err = pruneTelemetry(ctx, tx, c); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) telemetryRetention(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(ctx)
	c, err := telemetryConfig(ctx, tx)
	if err != nil {
		http.Error(w, "retention unavailable", 503)
		return
	}
	if r.Method == http.MethodPut {
		if !decode(w, r, &c) {
			return
		}
		if c.MetricRows < 1000 || c.MetricRows > 1000000 || c.LogRows < 1000 || c.LogRows > 1000000 || c.MetricBytes < 1048576 || c.MetricBytes > 10737418240 || c.LogBytes < 1048576 || c.LogBytes > 10737418240 || c.AuditRows < 1000 || c.AuditRows > 1000000 || c.LogDays < 1 || c.LogDays > 365 || c.AuditDays < 1 || c.AuditDays > 3650 || c.RawDays < 1 || c.RawDays > 7 || c.HourDays < c.RawDays || c.HourDays > 90 || c.DayDays < c.HourDays || c.DayDays > 3650 {
			http.Error(w, "invalid retention limits", 400)
			return
		}
		data, _ := json.Marshal(c)
		if _, err = tx.Exec(ctx, `UPDATE telemetry_settings SET configuration=$1 WHERE id=1`, data); err == nil {
			err = pruneTelemetry(ctx, tx, c)
		}
		if err != nil {
			http.Error(w, "could not update retention", 503)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		http.Error(w, "retention unavailable", 503)
		return
	}
	respond(w, 200, c)
}

func (s *Server) telemetryStorageUsage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	type store struct {
		Rows  int64  `json:"rows"`
		Bytes int64  `json:"bytes"`
		Basis string `json:"basis"`
	}
	out := map[string]store{}
	for _, item := range []struct{ name, query string }{
		{"metrics", `SELECT count(*),COALESCE(sum(octet_length(payload::text)+256),0) FROM metric_samples`},
		{"logs", `SELECT count(*),COALESCE(sum(octet_length(message)+octet_length(container)+256),0) FROM workload_log_entries`},
		{"audit", `SELECT count(*),pg_total_relation_size('audit_events') FROM audit_events`},
		{"operations", `SELECT count(*),pg_total_relation_size('operations') FROM operations`},
	} {
		var value store
		if err := s.DB.QueryRow(ctx, item.query).Scan(&value.Rows, &value.Bytes); err != nil {
			http.Error(w, "storage usage unavailable", 503)
			return
		}
		value.Basis = "accounted payload and row allowance"
		if item.name == "audit" || item.name == "operations" {
			value.Basis = "PostgreSQL table and indexes"
		}
		out[item.name] = value
	}
	respond(w, 200, out)
}
