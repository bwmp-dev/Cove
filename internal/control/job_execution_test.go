package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"testing"
	"time"
)

func TestScheduledJobExecutionPolicies(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('job-node','Node','localhost',22,'',$1,'ready','{"job_runs":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('job-project','Project','default')`)
	input := workloadInput{Name: "Job", NodeID: "job-node", ProjectID: "job-project", Kind: "job", JobService: "app", Compose: "services:\n  app:\n    image: busybox:1.37\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	notification := jsonString(t, f.call("root", "default", "POST", "/api/notifications", connectionConfig{Name: "Jobs", Notification: &notificationConfig{Provider: "webhook", URL: "https://notify.example.test/jobs", Enabled: true, Failure: true}}, 201), "id")
	path := "/api/workloads/" + id + "/schedule"
	schedule := func() {
		t.Helper()
		if err := f.s.scheduleJobs(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	due := func() { f.sql(`UPDATE workloads SET next_job_at=now()-interval '1 minute' WHERE id=$1`, id) }
	var count int
	var pending *time.Time
	read := func(want int, waiting bool) {
		t.Helper()
		if err := f.s.DB.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM operations WHERE workload_id=$1),job_pending_at FROM workloads WHERE id=$1`, id).Scan(&count, &pending); err != nil || count != want || (pending != nil) != waiting {
			t.Fatal(count, pending, err)
		}
	}
	f.call("root", "default", "PUT", path, map[string]any{"cron": "* * * * *", "overlap_policy": "queue_one", "timeout_seconds": 25}, 200)
	due()
	schedule()
	due()
	schedule()
	due()
	schedule()
	read(1, true)
	f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id=$1`, id)
	schedule()
	read(2, false)
	schedule()
	read(2, false)
	var payload []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT payload FROM operations WHERE workload_id=$1 AND state='queued'`, id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	raw, err := unseal(f.s.Key, payload)
	var request worker.Request
	if err != nil || json.Unmarshal(raw, &request) != nil || request.Job == nil || request.Job.TimeoutSeconds != 25 {
		t.Fatal(request.Job, err)
	}
	f.call("root", "default", "PUT", path, map[string]any{"cron": "* * * * *", "overlap_policy": "replace"}, 200)
	due()
	schedule()
	var cancelled bool
	if err := f.s.DB.QueryRow(context.Background(), `SELECT cancel_requested FROM operations WHERE workload_id=$1 AND state='queued'`, id).Scan(&cancelled); err != nil || cancelled {
		t.Fatal("queued work was preempted", err)
	}
	f.sql(`UPDATE operations SET state='running' WHERE workload_id=$1 AND state='queued'`, id)
	schedule()
	read(2, true)
	if err := f.s.DB.QueryRow(context.Background(), `SELECT cancel_requested FROM operations WHERE workload_id=$1 AND state='running'`, id).Scan(&cancelled); err != nil || !cancelled {
		t.Fatal("running job not cancelled", err)
	}
	schedule()
	read(2, true)
	f.sql(`UPDATE operations SET state='cancelled' WHERE workload_id=$1 AND state='running'`, id)
	schedule()
	read(3, false)
	due()
	schedule()
	read(3, true)
	f.call("root", "default", "PUT", path, map[string]any{"cron": ""}, 200)
	schedule()
	read(3, false)
	f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id=$1`, id)
	queued := f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "deploy"}, 202)
	runID := jsonString(t, queued, "id")
	if runID == "" {
		t.Fatal("run-now did not queue")
	}
	f.sql(`UPDATE operations SET state='failed',result='{"job":{"timed_out":true,"containers_stopped":true}}' WHERE id=$1`, runID)
	var deliveries int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM notification_deliveries WHERE operation_id=$1 AND connection_id=$2`, runID, notification).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatal("job failure did not create an alert event", deliveries, err)
	}
	for _, body := range []map[string]any{{"cron": "", "overlap_policy": "parallel"}, {"cron": "", "timeout_seconds": 4}, {"cron": "", "timeout_seconds": 86401}} {
		f.call("root", "default", "PUT", path, body, 400)
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='job-node'`)
	f.call("root", "default", "PUT", path, map[string]any{"cron": "* * * * *"}, 409)
	f.call("root", "default", "PUT", path, map[string]any{"cron": "* * * * *", "overlap_policy": "skip", "timeout_seconds": 1800}, 200)
}

func TestJobScheduleRetainsTicksBlockedByImageMetadata(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('metadata-node','Node','localhost',22,'',$1,'ready','{"job_runs":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('metadata-project','Project','default')`)
	for _, policy := range []string{"skip", "queue_one", "replace"} {
		input := workloadInput{Name: policy, NodeID: "metadata-node", ProjectID: "metadata-project", Kind: "job", JobService: "task", Compose: "services:\n  task:\n    image: busybox:1.37\n    command: ['true']\n"}
		id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
		f.call("root", "default", "PUT", "/api/workloads/"+id+"/schedule", map[string]any{"cron": "* * * * *", "overlap_policy": policy}, 200)
		f.sql(`UPDATE workloads SET next_job_at=now()-interval '1 minute' WHERE id=$1`, id)
		check := resourceID()
		f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES($1,$2,'image_check','running',$3)`, check, id, []byte{})
		for range 2 {
			if err := f.s.scheduleJobs(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		var pending *time.Time
		var next time.Time
		var cancelled bool
		var runs int
		if err := f.s.DB.QueryRow(context.Background(), `SELECT w.job_pending_at,w.next_job_at,o.cancel_requested,(SELECT count(*) FROM operations WHERE workload_id=w.id AND action='deploy') FROM workloads w JOIN operations o ON o.id=$2 WHERE w.id=$1`, id, check).Scan(&pending, &next, &cancelled, &runs); err != nil || pending == nil || !next.After(time.Now()) || cancelled || runs != 0 {
			t.Fatal("metadata consumed or preempted the tick", policy, pending, next, cancelled, runs, err)
		}
		f.sql(`UPDATE operations SET state='failed' WHERE id=$1`, check)
		if err := f.s.scheduleJobs(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := f.s.DB.QueryRow(context.Background(), `SELECT job_pending_at,(SELECT count(*) FROM operations WHERE workload_id=w.id AND action='deploy' AND state='queued') FROM workloads w WHERE id=$1`, id).Scan(&pending, &runs); err != nil || pending != nil || runs != 1 {
			t.Fatal("held execution was not queued once", policy, pending, runs, err)
		}
		f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id=$1 AND state='queued'`, id)
	}
}
