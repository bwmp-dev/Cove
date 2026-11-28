package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWorkloadOwnerRoutingAndMembership(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	owner := f.user("resource-owner", "resource-owner@example.com")
	other := f.user("resource-outsider", "resource-outsider@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','resource-owner','member','[{"scope":"team","role":"viewer"}]')`)
	f.sql("UPDATE users SET email_verified=true WHERE id='resource-owner'")
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('owner-node','Node','localhost',22,'',$1)", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('owner-project','Project','default')")
	sealed, _ := seal(f.s.Key, []byte(`{}`))
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('owned','owner-node','Resource',$1,'owner-project','default')", sealed)
	f.call("root", "default", "PUT", "/api/workloads/owned/owner", map[string]string{"user_id": "resource-outsider"}, 400)
	f.call("root", "default", "PUT", "/api/workloads/owned/owner", map[string]string{"user_id": "resource-owner"}, 200)
	f.call(owner, "default", "PUT", "/api/workloads/owned/owner", map[string]string{"user_id": "resource-owner"}, 403)
	f.call(other, "default", "GET", "/api/workloads/owned/owner", nil, 401)
	response := f.call(owner, "default", "GET", "/api/workloads/owned/owner", nil, 200).Body.String()
	if !strings.Contains(response, `"user_id":"resource-owner"`) || !strings.Contains(response, `"candidates":[]`) {
		t.Fatal(response)
	}
	config := connectionConfig{Name: "Owners", Notification: &notificationConfig{Provider: "email", Owner: true, Enabled: true, Failure: true}}
	f.call("root", "default", "POST", "/api/notifications", config, 201)
	sent := 0
	f.s.SendMail = func(_ context.Context, to, _, _ string) error {
		if to != "resource-owner@example.com" {
			t.Error("wrong recipient")
		}
		sent++
		return nil
	}
	f.sql("INSERT INTO operations(id,workload_id,action,payload) VALUES('owner-op','owned','deploy',$1)", sealed)
	f.sql("UPDATE operations SET state='failed' WHERE id='owner-op'")
	if err := f.s.dispatchNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatal("owner not notified", sent)
	}
	f.sql("UPDATE users SET disabled=true WHERE id='resource-owner'")
	f.sql("INSERT INTO operations(id,workload_id,action,payload) VALUES('owner-disabled-op','owned','deploy',$1)", sealed)
	f.sql("UPDATE operations SET state='failed' WHERE id='owner-disabled-op'")
	if err := f.s.dispatchNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatal("disabled owner notified", sent)
	}
	f.sql("DELETE FROM team_members WHERE team_id='default' AND user_id='resource-owner'")
	email, err := f.s.notificationOwnerEmail(ctx, "default", "owned")
	if err != nil || email != "" {
		t.Fatal("removed member still receives alerts", email, err)
	}
	var selected *string
	if err = f.s.DB.QueryRow(ctx, "SELECT owner_user_id FROM workloads WHERE id='owned'").Scan(&selected); err != nil || selected != nil {
		t.Fatal("owner assignment not cleared", selected, err)
	}
}

func TestThresholdRulesRoutingConfirmationRecovery(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('threshold-node','Node','localhost',22,'',$1)", []byte{})
	f.sql("INSERT INTO team_nodes(team_id,node_id) VALUES('default','threshold-node')")
	c := connectionConfig{Name: "Threshold", Notification: &notificationConfig{Provider: "webhook", URL: "https://example.com/hook", Enabled: true, Events: []string{"threshold.alert", "threshold.recovered"}}}
	channel := jsonString(t, f.call("root", "default", "POST", "/api/notifications", c, 201), "id")
	c.Name = "Not routed"
	f.call("root", "default", "POST", "/api/notifications", c, 201)
	rule := thresholdRule{Name: "CPU pressure", NodeID: "threshold-node", Metric: "cpu", Threshold: 80, Confirmation: 60, Repeat: 300, Channels: []string{channel}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/notifications/rules", rule, 201), "id")
	check := func(cpu float64) {
		t.Helper()
		if err := f.s.evaluateThresholdSample(ctx, "threshold-node", &worker.NodeMetrics{SampledAt: time.Now(), CPUPercent: cpu}); err != nil {
			t.Fatal(err)
		}
	}
	count := func(want int) {
		t.Helper()
		var n int
		if err := f.s.DB.QueryRow(ctx, "SELECT count(*) FROM health_deliveries").Scan(&n); err != nil || n != want {
			t.Fatal(n, want, err)
		}
	}
	if err := f.s.evaluateThresholdSample(ctx, "threshold-node", &worker.NodeMetrics{SampledAt: time.Now().Add(-3 * time.Minute), CPUPercent: 99}); err != nil {
		t.Fatal(err)
	}
	count(0)
	check(90)
	count(0)
	f.sql("UPDATE health_alerts SET first_seen=now()-interval '4 minutes',checked_at=now()-interval '3 minutes'")
	check(90)
	count(0)
	f.sql("UPDATE health_alerts SET first_seen=now()-interval '2 minutes'")
	check(90)
	count(1)
	check(90)
	count(1)
	f.sql("UPDATE health_alerts SET last_notified=now()-interval '6 minutes'")
	check(90)
	count(2)
	check(10)
	count(3)
	check(10)
	count(3)
	var wrong int
	if err := f.s.DB.QueryRow(ctx, "SELECT count(*) FROM health_deliveries WHERE connection_id<>$1", channel).Scan(&wrong); err != nil || wrong != 0 {
		t.Fatal("wrong channel", wrong, err)
	}
	var listed []thresholdRule
	if err := json.Unmarshal(f.call("root", "default", "GET", "/api/notifications/rules", nil, 200).Body.Bytes(), &listed); err != nil || len(listed) != 1 {
		t.Fatal(listed, err)
	}
	f.call("root", "default", "DELETE", "/api/notifications/rules/"+id, nil, 200)
	check(90)
	count(3)
	f.call("root", "default", "DELETE", "/api/notifications/rules/"+id, nil, 404)
	rule.Channels = []string{"missing"}
	f.call("root", "default", "POST", "/api/notifications/rules", rule, 400)
	f.sql("INSERT INTO teams(id,name) VALUES('threshold-other','Other')")
	if body := f.call("root", "threshold-other", "GET", "/api/notifications/rules", nil, 200).Body.String(); strings.Contains(body, "CPU pressure") {
		t.Fatal("cross-team rules exposed")
	}
	rule.Channels = []string{channel}
	f.call("root", "threshold-other", "POST", "/api/notifications/rules", rule, 400)
	rule.Metric = "certificate"
	f.call("root", "default", "POST", "/api/notifications/rules", rule, 400)
	f.sql(`INSERT INTO threshold_rules(id,team_id,node_id,name,metric,threshold,confirmation_seconds,repeat_seconds,channel_ids) SELECT 'cap-'||i,'default','threshold-node','Cap','cpu',80,60,300,$1 FROM generate_series(1,100) i`, []string{channel})
	rule.Metric = "cpu"
	f.call("root", "default", "POST", "/api/notifications/rules", rule, 409)
}
