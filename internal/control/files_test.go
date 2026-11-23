package control

import (
	"context"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestFileBrowserPermissionsBoundsAndAudit(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('files-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('files-project','Project','default')")
	sealed, _ := seal(f.s.Key, []byte(`{}`))
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('files-resource','files-node','Resource',$1,'files-project','default')", sealed)
	owner := f.user("files-owner", "files-owner@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','files-owner','owner','[]')`)
	f.call(owner, "default", "GET", "/api/workloads/files-resource/files/targets", nil, 403)
	f.call("root", "default", "GET", "/api/workloads/files-resource/files/targets", nil, 409)
	f.sql(`UPDATE nodes SET capabilities='{"file_browser":true}' WHERE id='files-node'`)
	calls := 0
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		calls++
		if node != "files-node" || r.Method != "files" || r.ID != "files-resource" || r.Files == nil {
			t.Fatal("incorrect file RPC")
		}
		return worker.RPCResponse{Files: &worker.FileResponse{Data: []byte("sample"), Revision: strings.Repeat("a", 64), MaxBytes: worker.MaxFileBytes}}, nil
	}
	path := "/api/workloads/files-resource/files/content?kind=container&target=aaaaaaaaaaaa&path=/data/config"
	response := f.call("root", "default", "GET", path, nil, 200)
	if response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), `"data":"c2FtcGxl"`) {
		t.Fatal("content response is cacheable or invalid")
	}
	download := f.call("root", "default", "GET", "/api/workloads/files-resource/files/download?kind=container&target=aaaaaaaaaaaa&path=/data/config", nil, 200)
	if download.Body.String() != "sample" || download.Header().Get("Content-Disposition") == "" {
		t.Fatal("file download failed")
	}
	var count int
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE subject='files-resource' AND action LIKE 'GET %/files/%' AND status=200`).Scan(&count); err != nil || count != 2 {
		t.Fatal("reads not audited", count, err)
	}
	f.call("root", "default", "GET", "/api/workloads/files-resource/files/content?kind=container&target=aaaaaaaaaaaa&path=/../secret", nil, 400)
	if calls != 2 {
		t.Fatal("unsafe path reached worker")
	}
	data := []byte("changed")
	in := worker.FileRequest{Kind: "container", Target: "aaaaaaaaaaaa", Path: "/data/config", Data: &data, Revision: strings.Repeat("a", 64)}
	f.call(owner, "default", "PUT", "/api/workloads/files-resource/files/content", in, 403)
	f.call("root", "default", "PUT", "/api/workloads/files-resource/files/content", in, 200)
	large := make([]byte, worker.MaxFileBytes+1)
	in.Data = &large
	f.call("root", "default", "POST", "/api/workloads/files-resource/files/upload", in, 400)
	f.sql("INSERT INTO operations(id,workload_id,action,payload) VALUES('files-active','files-resource','deploy',$1)", sealed)
	f.call("root", "default", "GET", "/api/workloads/files-resource/files/targets", nil, 409)
	f.sql("INSERT INTO teams(id,name) VALUES('files-other','Other')")
	f.call("root", "files-other", "GET", "/api/workloads/files-resource/files/targets", nil, 404)
}
