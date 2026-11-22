package control

import (
	"context"
	"cove/internal/worker"
	"testing"
)

func TestScheduledRestoreDrillsAreDurableAndDoNotOverlap(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('drill-node','Node','localhost',22,'',$1,'ready','{"restore_drills":true}')`, []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('drill-project','Project','default')")
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Drill", NodeID: "drill-node", ProjectID: "drill-project", Compose: "services:\n  app:\n    image: busybox:1.37\n"}, 201), "id")
	policy := worker.BackupConfig{Repository: "local", Password: "fixture-backup-password", KeepLast: 3, DrillIntervalHours: 24}
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/backup-policy", map[string]any{"backup": policy}, 200)
	f.sql("UPDATE workloads SET next_drill_at=now()-interval '1 hour' WHERE id=$1", id)
	if err := f.s.scheduleRestoreDrills(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.s.DB.QueryRow(ctx, "SELECT count(*) FROM operations WHERE workload_id=$1", id).Scan(&count); err != nil || count != 0 {
		t.Fatal("drilled without a backup", count, err)
	}
	f.sql("INSERT INTO operations(id,workload_id,action,state,payload) VALUES('drill-backup',$1,'backup','succeeded',$2)", id, []byte{})
	f.sql("UPDATE workloads SET next_drill_at=now()-interval '1 hour' WHERE id=$1", id)
	if err := f.s.scheduleRestoreDrills(ctx); err != nil {
		t.Fatal(err)
	}
	f.sql("UPDATE workloads SET next_drill_at=now()-interval '1 hour' WHERE id=$1", id)
	if err := f.s.scheduleRestoreDrills(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(ctx, "SELECT count(*) FROM operations WHERE workload_id=$1 AND action='drill'", id).Scan(&count); err != nil || count != 1 {
		t.Fatal("overlapping drills", count, err)
	}
	viewer := f.user("drill-viewer", "drill-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','drill-viewer','member','[{"scope":"team","role":"operator"}]')`)
	f.call(viewer, "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "drill", "backup_id": "drill-backup"}, 403)
	f.sql("UPDATE operations SET state='succeeded' WHERE workload_id=$1", id)
	f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "drill", "backup_id": "drill-backup"}, 202)
}
