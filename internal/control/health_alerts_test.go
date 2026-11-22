package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestHealthDebounceRecoveryAndIsolation(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	owner := f.user("health-owner", "health@example.com")
	viewer := f.user("health-viewer", "viewer-health@example.com")
	f.sql("INSERT INTO teams(id,name) VALUES('health','Health'),('health-other','Other')")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('health','health-owner','owner','[]'),('health','health-viewer','member','[]')`)
	c := connectionConfig{Name: "Health", Notification: &notificationConfig{Provider: "webhook", URL: "https://example.com/hook", Enabled: true, Health: true}}
	f.call(owner, "health", "POST", "/api/notifications", c, 201)
	record := func(bad bool) {
		t.Helper()
		if err := f.s.recordHealth(ctx, "health", "node_unreachable", "test-node", "Worker unavailable", bad); err != nil {
			t.Fatal(err)
		}
	}
	count := func(want int) {
		t.Helper()
		var got int
		if err := f.s.DB.QueryRow(ctx, "SELECT count(*) FROM health_deliveries").Scan(&got); err != nil || got != want {
			t.Fatal(got, want, err)
		}
	}
	record(true)
	count(0)
	f.sql("UPDATE health_alerts SET first_seen=now()-interval '3 minutes'")
	record(true)
	count(1)
	record(true)
	count(1)
	if !strings.Contains(f.call(owner, "health", "GET", "/api/notifications/health", nil, 200).Body.String(), "Worker unavailable") {
		t.Fatal("alert missing")
	}
	f.call(viewer, "health", "GET", "/api/notifications/health", nil, 403)
	if strings.Contains(f.call("root", "health-other", "GET", "/api/notifications/health", nil, 200).Body.String(), "Worker unavailable") {
		t.Fatal("cross-team alert")
	}
	record(false)
	count(2)
	record(false)
	count(2)
	sent := 0
	f.s.NotificationTransport = notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
		sent++
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable")), Header: http.Header{}}, nil
	})
	if err := f.s.dispatchHealthNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if sent != 2 {
		t.Fatal("health delivery not attempted", sent)
	}
	f.sql("UPDATE health_deliveries SET next_attempt=now()")
	f.s.NotificationTransport = notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
	})
	if err := f.s.dispatchHealthNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	var delivered int
	if err := f.s.DB.QueryRow(ctx, "SELECT count(*) FROM health_deliveries WHERE state='sent' AND attempts=2").Scan(&delivered); err != nil || delivered != 2 {
		t.Fatal(delivered, err)
	}
	f.call(owner, "health", "GET", "/api/notifications/health-history", nil, 200)
	f.call(viewer, "health", "GET", "/api/notifications/health-history", nil, 403)

}

func TestLimitsPersistAndRequireWorkerSupport(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('limit-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('limit-project','Project','default')")
	config := workloadInput{Name: "Limited", NodeID: "limit-node", ProjectID: "limit-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n", Limits: map[string]worker.ServiceLimits{"app": {CPUs: 0.5, MemoryMB: 64}}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	config.ProjectID = ""
	config.Limits = nil
	f.call("root", "default", "PUT", "/api/workloads/"+id, config, 200)
	response := f.call("root", "default", "GET", "/api/workloads/"+id, nil, 200)
	var saved workloadInput
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil || saved.Limits["app"].CPUs != 0.5 {
		t.Fatal(response.Body.String(), err)
	}
	req := worker.Request{Workload: id, Action: "deploy", Limits: saved.Limits}
	if f.s.prepareInfrastructure(context.Background(), &req) == nil {
		t.Fatal("old worker accepted limits")
	}
	f.sql(`UPDATE nodes SET capabilities='{"resource_limits":true}' WHERE id='limit-node'`)
	if err := f.s.prepareInfrastructure(context.Background(), &req); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(config)
	var cleared map[string]any
	if err := json.Unmarshal(raw, &cleared); err != nil {
		t.Fatal(err)
	}
	cleared["limits"] = map[string]any{}
	f.call("root", "default", "PUT", "/api/workloads/"+id, cleared, 200)
	response = f.call("root", "default", "GET", "/api/workloads/"+id, nil, 200)
	saved = workloadInput{}
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil || len(saved.Limits) != 0 {
		t.Fatal("limits not cleared", err)
	}
}

func TestBackupOverdueAndPolicyGrace(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('backup-health-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('backup-health-project','Project','default')")
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id,backup_interval,backup_policy_since) VALUES('backup-health','backup-health-node','Resource',$1,'backup-health-project','default',1,now()-interval '3 hours')", []byte{})
	if err := f.s.checkBackupHealth(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.s.DB.QueryRow(ctx, "SELECT count(*) FROM health_alerts WHERE kind='backup_overdue'").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	f.sql("UPDATE workloads SET backup_policy_since=now() WHERE id='backup-health'")
	if err := f.s.checkBackupHealth(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(ctx, "SELECT count(*) FROM health_alerts WHERE kind='backup_overdue'").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if err := f.s.dispatchHealthNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.checkResourceHealth(ctx); err != nil {
		t.Fatal(err)
	}
}
