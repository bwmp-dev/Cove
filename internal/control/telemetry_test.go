package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cove/internal/worker"
)

func TestTelemetryCollectorBoundsAndFailureStatus(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('collector-node','Node','localhost',22,'',$1,'ready','{"telemetry":true}')`, []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('collector-project','Project','default')")
	sealed, _ := seal(f.s.Key, []byte(`{}`))
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) SELECT 'collector-'||i,'collector-node','Resource',$1,'collector-project','default' FROM generate_series(1,9) i`, sealed)
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) SELECT 'collector-op-'||i,'collector-'||i,'deploy','succeeded',$1 FROM generate_series(1,9) i`, sealed)
	var active, maxActive, calls atomic.Int32
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maxActive.Load()
			if current <= old || maxActive.CompareAndSwap(old, current) {
				break
			}
		}
		calls.Add(1)
		if r.Method != "telemetry" || r.Telemetry == nil {
			t.Error("invalid telemetry RPC")
		}
		return worker.RPCResponse{}, errors.New("private transport details")
	}
	if err := f.s.collectTelemetry(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 8 || maxActive.Load() > 4 {
		t.Fatal("collector exceeded pass or concurrency limits", calls.Load(), maxActive.Load())
	}
	var errors int
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM workloads WHERE telemetry_error<>'' AND telemetry_error NOT LIKE '%private%'`).Scan(&errors); err != nil || errors != 8 {
		t.Fatal("collection failure not safely exposed", errors, err)
	}
	if err := f.s.collectTelemetry(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 9 {
		t.Fatal("resources were retried before next interval", calls.Load())
	}
}

