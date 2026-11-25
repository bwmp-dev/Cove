package control

import (
	"context"
	"testing"
)

func TestBuildpacksMergeMaintenanceQuotaSlots(t *testing.T) {
	for _, action := range []string{"pitr_configure", "move", "registry_maintenance"} {
		t.Run(action, func(t *testing.T) {
			f, id := buildpacksFixture(t)
			f.sql(`INSERT INTO operations(id,workload_id,action,payload,admitted_at) VALUES('maintenance',$1,$2,$3,now())`, id, action, []byte{})
			f.sql(`INSERT INTO workloads(id,node_id,name,configuration,team_id,project_id) SELECT 'cnb-next','cnb-other','next',configuration,team_id,project_id FROM workloads WHERE id=$1`, id)
			f.sql(`INSERT INTO operations(id,workload_id,action,payload,execution_node_id) VALUES('cnb-next','cnb-next','build',$1,'cnb-other')`, []byte{})
			tx, err := f.s.DB.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err = reserveBuildpacks(context.Background(), tx, "cnb-next", "cnb-other"); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.call("root", "default", "PUT", "/api/teams/default/quota", teamQuota{Deployments: 1}, 200)
			if ok, _, err := f.s.admitBuildpacks(context.Background(), "cnb-next", "cnb-other"); err != nil || ok {
				t.Fatal("CNB ignored active maintenance slot", ok, err)
			}
		})
	}
}
