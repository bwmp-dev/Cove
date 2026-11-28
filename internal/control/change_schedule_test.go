package control

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRecurringWindowsAcrossMidnightAndDST(t *testing.T) {
	p := deploymentPolicy{Timezone: "America/Los_Angeles", Windows: []changeWindow{{Days: []int{1}, Start: "23:00", End: "02:00"}}}
	if err := validateSchedule(&p); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		at   string
		want bool
	}{{"2026-11-24T07:30:00Z", true}, {"2026-11-24T09:30:00Z", true}, {"2026-11-24T10:00:00Z", false}, {"2026-11-25T07:30:00Z", false}} {
		now, _ := time.Parse(time.RFC3339, tt.at)
		got, e := scheduleWindowAllows(p, now)
		if e != nil || got != tt.want {
			t.Fatal(tt, got, e)
		}
	}
	p.Windows = []changeWindow{{Days: []int{0}, Start: "01:00", End: "02:00"}}
	for _, at := range []string{"2026-11-01T08:30:00Z", "2026-11-01T09:30:00Z"} {
		now, _ := time.Parse(time.RFC3339, at)
		got, e := scheduleWindowAllows(p, now)
		if e != nil || !got {
			t.Fatal("DST repeated wall-clock hour rejected", at, e)
		}
	}
	p.Timezone = "Invalid/Zone"
	if validateSchedule(&p) == nil {
		t.Fatal("invalid timezone accepted")
	}
}

func TestTeamFreezeAndEmergencyOverrideIntegration(t *testing.T) {
	f := newAccountFixture(t)
	editor := f.user("schedule-editor", "schedule-editor@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','schedule-editor','member','[{"scope":"team","role":"editor"}]')`)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('schedule-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('schedule-project','Project','default')")
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "App", NodeID: "schedule-node", ProjectID: "schedule-project", Compose: "services: {}"}, 201), "id")
	policy := deploymentPolicy{Timezone: "UTC", Freezes: []changeFreeze{{From: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour), Reason: "Maintenance"}}}
	f.call("root", "default", "PUT", "/api/teams/default/deployment-policy", policy, 200)
	f.call(editor, "default", "PUT", "/api/teams/default/deployment-policy", policy, 403)
	f.call(editor, "default", "GET", "/api/teams/default/deployment-policy", nil, 403)
	outsider := f.user("schedule-outsider", "outsider@example.com")
	f.call(outsider, "default", "GET", "/api/teams/default/deployment-policy", nil, 401)
	for _, trigger := range []string{"manual", "webhook", "sequence", "promotion", "preview"} {
		op := "freeze-" + trigger
		f.sql("INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'deploy',$3)", op, id, []byte{})
		allowed, e := f.s.admitDeployment(context.Background(), op)
		if e != nil || allowed {
			t.Fatal(trigger, "bypassed freeze", allowed, e)
		}
		f.call(editor, "default", "POST", "/api/operations/"+op+"/emergency-override", map[string]string{"reason": "Recovery"}, 403)
		f.call("root", "default", "POST", "/api/operations/"+op+"/emergency-override", map[string]string{"reason": ""}, 400)
		f.call("root", "default", "POST", "/api/operations/"+op+"/emergency-override", map[string]string{"reason": "Incident recovery"}, 200)
		allowed, e = f.s.admitDeployment(context.Background(), op)
		if e != nil || !allowed {
			t.Fatal(trigger, "override not applied", allowed, e)
		}
		f.call("root", "default", "POST", "/api/operations/"+op+"/emergency-override", map[string]string{"reason": "Again"}, 409)
		f.sql("UPDATE operations SET state='succeeded' WHERE id=$1", op)
	}
	w := f.call("root", "default", "GET", "/api/audit", nil, 200)
	if !strings.Contains(w.Body.String(), "Incident recovery") {
		t.Fatal("audit lost emergency reason", w.Body.String())
	}
}

func TestDeployReasonPersistsInHistoryAndAuditIntegration(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('reason-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('reason-project','Project','default')")
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "App", NodeID: "reason-node", ProjectID: "reason-project", Compose: "services: {}"}, 201), "id")
	w := f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "deploy", "reason": "Fix incident 42"}, 202)
	op := jsonString(t, w, "id")
	for _, path := range []string{"/api/operations/" + op, "/api/operations?workload_id=" + id, "/api/audit"} {
		w = f.call("root", "default", "GET", path, nil, 200)
		if !strings.Contains(w.Body.String(), "Fix incident 42") {
			t.Fatal(path, w.Body.String())
		}
	}
}
