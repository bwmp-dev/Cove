package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

func (s *Server) workloadCapacity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var node string
	var supported bool
	var configuration []byte
	err := s.DB.QueryRow(ctx, `SELECT w.node_id,w.configuration,n.state='ready' AND COALESCE((n.capabilities->>'capacity_advice')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, r.PathValue("id")).Scan(&node, &configuration, &supported)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	if !supported {
		http.Error(w, "upgrade and verify a ready worker for capacity advice", 409)
		return
	}
	plain, err := unseal(s.Key, configuration)
	var c workloadInput
	if err != nil || json.Unmarshal(plain, &c) != nil {
		http.Error(w, "resource configuration unavailable", 503)
		return
	}
	request := worker.Request{ID: resourceID(), Workload: r.PathValue("id"), Action: "deploy", Compose: c.Compose, Source: c.Source, Environment: c.Environment, Limits: c.Limits, Kind: c.Kind, JobService: c.JobService, Routes: c.Routes, Private: c.Private, Hooks: c.Hooks, FileSecrets: c.FileSecrets, GuardedDeploy: c.GuardedDeploy}
	if err = s.prepareInfrastructure(ctx, &request); err != nil {
		http.Error(w, "check resource variables and worker capabilities", 409)
		return
	}
	if err = s.prepareAppDeployment(ctx, &request); err != nil {
		http.Error(w, "repository access unavailable", 409)
		return
	}
	response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "capacity", Request: &request})
	if err != nil || response.Preflight == nil || response.Error != "" {
		http.Error(w, "capacity inspection unavailable", 502)
		return
	}
	respond(w, 200, response.Preflight)
}

type sizingAdvice struct {
	Service           string    `json:"service"`
	Samples           int       `json:"samples"`
	Hours             int       `json:"hours"`
	FirstSample       time.Time `json:"first_sample"`
	LastSample        time.Time `json:"last_sample"`
	CPUP95            float64   `json:"cpu_p95_percent"`
	MemoryP95         float64   `json:"memory_p95_bytes"`
	SuggestedCPUs     *float64  `json:"suggested_cpus"`
	SuggestedMemoryMB *int64    `json:"suggested_memory_mb"`
	Notice            string    `json:"notice"`
}

func suggestSizing(c *sizingAdvice) {
	if c.Hours < 20 {
		c.Notice = "At least 20 distinct hours of samples are needed."
		return
	}
	if time.Since(c.LastSample) > 2*time.Hour {
		c.Notice = "Recent measurements are unavailable; historical usage may no longer apply."
		return
	}
	cpu := max(0.1, math.Ceil(c.CPUP95/100*1.25*10)/10)
	memoryValue := max(16, math.Ceil(c.MemoryP95*1.25/float64(16<<20))*16)
	if cpu > 1024 || memoryValue > 16777216 || math.IsInf(memoryValue, 0) || math.IsNaN(memoryValue) {
		c.Notice = "Measured demand exceeds the supported suggestion range."
		return
	}
	memory := int64(memoryValue)
	c.SuggestedCPUs = &cpu
	c.SuggestedMemoryMB = &memory
	c.Notice = "25% headroom above observed p95 hourly samples; review burst usage before changing limits."
}

func (s *Server) workloadSizing(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), `SELECT payload->>'service',count(*),count(DISTINCT date_trunc('hour',sampled_at)),min(sampled_at),max(sampled_at),percentile_cont(0.95) WITHIN GROUP(ORDER BY (payload->'metrics'->'usage'->>'cpu_percent')::double precision),percentile_cont(0.95) WITHIN GROUP(ORDER BY (payload->'metrics'->'usage'->>'memory_used')::double precision) FROM metric_samples WHERE workload_id=$1 AND tier='hour' AND sampled_at>now()-interval '7 days' AND COALESCE(payload->>'service','')<>'' AND jsonb_typeof(payload->'metrics'->'usage'->'cpu_percent')='number' AND jsonb_typeof(payload->'metrics'->'usage'->'memory_used')='number' GROUP BY payload->>'service' ORDER BY payload->>'service' LIMIT 64`, r.PathValue("id"))
	if err != nil {
		http.Error(w, "right-sizing history unavailable", 503)
		return
	}
	defer rows.Close()
	out := []sizingAdvice{}
	for rows.Next() {
		var c sizingAdvice
		if rows.Scan(&c.Service, &c.Samples, &c.Hours, &c.FirstSample, &c.LastSample, &c.CPUP95, &c.MemoryP95) != nil {
			http.Error(w, "right-sizing history unavailable", 503)
			return
		}
		if math.IsNaN(c.CPUP95) || math.IsInf(c.CPUP95, 0) || c.CPUP95 < 0 || math.IsNaN(c.MemoryP95) || math.IsInf(c.MemoryP95, 0) || c.MemoryP95 < 0 {
			continue
		}
		suggestSizing(&c)
		out = append(out, c)
	}
	if rows.Err() != nil {
		http.Error(w, "right-sizing history unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"window": "7 days", "sampling": "Latest sample per hour and container, grouped by service; no automatic limit changes.", "services": out})
}

func (s *Server) workloadLiveLogs(w http.ResponseWriter, r *http.Request) {
	var node string
	var supported bool
	err := s.DB.QueryRow(r.Context(), `SELECT w.node_id,n.state='ready' AND COALESCE((n.capabilities->>'structured_logs')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, r.PathValue("id")).Scan(&node, &supported)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	if !supported {
		http.Error(w, "upgrade and verify a ready worker for merged live logs", 409)
		return
	}
	since := time.Time{}
	if value := r.URL.Query().Get("since"); value != "" {
		since, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			http.Error(w, "use an RFC3339 log cursor", 400)
			return
		}
	}
	response, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "live_logs", ID: r.PathValue("id"), Telemetry: &worker.TelemetryRequest{Since: since}})
	if err != nil || response.Error != "" || response.Telemetry == nil {
		http.Error(w, "live logs unavailable; worker may be busy or unreachable", 502)
		return
	}
	sample := response.Telemetry
	if len(sample.Logs) > 2000 {
		http.Error(w, "worker live log response exceeds limits", 502)
		return
	}
	type line struct {
		ID        string    `json:"id"`
		Container string    `json:"container"`
		Timestamp time.Time `json:"timestamp"`
		Message   string    `json:"message"`
		Clipped   bool      `json:"clipped"`
	}
	out := []line{}
	occurrences := map[string]int{}
	for _, entry := range sample.Logs {
		if len(entry.Message) > 8192 || len(entry.Container) > 256 || entry.Timestamp.IsZero() {
			http.Error(w, "invalid worker live log response", 502)
			return
		}
		key := entry.ContainerID + "\x00" + entry.Timestamp.Format(time.RFC3339Nano) + "\x00" + entry.Message
		occurrences[key]++
		hash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", r.PathValue("id"), key, occurrences[key])))
		out = append(out, line{ID: hex.EncodeToString(hash[:]), Container: entry.Container, Timestamp: entry.Timestamp, Message: entry.Message, Clipped: entry.Clipped})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	respond(w, 200, map[string]any{"entries": out, "sampled_at": sample.SampledAt, "limited": sample.LogsLimited})
}
