package control

import (
	"context"
	"cove/internal/outbound"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cove/internal/worker"
)

func TestTelemetryForwardingUptimeTrafficAndMetrics(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('output-node','Node','localhost',22,'',$1,'ready','{"traffic_logs":true}')`, []byte{})
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('default','output-node')`)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('output-project','Output','default')`)
	configuration, _ := seal(f.s.Key, []byte(`{"kind":"service","routes":[{"domain":"app.example.test","service":"app","port":80}]}`))
	f.sql(`INSERT INTO workloads(id,name,node_id,project_id,team_id,configuration) VALUES('output-resource','Output','output-node','output-project','default',$1)`, configuration)
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method != "traffic_control" || r.TrafficControl == nil {
			t.Fatal(r)
		}
		response := worker.RPCResponse{TrafficState: &worker.ProxyTrafficState{}}
		if r.TrafficControl.Action == "apply" {
			response.Error = "wait for worker operations and proxy recovery"
		}
		return response, nil
	}
	f.call("root", "default", "GET", "/api/nodes/output-node/traffic", nil, 200)
	blocked := f.call("root", "default", "POST", "/api/nodes/output-node/traffic", map[string]string{"action": "apply"}, 409)
	if !strings.Contains(blocked.Body.String(), "active worker operations") {
		t.Fatal("actionable proxy failure missing", blocked.Body.String())
	}
	forwarder := logForwarder{Name: "Destination", Kind: "loki", Endpoint: "https://receiver.example.test/loki/api/v1/push", Token: "never-return-this-secret", Enabled: true}
	id := jsonString(t, f.call("root", "default", "POST", "/api/notifications/forwarders", forwarder, 201), "id")
	listed := f.call("root", "default", "GET", "/api/notifications/forwarders", nil, 200).Body.String()
	if strings.Contains(listed, forwarder.Token) || strings.Contains(listed, forwarder.Endpoint) {
		t.Fatal("receiver secret returned", listed)
	}
	forwarder.Token = ""
	forwarder.Endpoint = ""
	f.call("root", "default", "PUT", "/api/notifications/forwarders/"+id, forwarder, 200)
	stored, err := f.s.readLogForwarder(ctx, id, "default")
	if err != nil || stored.Token != "never-return-this-secret" {
		t.Fatal(stored, err)
	}
	now := time.Now().UTC()
	sample := &worker.Telemetry{SampledAt: now, Logs: []worker.LogEntry{{ContainerID: "container", Container: "app", Timestamp: now, Message: "redacted entry"}}}
	for range 2 {
		if err = f.s.storeWorkloadTelemetry(ctx, "output-node", "output-resource", sample); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM log_forward_deliveries`).Scan(&count); err != nil || count != 1 {
		t.Fatal("replayed logs forwarded twice", count, err)
	}
	var queued []byte
	if err = f.s.DB.QueryRow(ctx, `SELECT payload FROM log_forward_deliveries`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(queued), "redacted entry") {
		t.Fatal("queued logs not sealed")
	}
	f.s.NotificationTransport = notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "receiver.example.test" || r.Header.Get("Authorization") != "Bearer never-return-this-secret" {
			t.Fatal(r.URL, r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "redacted entry") {
			t.Fatal(string(body))
		}
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if err = f.s.dispatchLogForwards(ctx); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM log_forward_deliveries`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	check := uptimeCheck{Domain: "app.example.test", Path: "/health", Enabled: true, Interval: 300, Timeout: 2, ExpectedMin: 200, ExpectedMax: 299, PublicLabel: "Website"}
	checkID := jsonString(t, f.call("root", "default", "POST", "/api/workloads/output-resource/uptime", check, 201), "id")
	f.s.NotificationTransport = notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://app.example.test/health" {
			t.Fatal(r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ignored body"))}, nil
	})
	if err = f.s.collectUptime(ctx); err != nil {
		t.Fatal(err)
	}
	channel := jsonString(t, f.call("root", "default", "POST", "/api/notifications", connectionConfig{Name: "Uptime", Notification: &notificationConfig{Provider: "webhook", URL: "https://example.test/hook", Enabled: false, Health: true}}, 201), "id")
	rule := thresholdRule{Name: "Website unavailable", Metric: "uptime", WorkloadID: "output-resource", Domain: check.Domain, Threshold: 1, Confirmation: 120, Repeat: 300, Channels: []string{channel}}
	ruleID := jsonString(t, f.call("root", "default", "POST", "/api/notifications/rules", rule, 201), "id")
	if err = f.s.evaluateUptimeRules(ctx, "output-resource", check.Domain, 1); err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE health_alerts SET first_seen=now()-interval '15 minutes',checked_at=now()-interval '6 minutes' WHERE kind=$1`, "threshold_"+ruleID)
	if err = f.s.evaluateUptimeRules(ctx, "output-resource", check.Domain, 1); err != nil {
		t.Fatal(err)
	}
	var active bool
	if err = f.s.DB.QueryRow(ctx, `SELECT active FROM health_alerts WHERE kind=$1`, "threshold_"+ruleID).Scan(&active); err != nil || !active {
		t.Fatal("long check interval reset confirmation", active, err)
	}
	if err = f.s.evaluateUptimeRules(ctx, "output-resource", check.Domain, 0); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM health_deliveries WHERE event='threshold.recovered'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("uptime recovery not routed", count, err)
	}
	if response := f.call("root", "default", "GET", "/api/workloads/output-resource/uptime", nil, 200); !strings.Contains(response.Body.String(), `"up":true`) {
		t.Fatal(response.Body.String())
	}
	settings := f.call("root", "default", "PUT", "/api/projects/output-project/status-page", map[string]any{"enabled": true, "title": "Our services"}, 200)
	slug := jsonString(t, settings, "slug")
	req := httptest.NewRequest("GET", "/api/status/"+slug, nil)
	res := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(res, req)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "Website") {
		t.Fatal(res.Code, res.Body.String())
	}
	for _, secret := range []string{checkID, "output-resource", "output-node", "app.example.test", "/health"} {
		if strings.Contains(res.Body.String(), secret) {
			t.Fatal("public identifiers leaked", res.Body.String())
		}
	}
	f.sql(`UPDATE route_checks SET checked_at=now()-interval '1 hour' WHERE id=$1`, checkID)
	res = httptest.NewRecorder()
	f.s.Handler().ServeHTTP(res, req)
	if !strings.Contains(res.Body.String(), `"up":null`) {
		t.Fatal("stale status presented as up", res.Body.String())
	}
	check.Path = "/ready"
	f.call("root", "default", "PUT", "/api/workloads/output-resource/uptime/"+checkID, check, 200)
	if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM route_check_results WHERE check_id=$1`, checkID).Scan(&count); err != nil || count != 0 {
		t.Fatal("new check path inherited old measurements", count, err)
	}
	traffic := &worker.TrafficReport{SampledAt: now, Available: true, Entries: []worker.TrafficEntry{{ID: strings.Repeat("c", 64), Workload: "output-resource", Domain: check.Domain, Timestamp: now, Status: 200, Bytes: 123, DurationMS: 12, ClientIP: "203.0.113.1"}, {ID: strings.Repeat("d", 64), Workload: "unknown", Domain: check.Domain, Timestamp: now, Status: 500}}}
	for range 2 {
		if err = f.s.storeTraffic(ctx, "output-node", traffic); err != nil {
			t.Fatal(err)
		}
	}
	response := f.call("root", "default", "GET", "/api/workloads/output-resource/traffic", nil, 200)
	if !strings.Contains(response.Body.String(), `"requests":1`) || !strings.Contains(response.Body.String(), `"p95_ms":12`) {
		t.Fatal(response.Body.String())
	}
	usage := f.call("root", "default", "GET", "/api/settings/storage-usage", nil, 200).Body.String()
	for _, key := range []string{`"traffic":`, `"uptime":`, `"forwarding":`, `"limit":`} {
		if !strings.Contains(usage, key) {
			t.Fatal("new store missing from storage usage", usage)
		}
	}
	f.sql(`INSERT INTO route_traffic(id,workload_id,node_id,domain,timestamp,status,bytes,duration_ms,client_ip) SELECT md5(n::text)||md5(n::text),'output-resource','output-node','history'||n||'.example.test',now(),200,1,1,'203.0.113.2' FROM generate_series(1,130) n`)
	var clipped struct {
		Routes  []json.RawMessage `json:"routes"`
		Limited bool              `json:"limited"`
	}
	if err = json.Unmarshal(f.call("root", "default", "GET", "/api/workloads/output-resource/traffic", nil, 200).Body.Bytes(), &clipped); err != nil || len(clipped.Routes) != 128 || !clipped.Limited {
		t.Fatal("route history clipping hidden", len(clipped.Routes), clipped.Limited, err)
	}
	metricsToken := jsonString(t, f.call("root", "", "POST", "/api/settings/metrics-token", nil, 200), "token")
	if err = f.s.storeNodeMetric(ctx, "output-node", &worker.NodeMetrics{SampledAt: now, CPUPercent: 25, MemoryTotal: 1024, MemoryAvailable: 512}); err != nil {
		t.Fatal(err)
	}
	if err = f.s.storeNodeMetric(ctx, "output-node", &worker.NodeMetrics{SampledAt: now.Add(24 * time.Hour), CPUPercent: 99, MemoryTotal: 1024, MemoryAvailable: 512}); err != nil {
		t.Fatal(err)
	}
	metrics := func(token string) *httptest.ResponseRecorder {
		q := httptest.NewRequest("GET", "/metrics", nil)
		q.Header.Set("Authorization", "Bearer "+token)
		p := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(p, q)
		return p
	}
	if metrics("root").Code != 401 {
		t.Fatal("admin token used as metrics token")
	}
	if p := metrics(metricsToken); p.Code != 200 || !strings.Contains(p.Body.String(), "cove_node_cpu_percent{") || !strings.Contains(p.Body.String(), "cove_node_cpu_percent{node=\"output-node\",workload=\"\",series=\"\"} 25\n") {
		t.Fatal(p.Code, p.Body.String())
	}
	f.call("root", "", "DELETE", "/api/settings/metrics-token", nil, 200)
	if metrics(metricsToken).Code != 401 {
		t.Fatal("revoked token accepted")
	}
	f.sql(`INSERT INTO log_forward_deliveries(id,forwarder_id,payload,entries) SELECT 'full-'||n,$1,'sealed-placeholder'::bytea,1 FROM generate_series(1,2000) n`, id)
	sample.SampledAt = now.Add(time.Second)
	sample.Logs[0].Timestamp = now.Add(time.Second)
	sample.Logs[0].Message = "bounded overflow"
	if err = f.s.storeWorkloadTelemetry(ctx, "output-node", "output-resource", sample); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM log_forward_deliveries`).Scan(&count); err != nil || count != 2000 {
		t.Fatal("queue cap not enforced", count, err)
	}
	var dropped int
	if err = f.s.DB.QueryRow(ctx, `SELECT dropped_entries FROM log_forwarders WHERE id=$1`, id).Scan(&dropped); err != nil || dropped != 1 {
		t.Fatal("overflow invisible", dropped, err)
	}
	f.sql(`UPDATE log_forward_deliveries SET created_at=now()-interval '25 hours'`)
	if err = f.s.dispatchLogForwards(ctx); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM log_forward_deliveries`).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired queue retained", count, err)
	}
	viewer := f.user("output-viewer", "output-viewer@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','output-viewer','member')`)
	f.call(viewer, "default", "GET", "/api/notifications/forwarders", nil, 403)
	f.call(viewer, "default", "POST", "/api/settings/metrics-token", nil, 403)
}

func TestUptimePathsAndPublicNetworkPolicy(t *testing.T) {
	base := uptimeCheck{Path: "/health", Interval: 60, Timeout: 1, ExpectedMin: 200, ExpectedMax: 299}
	for _, path := range []string{"//internal/path", "/x?secret=yes", "https://example.com/x", "/x#fragment", "/\n"} {
		c := base
		c.Path = path
		if validUptimeCheck(c) {
			t.Fatal("unsafe path", path)
		}
	}
	s := &Server{outboundState: &outboundProxyState{}}
	s.outboundState.value.Store(&outbound.Config{})
	base.Domain = "127.0.0.1"
	up, _, _, message := s.probeUptime(context.Background(), base)
	if up || message != "HTTPS check failed" {
		t.Fatal("private destination accepted", up, message)
	}
}

func TestPrometheusEscaping(t *testing.T) {
	if got := prometheusLabels("node", "a\"\nb\\c"); got != "{node=\"a\\\"\\nb\\\\c\"}" {
		t.Fatal(got)
	}
	var c uptimeCheck
	if err := json.Unmarshal([]byte(`{"path":"/"}`), &c); err != nil {
		t.Fatal(err)
	}
}
