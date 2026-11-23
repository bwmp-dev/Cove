package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"strings"
	"testing"
)

func TestFilesystemSnapshotPolicyPreviewAndWorkerCapability(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('snapshot-node','Node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('snapshot-project','Project','default')`)
	data, _ := json.Marshal(workloadInput{Name: "Snapshot", Kind: "service"})
	sealed, _ := seal(f.s.Key, data)
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('snapshot-resource','snapshot-node','Snapshot',$1,'snapshot-project','default')`, sealed)
	policy := worker.BackupConfig{Repository: "local", Password: "synthetic-snapshot-password", KeepLast: 2, VolumeSnapshot: &worker.VolumeSnapshotConfig{MaxBytes: 256 << 20}}
	f.call("root", "default", "PUT", "/api/workloads/snapshot-resource/backup-policy", map[string]any{"backup": policy}, 409)
	f.call("root", "default", "GET", "/api/workloads/snapshot-resource/backups/snapshot-preview", nil, 409)
	if f.s.prepareInfrastructure(context.Background(), &worker.Request{Workload: "snapshot-resource", Action: "backup", Backup: &policy}) == nil {
		t.Fatal("old worker accepted filesystem snapshot policy")
	}
	f.sql(`UPDATE nodes SET capabilities='{"filesystem_snapshots":true}' WHERE id='snapshot-node'`)
	f.call("root", "default", "PUT", "/api/workloads/snapshot-resource/backup-policy", map[string]any{"backup": policy}, 200)
	body := f.call("root", "default", "GET", "/api/workloads/snapshot-resource/backup-policy", nil, 200).Body.String()
	if strings.Contains(body, policy.Password) || !strings.Contains(body, `"max_bytes":268435456`) {
		t.Fatal("policy secret or options changed", body)
	}
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "snapshot-node" || r.Method != "snapshot_preview" || r.ID != "snapshot-resource" || r.Snapshot == nil || r.Snapshot.MaxBytes != 256<<20 {
			t.Fatal("incorrect snapshot preview request", r)
		}
		return worker.RPCResponse{Snapshot: &worker.SnapshotPreview{Available: false, Reason: "Unsupported filesystem", Mounts: []worker.SnapshotCapability{{Mount: "data", Filesystem: "overlay", Available: false, Reason: "Use stopped backup"}}}}, nil
	}
	viewer := f.user("snapshot-viewer", "viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','snapshot-viewer','member','[{"scope":"project","scope_id":"snapshot-project","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", "/api/workloads/snapshot-resource/backups/snapshot-preview?max_bytes=268435456", nil, 403)
	manager := f.user("snapshot-manager", "manager@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','snapshot-manager','member','[{"scope":"project","scope_id":"snapshot-project","role":"operator"}]')`)
	f.call(manager, "default", "GET", "/api/workloads/snapshot-resource/backups/snapshot-preview?max_bytes=268435456", nil, 200)
	f.call(manager, "default", "PUT", "/api/workloads/snapshot-resource/backup-policy", map[string]any{"backup": policy}, 403)
	f.call(viewer, "default", "PUT", "/api/workloads/snapshot-resource/backup-policy", map[string]any{"backup": policy}, 403)
	stranger := f.user("snapshot-stranger", "stranger@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','snapshot-stranger','member')`)
	f.call(stranger, "default", "GET", "/api/workloads/snapshot-resource/backups/snapshot-preview?max_bytes=268435456", nil, 403)
	f.call("root", "default", "GET", "/api/workloads/snapshot-resource/backups/snapshot-preview?max_bytes=268435456", nil, 200)
	for _, size := range []string{"-1", "0", "1", "68719476737", "bad"} {
		f.call("root", "default", "GET", "/api/workloads/snapshot-resource/backups/snapshot-preview?max_bytes="+size, nil, 400)
	}
	policy.VolumeSnapshot.MaxBytes = 1
	f.call("root", "default", "PUT", "/api/workloads/snapshot-resource/backup-policy", map[string]any{"backup": policy}, 400)
	policy.VolumeSnapshot.MaxBytes = 256 << 20
	policy.Database = &worker.DatabaseBackup{Engine: "postgresql", Service: "db", Name: "app"}
	f.sql(`UPDATE nodes SET capabilities='{"filesystem_snapshots":true,"logical_backups":true}' WHERE id='snapshot-node'`)
	f.call("root", "default", "PUT", "/api/workloads/snapshot-resource/backup-policy", map[string]any{"backup": policy}, 400)
}
