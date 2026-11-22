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
