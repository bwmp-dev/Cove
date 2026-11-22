package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestResourceThresholdMeasurements(t *testing.T) {
	now := time.Now()
	expiry := now.Add(12 * time.Hour)
	s := &worker.WorkloadObservations{SampledAt: now, EventsAvailable: true, RestartEvents: 4, OOMEvents: 1, CrashEvents: 2, Certificates: []worker.CertificateObservation{{Domain: "app.example.test", ExpiresAt: &expiry}}, Containers: []worker.ContainerObservation{{Exited: true, ExpectedCompleted: true}}}
	for metric, want := range map[string]float64{"certificate": 0.5, "restarts": 4, "oom": 1, "crashes": 2} {
		got, known := resourceThresholdValue(metric, "app.example.test", s)
		if !known || got != want {
			t.Fatal(metric, got, known)
		}
	}
	s.EventsAvailable = false
	if _, known := resourceThresholdValue("oom", "", s); known {
		t.Fatal("unknown event stream treated as healthy")
	}
	s.SampledAt = now.Add(-3 * time.Minute)
	if _, known := resourceThresholdValue("certificate", "app.example.test", s); known {
		t.Fatal("stale certificate sample used")
	}
}

func TestResourceThresholdRoutingSuppressionAndRecovery(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('observations-node','Node','localhost',22,'',$1,'ready','{"alert_observations":true}')`, []byte{})
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('default','observations-node')`)
	config, _ := seal(f.s.Key, []byte(`{"routes":[{"domain":"app.example.test","service":"app","port":80}]}`))
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('observations-project','Project','default')`)
	f.sql(`INSERT INTO workloads(id,name,node_id,configuration,team_id,project_id) VALUES('observed','Observed','observations-node',$1,'default','observations-project')`, config)
	c := connectionConfig{Name: "Alerts", Notification: &notificationConfig{Provider: "webhook", URL: "https://example.com/hook", Enabled: true, Health: true}}
	channel := jsonString(t, f.call("root", "default", "POST", "/api/notifications", c, 201), "id")
	rule := thresholdRule{Name: "Crashes", WorkloadID: "observed", Metric: "crashes", Threshold: 1, Confirmation: 60, Repeat: 300, Channels: []string{channel}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/notifications/rules", rule, 201), "id")
	now := time.Now()
	sample := &worker.WorkloadObservations{SampledAt: now, EventsAvailable: true, Events: []worker.ContainerEvent{{Action: "die", Timestamp: now.Add(-time.Minute)}}}
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "observations-node" || r.Method != "observations" {
			t.Fatal(node, r)
		}
		return worker.RPCResponse{Observations: sample}, nil
	}
	check := func() {
		t.Helper()
		if err := f.s.evaluateResourceThresholds(ctx, "observed"); err != nil {
			t.Fatal(err)
		}
	}
	check()
	f.sql(`UPDATE health_alerts SET first_seen=now()-interval '2 minutes'`)
	check()
	var count int
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM health_deliveries WHERE subject_id='observed' AND event='threshold.alert'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	sample.Events = nil
	sample.Containers = []worker.ContainerObservation{{Exited: true, ExitUnknown: true}}
	check()
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM health_deliveries WHERE event='threshold.recovered'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("unknown exit caused recovery", count, err)
	}
	sample.Containers[0].ExitUnknown = false
	sample.Containers[0].ManualStop = true
	check()
	f.sql(`UPDATE workloads SET manually_stopped=true WHERE id='observed'`)
	check()
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM health_deliveries WHERE event='threshold.recovered'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	f.sql(`UPDATE workloads SET manually_stopped=false WHERE id='observed'`)
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,created_at,updated_at) VALUES('intentional','observed','restart','succeeded',$1,now()-interval '2 minutes',now())`, config)
	check()
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM health_alerts WHERE kind=$1`, "threshold_"+id).Scan(&count); err != nil || count != 0 {
		t.Fatal("intentional event alerted", count, err)
	}
	rule.Metric = "certificate"
	rule.Domain = "foreign.example.test"
	f.call("root", "default", "POST", "/api/notifications/rules", rule, 400)
	rule.Domain = "app.example.test"
	rule.Threshold = 14
	f.call("root", "default", "POST", "/api/notifications/rules", rule, 201)
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='observations-node'`)
	f.call("root", "default", "POST", "/api/notifications/rules", rule, 409)
	var listed []thresholdRule
	if err := json.Unmarshal(f.call("root", "default", "GET", "/api/notifications/rules", nil, 200).Body.Bytes(), &listed); err != nil || len(listed) != 2 || !strings.Contains(listed[0].WorkloadID, "observed") {
		t.Fatal(listed, err)
	}
}