func TestTelemetryHistoryRetentionAndIsolation(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('telemetry-node','Node','localhost',22,'',$1)", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('telemetry-project','Project','default')")
	sealed, _ := seal(f.s.Key, []byte(`{}`))
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('telemetry-resource','telemetry-node','Resource',$1,'telemetry-project','default')", sealed)
	stamp := time.Now().UTC()
	node := &worker.NodeMetrics{SampledAt: stamp, CPUPercent: 20, MemoryTotal: 100, MemoryAvailable: 30}
	if err := f.s.storeNodeMetric(ctx, "telemetry-node", node); err != nil {
		t.Fatal(err)
	}
	node.CPUPercent = 40
	if err := f.s.storeNodeMetric(ctx, "telemetry-node", node); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM metric_samples`).Scan(&count); err != nil || count != 3 {
		t.Fatal("tier upsert failed", count, err)
	}
	result := &worker.Telemetry{SampledAt: stamp, Containers: []worker.Container{{ID: "container", Name: "app", Metrics: &worker.ContainerMetrics{CPU: "10%", Usage: &worker.ContainerUsage{CPUPercent: 10}}}}, Logs: []worker.LogEntry{{ContainerID: "container", Container: "app", Timestamp: stamp, Message: "first message"}, {ContainerID: "container", Container: "app", Timestamp: stamp, Message: "first message"}}}
	for i := 0; i < 2; i++ {
		if err := f.s.storeWorkloadTelemetry(ctx, "telemetry-node", "telemetry-resource", result); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM workload_log_entries`).Scan(&count); err != nil || count != 2 {
		t.Fatal("log replay or identical-line loss", count, err)
	}
	var history struct {
		Points []json.RawMessage `json:"points"`
	}
	if err := json.Unmarshal(f.call("root", "default", "GET", "/api/workloads/telemetry-resource/metrics", nil, 200).Body.Bytes(), &history); err != nil || len(history.Points) != 1 {
		t.Fatal(history, err)
	}
	body := f.call("root", "default", "GET", "/api/workloads/telemetry-resource/log-history?q=FIRST&container=container", nil, 200).Body.String()
	if !strings.Contains(body, "first message") {
		t.Fatal(body)
	}
	if strings.Contains(f.call("root", "default", "GET", "/api/workloads/telemetry-resource/log-history?q=unmatched", nil, 200).Body.String(), "first message") {
		t.Fatal("search ignored")
	}
	download := f.call("root", "default", "GET", "/api/workloads/telemetry-resource/log-history?download=1", nil, 200)
	if download.Header().Get("Content-Disposition") == "" || !strings.Contains(download.Body.String(), "[app] first message") {
		t.Fatal("download unavailable")
	}
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='GET /api/workloads/{id}/log-history' AND subject='telemetry-resource' AND status=200`).Scan(&count); err != nil || count != 1 {
		t.Fatal("download not audited", count, err)
	}
	f.sql(`INSERT INTO workload_log_entries(id,workload_id,container_id,container,timestamp,message) SELECT 'page-'||i,'telemetry-resource','container','app',now(),'page message' FROM generate_series(1,501) i`)
	var page struct {
		Entries []json.RawMessage `json:"entries"`
		Next    struct {
			Before string `json:"before"`
			ID     string `json:"before_id"`
		} `json:"next_cursor"`
	}
	if err := json.Unmarshal(f.call("root", "default", "GET", "/api/workloads/telemetry-resource/log-history?q=page", nil, 200).Body.Bytes(), &page); err != nil || len(page.Entries) != 500 || page.Next.ID == "" {
		t.Fatal("first log page invalid", len(page.Entries), err)
	}
	var older struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(f.call("root", "default", "GET", "/api/workloads/telemetry-resource/log-history?q=page&before="+page.Next.Before+"&before_id="+page.Next.ID, nil, 200).Body.Bytes(), &older); err != nil || len(older.Entries) != 1 {
		t.Fatal("equal-timestamp entries lost during pagination", len(older.Entries), err)
	}
	f.sql("INSERT INTO teams(id,name) VALUES('telemetry-other','Other')")
	f.call("root", "telemetry-other", "GET", "/api/workloads/telemetry-resource/metrics", nil, 404)
	f.call("root", "telemetry-other", "GET", "/api/workloads/telemetry-resource/log-history", nil, 404)
	member := f.user("telemetry-member", "telemetry-member@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','telemetry-member','member','[{"scope":"team","role":"viewer"}]')`)
	f.call(member, "default", "GET", "/api/workloads/telemetry-resource/log-history", nil, 200)
	f.call(member, "default", "GET", "/api/settings/retention", nil, 403)
	f.call("root", "default", "GET", "/api/settings/storage-usage", nil, 200)
	var config telemetryRetention
	if err := json.Unmarshal(f.call("root", "default", "GET", "/api/settings/retention", nil, 200).Body.Bytes(), &config); err != nil {
		t.Fatal(err)
	}
	config.MetricRows = 1000
	config.LogRows = 1000
	config.MetricBytes = 1048576
	config.LogBytes = 1048576
	f.call("root", "default", "PUT", "/api/settings/retention", config, 200)
	f.sql(`INSERT INTO metric_samples(node_id,series,tier,bucket,sampled_at,payload) SELECT 'telemetry-node','bulk-'||i,'minute',now(),now(),jsonb_build_object('padding',repeat('x',2000)) FROM generate_series(1,1100) i`)
	f.sql(`INSERT INTO workload_log_entries(id,workload_id,container_id,container,timestamp,message) SELECT 'bulk-'||i,'telemetry-resource','container','app',now(),repeat('x',2000) FROM generate_series(1,1100) i`)
	f.call("root", "default", "PUT", "/api/settings/retention", config, 200)
	var metricRows, logRows, metricBytes, logBytes int64
	if err := f.s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM metric_samples),(SELECT count(*) FROM workload_log_entries),(SELECT COALESCE(sum(octet_length(payload::text)+256),0) FROM metric_samples),(SELECT COALESCE(sum(octet_length(message)+octet_length(container)+256),0) FROM workload_log_entries)`).Scan(&metricRows, &logRows, &metricBytes, &logBytes); err != nil || metricRows > 1000 || logRows > 1000 || metricBytes > 1048576 || logBytes > 1048576 {
		t.Fatal("storage cap exceeded", metricRows, logRows, metricBytes, logBytes, err)
	}
	f.sql(`UPDATE metric_samples SET sampled_at=now()-interval '400 days',bucket=bucket-interval '400 days'`)
	f.sql(`UPDATE workload_log_entries SET timestamp=now()-interval '400 days'`)
	f.call("root", "default", "PUT", "/api/settings/retention", config, 200)
	if err := f.s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM metric_samples)+(SELECT count(*) FROM workload_log_entries)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired data retained", count, err)
	}
	config.LogBytes = 0
	f.call("root", "default", "PUT", "/api/settings/retention", config, 400)
}
