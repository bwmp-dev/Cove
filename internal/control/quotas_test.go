package control

import (
	"context"
	"cove/internal/worker"
	"sync"
	"sync/atomic"
	"testing"
)

func TestTeamQuotasCoverCreateEditCloneAndConcurrentAdmission(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('quota-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('quota-project','Project','default')")
	quota := teamQuota{Resources: 2, CPUs: 2, MemoryMB: 256, Deployments: 1}
	f.call("root", "default", "PUT", "/api/teams/default/quota", quota, 200)
	input := workloadInput{Name: "App", NodeID: "quota-node", ProjectID: "quota-project", Compose: "services:\n  app:\n    image: busybox:1.37\n", Limits: map[string]worker.ServiceLimits{"app": {CPUs: 1, MemoryMB: 128}}}
	first := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	second := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	f.call("root", "default", "POST", "/api/workloads", input, 409)
	f.call("root", "default", "POST", "/api/workloads/"+first+"/clone", map[string]any{"name": "Clone", "include_secrets": false}, 409)
	input.ProjectID = ""
	input.Limits["app"] = worker.ServiceLimits{CPUs: 2, MemoryMB: 128}
	f.call("root", "default", "PUT", "/api/workloads/"+first, input, 409)
	input.Limits["app"] = worker.ServiceLimits{CPUs: 1, MemoryMB: 256}
	f.call("root", "default", "PUT", "/api/workloads/"+first, input, 409)
	f.call("root", "default", "PUT", "/api/teams/default/quota", teamQuota{Resources: 1}, 409)
	manager := f.user("quota-manager", "quota-manager@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','quota-manager','admin','[]')`)
	f.call(manager, "default", "GET", "/api/teams/default/quota", nil, 200)
	f.call(manager, "default", "PUT", "/api/teams/default/quota", teamQuota{}, 403)
	f.sql("INSERT INTO operations(id,workload_id,action,payload) VALUES('quota-a',$1,'deploy',$3),('quota-b',$2,'deploy',$3)", first, second, []byte{})
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for _, id := range []string{"quota-a", "quota-b"} {
		wg.Go(func() {
			ok, err := f.s.admitDeployment(context.Background(), id)
			if err != nil {
				t.Error(err)
			}
			if ok {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatal("over-admitted", admitted.Load())
	}
	f.sql("UPDATE operations SET state='succeeded' WHERE admitted_at IS NOT NULL")
	var waiting string
	if err := f.s.DB.QueryRow(context.Background(), "SELECT id FROM operations WHERE admitted_at IS NULL AND workload_id IN ($1,$2)", first, second).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.s.admitDeployment(context.Background(), waiting); err != nil || !ok {
		t.Fatal("slot not released", ok, err)
	}
}
