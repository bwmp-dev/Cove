package control

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestDeploymentWindows(t *testing.T) {
	for _, v := range []struct {
		start, end, now string
		allowed         bool
	}{
		{"", "", "12:00", true}, {"09:00", "17:00", "09:00", true}, {"09:00", "17:00", "17:00", false}, {"23:00", "02:00", "01:59", true}, {"23:00", "02:00", "02:00", false}, {"23:00", "02:00", "23:00", true}, {"23:00", "02:00", "12:00", false}, {"09:00", "", "12:00", false}, {"09:00", "09:00", "09:00", false},
	} {
		if withinDeploymentWindow(v.start, v.end, v.now) != v.allowed {
			t.Fatal(v)
		}
	}
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('window-node','Node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('window-project','Project','default')`)
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "App", NodeID: "window-node", ProjectID: "window-project", Compose: "services: {}"}, 201), "id")
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES('window-op',$1,'deploy',$2)`, id, []byte{})
	now := time.Now().UTC()
	policy := deploymentPolicy{WindowStart: now.Add(time.Hour).Format("15:04"), WindowEnd: now.Add(2 * time.Hour).Format("15:04")}
	f.call("root", "default", "PUT", "/api/projects/window-project/deployment-policy", policy, 200)
	if admitted, err := f.s.admitDeployment(context.Background(), "window-op"); err != nil || admitted {
		t.Fatal("outside-window deployment admitted", admitted, err)
	}
	policy.WindowStart = now.Add(-time.Hour).Format("15:04")
	policy.WindowEnd = now.Add(time.Hour).Format("15:04")
	f.call("root", "default", "PUT", "/api/projects/window-project/deployment-policy", policy, 200)
	if admitted, err := f.s.admitDeployment(context.Background(), "window-op"); err != nil || !admitted {
		t.Fatal("inside-window deployment held", admitted, err)
	}
}

func TestDeploymentPolicyApprovalFreezeAndCancellation(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('policy-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('policy-project','Project','default')")
	f.sql("INSERT INTO environments(id,project_id,name) VALUES('policy-env','policy-project','Production')")
	input := workloadInput{Name: "App", NodeID: "policy-node", ProjectID: "policy-project", EnvironmentID: "policy-env", Compose: "services:\n  app:\n    image: busybox:1.37\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	editor := f.user("policy-editor", "policy-editor@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','policy-editor','member','[{"scope":"team","role":"editor"}]')`)
	path := "/api/projects/policy-project/deployment-policy"
	f.call(editor, "default", "PUT", path, deploymentPolicy{Approval: true}, 403)
	f.call("root", "default", "PUT", path, deploymentPolicy{Approval: true}, 200)
	f.sql("INSERT INTO operations(id,workload_id,action,payload) VALUES('policy-op',$1,'deploy',$2)", id, []byte{})
	assertAdmission := func(want bool) {
		t.Helper()
		ok, err := f.s.admitDeployment(context.Background(), "policy-op")
		if err != nil || ok != want {
			t.Fatalf("admission %v, %v; want %v", ok, err, want)
		}
	}
	assertAdmission(false)
	var detail struct {
		Approval   bool `json:"approval_required"`
		CanApprove bool `json:"can_approve"`
	}
	if err := json.Unmarshal(f.call(editor, "default", "GET", "/api/operations/policy-op", nil, 200).Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if !detail.Approval || detail.CanApprove {
		t.Fatal(detail)
	}
	f.call(editor, "default", "POST", "/api/operations/policy-op/approve", map[string]any{}, 403)
	// Removing a policy does not silently approve deployments already awaiting review.
	f.call("root", "default", "PUT", path, deploymentPolicy{}, 200)
	assertAdmission(false)
	f.call("root", "default", "POST", "/api/operations/policy-op/approve", map[string]any{}, 200)
	f.call("root", "default", "POST", "/api/operations/policy-op/approve", map[string]any{}, 409)
	from, until := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	f.call("root", "default", "PUT", "/api/environments/policy-env/deployment-policy", deploymentPolicy{FreezeFrom: &from, FreezeUntil: &until}, 200)
	assertAdmission(false)
	var slots int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM operations WHERE id='policy-op' AND admitted_at IS NOT NULL").Scan(&slots); err != nil || slots != 0 {
		t.Fatal(slots, err)
	}
	f.sql("UPDATE operations SET cancel_requested=true WHERE id='policy-op'")
	assertAdmission(true)
	f.sql("UPDATE operations SET cancel_requested=false WHERE id='policy-op'")
	f.call("root", "default", "PUT", "/api/environments/policy-env/deployment-policy", deploymentPolicy{}, 200)
	assertAdmission(true)
	// A freeze affects future admissions, never operations already handed to a worker.
	f.call("root", "default", "PUT", path, deploymentPolicy{FreezeFrom: &from, FreezeUntil: &until}, 200)
	assertAdmission(true)
}
