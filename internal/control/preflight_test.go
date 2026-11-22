package control

import (
	"context"
	"testing"
)

func TestPreflightRequiresEditorAndWorkerCapability(t *testing.T) {
	f := newAccountFixture(t)
	viewer := f.user("preflight-viewer", "preflight-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','preflight-viewer','member','[{"scope":"team","role":"viewer"}]')`)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('preflight-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('preflight-project','Project','default')")
	config := workloadInput{Name: "Preflight", NodeID: "preflight-node", ProjectID: "preflight-project", Compose: "services:\n  app:\n    image: busybox:1.37\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	f.call(viewer, "default", "POST", "/api/workloads/"+id+"/preflight", nil, 403)
	f.call("root", "default", "POST", "/api/workloads/"+id+"/preflight", nil, 409)
	var count int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM operations WHERE workload_id=$1", id).Scan(&count); err != nil || count != 0 {
		t.Fatal("preflight queued an operation", count, err)
	}
}
