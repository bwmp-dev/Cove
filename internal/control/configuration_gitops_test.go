package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestProjectGitOpsStagesExplicitDiffAndRevalidatesAutomaticOwner(t *testing.T) {
	f, id := variablesFixture(t)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"source_files":true}'::jsonb WHERE id='variable-node'`)
	config := workloadInput{Name: "Variables", NodeID: "variable-node", Source: &worker.GitSource{Repository: "https://fixture.invalid/app.git", Branch: "main", ComposePath: "compose.yaml"}}
	f.call("root", "default", "PUT", "/api/workloads/"+id, config, 200)
	base := "/api/projects/variable-project/configuration"
	get := f.call("root", "default", "GET", base, nil, 200)
	var current struct {
		Manifest projectConfiguration `json:"manifest"`
	}
	if json.Unmarshal(get.Body.Bytes(), &current) != nil {
		t.Fatal("invalid manifest")
	}
	current.Manifest.Resources[0].Configuration.Name = "GitOps pending"
	manifest, _ := json.Marshal(current.Manifest)
	calls := 0
	f.s.workerRPC = func(_ context.Context, _ string, rpc worker.RPCRequest) (worker.RPCResponse, error) {
		calls++
		if rpc.Method != "source_files" || rpc.Request.Source.ComposePath != "cove.yaml" || rpc.Request.ComposeBundle != nil {
			t.Fatal("GitOps attempted a deployment")
		}
		return worker.RPCResponse{SourceFiles: &worker.SourceFiles{Files: map[string]string{"cove.yaml": string(manifest)}, Revision: strings.Repeat("a", 40)}}, nil
	}
	settings := gitOpsInput{WorkloadID: id, Path: "cove.yaml"}
	f.call("root", "default", "PUT", base+"/gitops", settings, 200)
	sync := f.call("root", "default", "POST", base+"/gitops/sync", nil, 200)
	draft := jsonString(t, sync, "draft_id")
	if jsonString(t, sync, "state") != "pending" || draft == "" {
		t.Fatal("sync did not stage a pending diff")
	}
	var name string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT name FROM workloads WHERE id=$1`, id).Scan(&name); err != nil || name != "Variables" {
		t.Fatal("manual sync applied its diff", err)
	}
	f.call("root", "default", "POST", base+"/drafts/"+draft+"/apply", nil, 200)
	settings.Automatic = true
	f.call("root", "default", "PUT", base+"/gitops", settings, 409)
	owner := f.user("gitops-owner", "gitops-owner@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','gitops-owner','admin','[]')`)
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('default','variable-node')`)
	f.call(owner, "default", "PUT", base+"/gitops", settings, 200)
	current.Manifest.Resources[0].Configuration.Name = "GitOps automatic"
	manifest, _ = json.Marshal(current.Manifest)
	if err := f.s.reconcileProjectGitOps(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(context.Background(), `SELECT name FROM workloads WHERE id=$1`, id).Scan(&name); err != nil || name != "GitOps automatic" {
		t.Fatal("opted-in sync did not apply configuration", err)
	}
	if draft := jsonString(t, f.call("root", "default", "GET", base+"/gitops", nil, 200), "draft_id"); draft != "" {
		t.Fatal("applied GitOps declaration still reported a pending draft")
	}
	before := calls
	f.sql(`DELETE FROM team_members WHERE user_id='gitops-owner'`)
	f.sql(`UPDATE configuration_gitops SET next_scan_at=now()-interval '1 second'`)
	if err := f.s.reconcileProjectGitOps(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != before {
		t.Fatal("revoked owner could still fetch repository data")
	}
	status := f.call("root", "default", "GET", base+"/gitops", nil, 200)
	if !strings.Contains(status.Body.String(), "current grants") || strings.Contains(status.Body.String(), "saved-secret") {
		t.Fatal("owner failure missing or secret value returned")
	}
}
