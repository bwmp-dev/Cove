package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"testing"
)

func TestWebhookCoalescingPreservesJobsAndDependencies(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('queue-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('queue-project','Project','default')")
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('queue','queue-node','Resource',$1,'queue-project','default')", []byte{})
	for i, delivery := range []string{"old", "new", "job", "dependency", "child", "other-branch"} {
		request := worker.Request{ID: resourceID(), Workload: "queue", Action: "deploy", Kind: "service", Source: &worker.GitSource{Repository: "https://github.com/example/app", Branch: "main"}}
		if delivery == "job" {
			request.Kind = "job"
		}
		if delivery == "other-branch" {
			request.Source.Branch = "other"
		}
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		encrypted, err := seal(f.s.Key, raw)
		if err != nil {
			t.Fatal(err)
		}
		f.sql("INSERT INTO webhook_deliveries(workload_id,delivery,payload,created_at) VALUES('queue',$1,$2,now()+$3*interval '1 second')", delivery, encrypted, i)
	}
	f.sql("UPDATE webhook_deliveries SET prerequisite_workload='queue',prerequisite_delivery='dependency' WHERE delivery='child'")
	tx, err := f.s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = f.s.coalesceWebhooks(ctx, tx, "queue"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(workloadInput{Name: "Resource", NodeID: "queue-node", Source: &worker.GitSource{Repository: "https://github.com/example/app", Branch: "main", ComposePath: "compose.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := seal(f.s.Key, raw)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("UPDATE workloads SET configuration=$1,webhook_secret=$2 WHERE id='queue'", encrypted, []byte{1})
	if err = f.s.dispatchWebhook(ctx, "queue"); err != nil {
		t.Fatal(err)
	}
	var dispatched string
	if err = f.s.DB.QueryRow(ctx, "SELECT delivery FROM webhook_deliveries WHERE operation_id IS NOT NULL").Scan(&dispatched); err != nil || dispatched != "new" {
		t.Fatal(dispatched, err)
	}
	var count int
	var replacement string
	if err = f.s.DB.QueryRow(ctx, "SELECT count(*) FROM webhook_deliveries WHERE superseded_by IS NOT NULL").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err = f.s.DB.QueryRow(ctx, "SELECT superseded_by FROM webhook_deliveries WHERE delivery='old'").Scan(&replacement); err != nil || replacement != "new" {
		t.Fatal(replacement, err)
	}
}
