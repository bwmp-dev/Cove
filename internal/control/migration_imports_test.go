package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestCoolifyImportDiscoveryMappingDryRunAndExplicitApply(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('import-node','Node','localhost',22,'',$1,'ready','{"imports":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('import-project','Imported','default')`)
	base := "/api/projects/import-project/imports"
	payload := map[string]any{"items": []any{map[string]any{"kind": "service", "resource": map[string]any{"uuid": "coolify-app", "name": "Imported App", "docker_compose_raw": "services: {app: {image: busybox:1.37}}", "manual_webhook_secret_github": "discard-me"}, "environment_variables": []any{map[string]any{"key": "PASSWORD", "value": "private-coolify-password"}}}}}
	response := f.call("root", "default", "POST", base+"/coolify", payload, 201)
	if strings.Contains(response.Body.String(), "private-coolify") || strings.Contains(response.Body.String(), "discard-me") || strings.Contains(response.Body.String(), "busybox") {
		t.Fatal("discovery leaked source fields")
	}
	id := jsonString(t, response, "id")
	var sealed []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT payload FROM migration_imports WHERE id=$1`, id).Scan(&sealed); err != nil || strings.Contains(string(sealed), "private-coolify") {
		t.Fatal("import not encrypted", err)
	}
	raw, err := unseal(f.s.Key, sealed)
	if err != nil || strings.Contains(string(raw), "discard-me") {
		t.Fatal("unrelated credentials retained", err)
	}
	f.call("root", "default", "GET", base, nil, 200)
	f.call("root", "default", "GET", base+"/"+id, nil, 200)
	calls := 0
	f.s.workerRPC = func(_ context.Context, node string, rpc worker.RPCRequest) (worker.RPCResponse, error) {
		calls++
		if node != "import-node" || rpc.Method != "import_compose" || rpc.Request.Environment["PASSWORD"] != "private-coolify-password" {
			t.Fatal("import changed source or invoked wrong RPC")
		}
		return worker.RPCResponse{ImportedCompose: &worker.ImportedCompose{Compose: `{"services":{"app":{"image":"${COVE_IMPORT_001}","environment":{"PASSWORD":"${COVE_IMPORT_002}"}}}}`, Environment: map[string]string{"COVE_IMPORT_001": "busybox:1.37", "COVE_IMPORT_002": "private-coolify-password"}}}, nil
	}
	mapping := importDryRunInput{Mappings: []importMapping{{SourceID: "coolify-app", Key: "imported-app", Name: "Imported App", NodeID: "import-node", Environment: "Production"}}}
	dry := f.call("root", "default", "POST", base+"/"+id+"/dry-run", mapping, 201)
	if strings.Contains(dry.Body.String(), "private-coolify") || calls != 1 {
		t.Fatal("dry-run leaked or mutated source")
	}
	draft := jsonString(t, dry, "draft_id")
	var count int
	if err = f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM workloads WHERE project_id='import-project'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("dry-run adopted resources", err)
	}
	f.call("root", "default", "POST", "/api/projects/import-project/configuration/drafts/"+draft+"/apply", nil, 200)
	var workload string
	if err = f.s.DB.QueryRow(context.Background(), `SELECT id FROM workloads WHERE project_id='import-project'`).Scan(&workload); err != nil {
		t.Fatal(err)
	}
	view := f.call("root", "default", "GET", "/api/workloads/"+workload, nil, 200)
	if strings.Contains(view.Body.String(), "private-coolify") {
		t.Fatal("saved imported config exposed secrets")
	}
	if err = f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM operations WHERE workload_id=$1`, workload).Scan(&count); err != nil || count != 0 || calls != 1 {
		t.Fatal("import started workloads", err)
	}
	f.call("root", "default", "POST", base+"/"+id+"/dry-run", mapping, 400)
	f.call("root", "default", "DELETE", base+"/"+id, nil, 200)
	f.call("root", "default", "GET", base+"/"+id, nil, 404)
}

func TestCoolifyImportBlocksMaskedMissingAndUnsupportedExports(t *testing.T) {
	var items []coolifyImportItem
	if json.Unmarshal([]byte(`[{"kind":"application","resource":{"uuid":"one","name":"App","build_pack":"nixpacks"},"environment_variables":[{"key":"PASSWORD","value":"********"}]}]`), &items) != nil {
		t.Fatal("invalid fixture")
	}
	candidates, err := discoverCoolify(items)
	if err != nil || len(candidates[0].Blockers) != 3 || len(candidates[0].Environment) != 0 {
		t.Fatal("incomplete export accepted", err)
	}
	if _, err = discoverCoolify(append(items, items...)); err == nil {
		t.Fatal("duplicate source accepted")
	}
}
