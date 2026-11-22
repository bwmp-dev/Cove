package control

import (
	"context"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestComposeBundleSealedSnapshotAccessAndExport(t *testing.T) {
	f, id := variablesFixture(t)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"compose_bundles":true}'::jsonb WHERE id='variable-node'`)
	bundle := &worker.ComposeBundle{Files: []worker.ComposeFile{{Path: "production.yaml", Content: "services:\n  app:\n    image: busybox:1.37\n"}}, Assets: map[string]string{"extra.env": "TOKEN=bundle-secret\n"}, Profiles: []string{"production"}}
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/compose-bundle", map[string]any{"bundle": bundle}, 200)
	metadata := f.call("root", "default", "GET", "/api/workloads/"+id+"/compose-bundle", nil, 200)
	if strings.Contains(metadata.Body.String(), "bundle-secret") || !strings.Contains(metadata.Body.String(), "extra.env") {
		t.Fatal("bundle metadata exposed content or omitted file names")
	}
	exported := f.call("root", "default", "POST", "/api/workloads/"+id+"/compose-bundle/export", nil, 200)
	if !strings.Contains(exported.Body.String(), "bundle-secret") || exported.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("explicit export did not return private bundle")
	}
	var audited bool
	if err := f.s.DB.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action='Compose files exported' AND subject=$1)`, id).Scan(&audited); err != nil || !audited {
		t.Fatal("bundle export was not audited", err)
	}
	f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "deploy"}, 202)
	var sealed []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT payload FROM operations WHERE workload_id=$1`, id).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "bundle-secret") {
		t.Fatal("queued bundle stored as plaintext")
	}
	var request worker.Request
	if err := f.s.unsealJSON(sealed, &request); err != nil || request.ComposeBundle == nil || request.ComposeBundle.Assets["extra.env"] != "TOKEN=bundle-secret\n" {
		t.Fatal("queue did not freeze the selected bundle", err)
	}
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/compose-bundle", map[string]any{"bundle": nil}, 409)
	f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id=$1`, id)
	viewer := f.user("bundle-viewer", "bundle-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','bundle-viewer','member','[{"scope":"project","scope_id":"variable-project","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", "/api/workloads/"+id+"/compose-bundle", nil, 200)
	f.call(viewer, "default", "POST", "/api/workloads/"+id+"/compose-bundle/export", nil, 403)
	f.call(viewer, "default", "PUT", "/api/workloads/"+id+"/compose-bundle", map[string]any{"bundle": nil}, 403)
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='variable-node'`)
	if err := f.s.prepareInfrastructure(context.Background(), &request); err == nil || !strings.Contains(err.Error(), "upgrade and verify") {
		t.Fatal("old worker accepted bundle", err)
	}
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/compose-bundle", map[string]any{"bundle": bundle}, 409)
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/compose-bundle", map[string]any{"bundle": nil}, 200)
}
