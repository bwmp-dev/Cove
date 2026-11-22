package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestBackupCalendarPolicyCapabilitiesAndSealing(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('calendar-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('calendar-project','Project','default')")
	data, _ := json.Marshal(workloadInput{Name: "Calendar", Kind: "service"})
	sealed, _ := seal(f.s.Key, data)
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('calendar-resource','calendar-node','Calendar',$1,'calendar-project','default')", sealed)
	policy := worker.BackupConfig{Repository: "local", Password: "synthetic-retention-password", KeepLast: 2, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12}
	f.call("root", "default", "PUT", "/api/workloads/calendar-resource/backup-policy", map[string]any{"backup": policy}, 409)
	f.sql(`UPDATE nodes SET capabilities='{"backup_retention":true}' WHERE id='calendar-node'`)
	f.call("root", "default", "PUT", "/api/workloads/calendar-resource/backup-policy", map[string]any{"backup": policy}, 200)
	body := f.call("root", "default", "GET", "/api/workloads/calendar-resource/backup-policy", nil, 200).Body.String()
	if strings.Contains(body, policy.Password) || !strings.Contains(body, `"keep_monthly":12`) {
		t.Fatal("calendar policy missing or secret exposed")
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='calendar-node'`)
	if f.s.prepareInfrastructure(ctx, &worker.Request{Workload: "calendar-resource", Action: "backup", Backup: &policy}) == nil {
		t.Fatal("old worker accepted calendar retention")
	}
	lifecycle := worker.Request{Workload: "calendar-resource", Action: "stop", Backup: &policy}
	if err := f.s.prepareInfrastructure(ctx, &lifecycle); err != nil {
		t.Fatal("calendar retention blocked lifecycle action", err)
	}
	if lifecycle.Backup != nil {
		t.Fatal("unrelated backup configuration was sent with lifecycle action")
	}
	b2 := connectionConfig{Name: "B2", Storage: &worker.StorageLocation{Protocol: "b2", Path: "bucket/backups", AccessKey: "synthetic-key-id", SecretKey: "synthetic-b2-key"}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/storage-locations", b2, 201), "id")
	b2.Storage.SecretKey = ""
	f.call("root", "default", "PUT", "/api/storage-locations/"+id, b2, 200)
	stored, err := f.s.readConnection(ctx, id, "default", "storage")
	if err != nil || stored.Storage.SecretKey != "synthetic-b2-key" {
		t.Fatal("B2 key not preserved", err)
	}
	if strings.Contains(f.call("root", "default", "GET", "/api/storage-locations", nil, 200).Body.String(), "synthetic-b2-key") {
		t.Fatal("B2 key exposed")
	}
	f.sql("INSERT INTO team_nodes(team_id,node_id) VALUES('default','calendar-node')")
	f.sql(`UPDATE nodes SET capabilities='{"infrastructure":true}' WHERE id='calendar-node'`)
	f.call("root", "default", "POST", "/api/storage-locations/"+id+"/test", map[string]string{"node_id": "calendar-node"}, 409)
	f.sql(`UPDATE nodes SET capabilities='{"infrastructure":true,"backblaze_b2":true}' WHERE id='calendar-node'`)
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method != "storage_test" || r.Storage == nil || r.Storage.Protocol != "b2" {
			t.Fatal("incorrect B2 test request")
		}
		return worker.RPCResponse{}, nil
	}
	f.call("root", "default", "POST", "/api/storage-locations/"+id+"/test", map[string]string{"node_id": "calendar-node"}, 200)
}
