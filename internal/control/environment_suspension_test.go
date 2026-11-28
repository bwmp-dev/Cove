package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"strings"
	"testing"
)

func TestEnvironmentSuspensionIntentAndRecovery(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('pause-node','Node','localhost',22,'',$1,'ready','{"suspensions":true,"job_runs":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('pause-project','Project','default')`)
	f.sql(`INSERT INTO environments(id,name,project_id,team_id) VALUES('pause-env','Development','pause-project','default')`)
	create := func(name, kind string) string {
		t.Helper()
		input := workloadInput{Name: name, NodeID: "pause-node", ProjectID: "pause-project", EnvironmentID: "pause-env", Kind: kind, Compose: "services:\n  app:\n    image: busybox:1.37\n"}
		if kind == "job" {
			input.JobService = "app"
		}
		return jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	}
	app, job, stopped := create("App", "service"), create("Job", "job"), create("Stopped", "service")
	f.sql(`UPDATE workloads SET manually_stopped=true WHERE id=$1`, stopped)
	viewer := f.user("pause-viewer", "pause-viewer@example.test")
	operator := f.user("pause-operator", "pause-operator@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','pause-viewer','member','[{"scope":"environment","scope_id":"pause-env","role":"viewer"}]'),('default','pause-operator','member','[{"scope":"environment","scope_id":"pause-env","role":"operator"}]')`)
	path := "/api/environments/pause-env/suspension"
	f.call(viewer, "default", "GET", path, nil, 200)
	f.call(viewer, "default", "PUT", path, map[string]bool{"paused": true}, 403)
	f.call(operator, "default", "PUT", path, map[string]bool{}, 400)
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='pause-node'`)
	f.call(operator, "default", "PUT", path, map[string]bool{"paused": true}, 409)
	f.sql(`UPDATE nodes SET capabilities='{"suspensions":true,"job_runs":true}' WHERE id='pause-node'`)
	queued := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+app+"/operations", map[string]string{"action": "deploy"}, 202), "id")
	f.call(operator, "default", "PUT", path, map[string]bool{"paused": true}, 202)
	var cancelled bool
	if err := f.s.DB.QueryRow(ctx, `SELECT cancel_requested FROM operations WHERE id=$1`, queued).Scan(&cancelled); err != nil || !cancelled {
		t.Fatal(cancelled, err)
	}
	for _, action := range []string{"deploy", "start", "restart", "rollback", "rollout_promote", "restore", "drill"} {
		f.call("root", "default", "POST", "/api/workloads/"+app+"/operations", map[string]string{"action": action}, 409)
	}
	if !strings.Contains(f.call("root", "default", "GET", "/api/workloads/"+app, nil, 200).Body.String(), `"environment_paused":true`) {
		t.Fatal("pause intent not exposed")
	}
	f.call("root", "default", "PUT", "/api/workloads/"+app+"/environment", map[string]string{"project_id": "pause-project"}, 409)
	step := func() {
		t.Helper()
		if err := f.s.reconcileEnvironmentSuspensions(ctx); err != nil {
			t.Fatal(err)
		}
	}
	step()
	var ops int
	f.s.DB.QueryRow(ctx, `SELECT count(*) FROM operations WHERE workload_id=$1 AND action='suspend'`, app).Scan(&ops)
	if ops != 0 {
		t.Fatal("pause raced active cleanup")
	}
	f.sql(`UPDATE operations SET state='cancelled' WHERE id=$1`, queued)
	step()
	late := create("Added while paused", "service")
	step()
	requests := func(action string) map[string]worker.Request {
		t.Helper()
		rows, err := f.s.DB.Query(ctx, `SELECT payload FROM operations WHERE action=$1 AND state='queued'`, action)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]worker.Request{}
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&raw); err != nil {
				t.Fatal(err)
			}
			data, err := unseal(f.s.Key, raw)
			var r worker.Request
			if err != nil || json.Unmarshal(data, &r) != nil || worker.ValidateRequest(r) != nil {
				t.Fatal(r, err)
			}
			out[r.Workload] = r
		}
		if rows.Err() != nil {
			t.Fatal(rows.Err())
		}
		return out
	}
	pauses := requests("suspend")
	if len(pauses) != 4 {
		t.Fatal(pauses)
	}
	// A quick reversal must wait for each older pause before it can resume.
	f.call(operator, "default", "PUT", path, map[string]bool{"paused": false}, 202)
	var reversal struct {
		Paused bool
		Phase  string
	}
	if err := json.Unmarshal(f.call(viewer, "default", "GET", path, nil, 200).Body.Bytes(), &reversal); err != nil || reversal.Paused || reversal.Phase != "resuming" {
		t.Fatal("reversal reported the older desired state", reversal, err)
	}
	step()
	if len(requests("resume")) != 0 {
		t.Fatal("resume overtook pause")
	}
	for id, r := range pauses {
		running := 1
		if id == late || id == stopped {
			running = 0
		}
		result := worker.Operation{Suspension: &worker.SuspensionStatus{Phase: "paused", PreviouslyRunning: running}}
		raw, _ := json.Marshal(result)
		f.sql(`UPDATE operations SET state='succeeded',result=$2 WHERE id=$1`, r.ID, raw)
	}
	step()
	resumes := requests("resume")
	if len(resumes) != 4 || !resumes[app].Suspension.Start || resumes[job].Suspension.Start || resumes[late].Suspension.Start || resumes[stopped].Suspension.Start {
		t.Fatal(resumes)
	}
	f.sql(`UPDATE operations SET state='failed' WHERE id=$1`, resumes[app].ID)
	for id, r := range resumes {
		if id != app {
			f.sql(`UPDATE operations SET state='succeeded',result='{"suspension":{"phase":"resumed"}}' WHERE id=$1`, r.ID)
		}
	}
	step()
	if !strings.Contains(f.call(viewer, "default", "GET", path, nil, 200).Body.String(), `"phase":"error"`) {
		t.Fatal("failed resume shown as active")
	}
	step()
	if len(requests("resume")) != 0 {
		t.Fatal("failed resume retried without intent")
	}
	f.call(operator, "default", "PUT", path, map[string]bool{"paused": false}, 202)
	step()
	resumes = requests("resume")
	if len(resumes) != 1 || !resumes[app].Suspension.Start {
		t.Fatal(resumes)
	}
	f.sql(`UPDATE operations SET state='succeeded',result='{"suspension":{"phase":"resumed"}}' WHERE id=$1`, resumes[app].ID)
	step()
	if !strings.Contains(f.call(viewer, "default", "GET", path, nil, 200).Body.String(), `"phase":"active"`) {
		t.Fatal("resume not finalized")
	}
	f.call("root", "default", "PUT", path, map[string]bool{"paused": true}, 202)
	// A queue source racing the pause must be cancelled before worker admission.
	request := worker.Request{ID: "racing-pause", Workload: app, Action: "start"}
	payload, err := f.s.sealJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES('racing-pause',$1,'start',$2)`, app, payload)
	if err = f.s.DB.QueryRow(ctx, `SELECT cancel_requested FROM operations WHERE id='racing-pause'`).Scan(&cancelled); err != nil || !cancelled {
		t.Fatal(cancelled, err)
	}
	f.sql(`UPDATE workloads SET job_cron='* * * * *',next_job_at=now()-interval '1 minute',job_pending_at=now() WHERE id=$1`, job)
	if err = f.s.scheduleJobs(ctx); err != nil {
		t.Fatal(err)
	}
	var pending bool
	if err = f.s.DB.QueryRow(ctx, `SELECT job_pending_at IS NOT NULL FROM workloads WHERE id=$1`, job).Scan(&pending); err != nil || pending {
		t.Fatal(pending, err)
	}
	f.sql(`UPDATE operations SET state='cancelled' WHERE state IN ('queued','running')`)
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES('before-pause',$1,'deploy','succeeded',$2)`, app, payload)
	f.sql(`INSERT INTO health_alerts(team_id,kind,subject,message) VALUES('default','resource_unhealthy',$1,'old alert')`, app)
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		t.Fatal("intentional environment pause was inspected as unexpected downtime")
		return worker.RPCResponse{}, nil
	}
	if err = f.s.checkResourceHealth(ctx); err != nil {
		t.Fatal(err)
	}
	var alerts int
	if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM health_alerts WHERE subject=$1`, app).Scan(&alerts); err != nil || alerts != 0 {
		t.Fatal(alerts, err)
	}
	step()
	for _, r := range requests("suspend") {
		f.sql(`UPDATE operations SET state='succeeded',result='{"suspension":{"phase":"paused","previously_running":1}}' WHERE id=$1`, r.ID)
	}
	step()
	step()
	var stable struct {
		Phase     string
		Resources []suspendedResource
	}
	if err = json.Unmarshal(f.call(viewer, "default", "GET", path, nil, 200).Body.Bytes(), &stable); err != nil || stable.Phase != "paused" {
		t.Fatal(stable, err)
	}
	for _, resource := range stable.Resources {
		if resource.Operation == "" || resource.Phase != "paused" {
			t.Fatal("confirmed pause lost its operation link", resource)
		}
	}
	if len(requests("resume")) != 0 {
		t.Fatal("processed pause was mistaken for a resume on the next reconciliation")
	}
}
