package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestImageLifecycleAuthorizationAndCachedResults(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('image-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('image-project','Project','default')")
	config := workloadInput{Name: "App", NodeID: "image-node", ProjectID: "image-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	path := "/api/workloads/" + id
	f.call("root", "default", "POST", path+"/operations", map[string]any{"action": "image_check"}, 409)
	f.sql(`UPDATE nodes SET capabilities='{"image_lifecycle":true}' WHERE id='image-node'`)
	viewer := f.user("image-viewer", "image-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','image-viewer','member','[{"scope":"project","scope_id":"image-project","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", path+"/images", nil, 200)
	f.call(viewer, "default", "POST", path+"/operations", map[string]any{"action": "image_check"}, 403)
	operation := jsonString(t, f.call("root", "default", "POST", path+"/operations", map[string]any{"action": "image_check"}, 202), "id")
	f.sql(`INSERT INTO infrastructure_connections(id,team_id,name,kind,configuration) VALUES('image-notify','default','Notification','notification',$1)`, []byte{})
	result, _ := json.Marshal(worker.Operation{ImageCheck: &worker.ImageCheck{CheckedAt: time.Now().UTC(), Images: []worker.ImageStatus{{Service: "app", Reference: "busybox:1.37", Status: "update_available"}}}})
	f.sql("UPDATE operations SET state='succeeded',result=$2,updated_at=now() WHERE id=$1", operation, result)
	var response struct {
		Images []worker.ImageStatus `json:"images"`
		Stale  bool                 `json:"stale"`
	}
	if err := json.Unmarshal(f.call(viewer, "default", "GET", path+"/images", nil, 200).Body.Bytes(), &response); err != nil || response.Stale || len(response.Images) != 1 || response.Images[0].Status != "update_available" {
		t.Fatal(response, err)
	}
	var notices int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM notification_deliveries WHERE operation_id=$1", operation).Scan(&notices); err != nil || notices != 0 {
		t.Fatal("polling generated notification", notices, err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES('image-deploy',$1,'deploy','queued',$2)`, id, []byte{})
	f.sql(`UPDATE operations SET state='succeeded',updated_at=now() WHERE id='image-deploy'`)
	if err := json.Unmarshal(f.call(viewer, "default", "GET", path+"/images", nil, 200).Body.Bytes(), &response); err != nil || !response.Stale {
		t.Fatal("new deployment did not stale check", response, err)
	}
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM notification_deliveries WHERE operation_id='image-deploy'").Scan(&notices); err != nil || notices != 1 {
		t.Fatal("deployment notification changed", notices, err)
	}
	f.call("root", "default", "POST", path+"/operations", map[string]any{"action": "deploy", "pull_latest": true}, 202)
}

func TestImagePollingIsBoundedAndPreservesStoppedServices(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('image-node','Node','localhost',22,'',$1,'ready','{"image_lifecycle":true}')`, []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('image-project','Project','default')")
	for i := 0; i < 8; i++ {
		config := workloadInput{Name: fmt.Sprint("App", i), NodeID: "image-node", ProjectID: "image-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"}
		id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
		f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES($1,$2,'deploy','succeeded',$3)`, resourceID(), id, []byte{})
		if i == 0 {
			f.sql("UPDATE workloads SET manually_stopped=true WHERE id=$1", id)
		}
	}
	for i := 0; i < 2; i++ {
		if err := f.s.scheduleImageChecks(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM operations WHERE action='image_check'").Scan(&count); err != nil || count != 4 {
		t.Fatal(count, err)
	}
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE o.action='image_check' AND w.manually_stopped").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestJobImageChecksPreserveExecutionIntentAndInvalidateFailedRuns(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('job-images-node','Node','localhost',22,'',$1,'ready','{"image_lifecycle":true,"job_runs":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('job-images-project','Project','default')`)
	input := workloadInput{Name: "Job", NodeID: "job-images-node", ProjectID: "job-images-project", Kind: "job", JobService: "task", Compose: "services:\n  task:\n    image: busybox:1.37\n    command: ['true']\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	path := "/api/workloads/" + id
	f.call("root", "default", "POST", path+"/operations", map[string]any{"action": "image_check"}, 409)
	f.sql(`UPDATE nodes SET capabilities='{"image_lifecycle":true,"job_image_lifecycle":true,"job_runs":true}' WHERE id='job-images-node'`)
	f.sql(`UPDATE workloads SET manually_stopped=true,next_job_at=now()+interval '1 day' WHERE id=$1`, id)
	var before, after time.Time
	if err := f.s.DB.QueryRow(context.Background(), "SELECT next_job_at FROM workloads WHERE id=$1", id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	check := jsonString(t, f.call("root", "default", "POST", path+"/operations", map[string]any{"action": "image_check"}, 202), "id")
	var payload []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE id=$1", check).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	data, err := unseal(f.s.Key, payload)
	var request worker.Request
	if err != nil || json.Unmarshal(data, &request) != nil || request.Kind != "job" || request.JobService != "task" || request.Job != nil || request.PullLatest || request.Compose != "" {
		t.Fatal("metadata request changed execution inputs", request.Action, request.Kind, err)
	}
	var stopped bool
	var pending *time.Time
	if err := f.s.DB.QueryRow(context.Background(), "SELECT manually_stopped,next_job_at,job_pending_at FROM workloads WHERE id=$1", id).Scan(&stopped, &after, &pending); err != nil || !stopped || !before.Equal(after) || pending != nil {
		t.Fatal("checking changed schedule or stop intent", stopped, before, after, pending, err)
	}
	result, _ := json.Marshal(worker.Operation{ImageCheck: &worker.ImageCheck{CheckedAt: time.Now().UTC(), Images: []worker.ImageStatus{{Service: "task", Reference: "busybox:1.37", Status: "unpublished"}}}})
	f.sql("UPDATE operations SET state='succeeded',result=$2,updated_at=now() WHERE id=$1", check, result)
	var response struct{ Stale bool }
	if err := json.Unmarshal(f.call("root", "default", "GET", path+"/images", nil, 200).Body.Bytes(), &response); err != nil || response.Stale {
		t.Fatal("fresh job check unavailable", response, err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,updated_at) VALUES($1,$2,'deploy','failed',$3,now()+interval '1 second')`, resourceID(), id, []byte{})
	if err := json.Unmarshal(f.call("root", "default", "GET", path+"/images", nil, 200).Body.Bytes(), &response); err != nil || !response.Stale {
		t.Fatal("failed job run did not invalidate old images", response, err)
	}
	f.call("root", "default", "POST", path+"/operations", map[string]any{"action": "deploy", "pull_latest": true}, 400)
	f.sql(`UPDATE workloads SET manually_stopped=false,next_image_check_at=now()-interval '1 hour' WHERE id=$1`, id)
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES($1,$2,'deploy','succeeded',$3)`, resourceID(), id, []byte{})
	if err := f.s.scheduleImageChecks(context.Background()); err != nil {
		t.Fatal(err)
	}
	var checks int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM operations WHERE workload_id=$1 AND action='image_check'", id).Scan(&checks); err != nil || checks != 2 {
		t.Fatal("compatible retained job was not polled", checks, err)
	}
	if err := f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE workload_id=$1 AND action='image_check' AND state='queued'", id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	data, err = unseal(f.s.Key, payload)
	if err != nil || json.Unmarshal(data, &request) != nil || request.Kind != "job" || request.JobService != "task" || request.Action != "image_check" || request.Job != nil || request.Compose != "" {
		t.Fatal("polling queued execution instead of metadata", err)
	}
	f.sql(`UPDATE operations SET state='failed' WHERE workload_id=$1 AND state='queued'`, id)
	f.sql(`UPDATE workloads SET next_image_check_at=now()-interval '1 hour' WHERE id=$1`, id)
	f.sql(`UPDATE nodes SET capabilities='{"image_lifecycle":true,"job_runs":true}' WHERE id='job-images-node'`)
	if err := f.s.scheduleImageChecks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM operations WHERE workload_id=$1 AND action='image_check'", id).Scan(&checks); err != nil || checks != 2 {
		t.Fatal("old worker received a job metadata operation", checks, err)
	}
}

func TestMergedJobImageChecksUseUnifiedOperatePermissionAndTokenScope(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('scoped-job-node','Node','localhost',22,'',$1,'ready','{"image_lifecycle":true,"job_image_lifecycle":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('scoped-job-project','Project','default')`)
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Job", NodeID: "scoped-job-node", ProjectID: "scoped-job-project", Kind: "job", JobService: "task", Compose: "services:\n  task:\n    image: busybox:1.37\n    command: ['true']\n"}, 201), "id")
	viewer := f.user("scoped-job-viewer", "scoped-job-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','scoped-job-viewer','member','[{"scope":"project","scope_id":"scoped-job-project","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", "/api/workloads/"+id+"/images", nil, 200)
	f.call(viewer, "default", "POST", "/api/workloads/"+id+"/operations", map[string]any{"action": "image_check"}, 403)
	owner := f.user("scoped-job-owner", "scoped-job-owner@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','scoped-job-owner','owner')`)
	_, read := mintPublic(t, f, owner, "user", "workloads:read")
	_, operate := mintPublic(t, f, owner, "user", "workloads:operate")
	path := "/api/v1/workloads/" + id + "/operations"
	publicCall(t, f, read, "default", "POST", path, apiOperationInput{Action: "image_check"}, 403)
	publicCall(t, f, operate, "default", "POST", path, apiOperationInput{Action: "image_check"}, 202)
	var checks, executions int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FILTER(WHERE action='image_check'),count(*) FILTER(WHERE action='deploy') FROM operations WHERE workload_id=$1`, id).Scan(&checks, &executions); err != nil || checks != 1 || executions != 0 {
		t.Fatal("scoped metadata check queued execution or duplicated work", checks, executions, err)
	}
}
