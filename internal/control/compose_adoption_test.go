package control

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestComposeAdoptionBulkRecoveryAndPendingIsolation(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('adoption-node','Node','localhost',22,'',$1,'ready','{"adoption":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('adoption-project','Adopted','default')`)
	base := "/api/nodes/adoption-node/adoptions"
	owned := map[string]worker.AdoptionRequest{}
	failFirst, failSecond := true, true
	calls := 0
	f.s.workerRPC = func(_ context.Context, _ string, rpc worker.RPCRequest) (worker.RPCResponse, error) {
		calls++
		switch rpc.Method {
		case "compose_discover":
			return worker.RPCResponse{ComposeProjects: []worker.ComposeProject{{Project: "legacy-one", Services: []string{"app"}, Containers: 1, Blockers: []string{}}}}, nil
		case "compose_plan":
			return worker.RPCResponse{Adoption: &worker.AdoptionPlan{Project: rpc.Adoption.Project, Fingerprint: strings.Repeat("a", 64), Configuration: worker.ImportedCompose{Compose: `{"services":{"app":{"image":"${COVE_IMPORT_001}"}}}`, Environment: map[string]string{"COVE_IMPORT_001": "private-adopted-image"}}}}, nil
		case "compose_adoption_status":
			if entry, ok := owned[rpc.ID]; ok {
				return worker.RPCResponse{Adopted: &entry}, nil
			}
			return worker.RPCResponse{}, nil
		case "compose_adopt":
			if rpc.Adoption.Project == "legacy-one" && failFirst {
				failFirst = false
				owned[rpc.Adoption.Workload] = *rpc.Adoption
				return worker.RPCResponse{}, errors.New("lost reply after durable registration")
			}
			if rpc.Adoption.Project == "legacy-two" && failSecond {
				failSecond = false
				return worker.RPCResponse{}, errors.New("source changed before registration")
			}
			owned[rpc.Adoption.Workload] = *rpc.Adoption
			return worker.RPCResponse{}, nil
		default:
			t.Fatal("adoption invoked deployment or an unrelated mutation", rpc.Method)
		}
		return worker.RPCResponse{}, nil
	}
	f.call("root", "default", "POST", base+"/discover", nil, 200)
	input := map[string]any{"project_id": "adoption-project", "environment_id": "", "projects": []any{map[string]string{"project": "legacy-one", "key": "one", "name": "One"}, map[string]string{"project": "legacy-two", "key": "two", "name": "Two"}}}
	response := f.call("root", "default", "POST", base, input, 201)
	if strings.Contains(response.Body.String(), "private-adopted-image") || strings.Contains(response.Body.String(), strings.Repeat("a", 64)) {
		t.Fatal("adoption review exposed private snapshot data")
	}
	id := jsonString(t, response, "id")
	var reopened struct {
		Changes []configurationChange `json:"changes"`
	}
	if json.Unmarshal(f.call("root", "default", "GET", base+"/"+id, nil, 200).Body.Bytes(), &reopened) != nil || len(reopened.Changes) != 2 {
		t.Fatal("reopened adoption lost the reviewed canonical diff")
	}
	confirm := map[string]bool{"acknowledge": true, "manager_paused": true}
	f.call("root", "default", "POST", base+"/"+id+"/adopt", map[string]bool{"acknowledge": true}, 400)
	f.call("root", "default", "POST", base+"/"+id+"/adopt", confirm, 502)
	var resources []string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT resource_ids FROM compose_adoptions WHERE id=$1`, id).Scan(&resources); err != nil || len(resources) != 2 {
		t.Fatal("adoption reservation lost", err)
	}
	for _, resource := range resources {
		f.call("root", "default", "POST", "/api/workloads/"+resource+"/operations", apiOperationInput{Action: "stop"}, 409)
		view := f.call("root", "default", "GET", "/api/workloads/"+resource, nil, 200)
		var workload workloadView
		if json.Unmarshal(view.Body.Bytes(), &workload) != nil || workload.PendingAdoptionID != id || len(workload.Permissions) != 1 || workload.Permissions[0] != "read" {
			t.Fatal("pending resource is editable", view.Body.String())
		}
		if _, err := f.s.DB.Exec(context.Background(), `UPDATE workloads SET name='not allowed',configuration=configuration WHERE id=$1`, resource); err == nil {
			t.Fatal("direct update bypassed pending adoption guard")
		}
		if _, err := f.s.DB.Exec(context.Background(), `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'stop',$3)`, resourceID(), resource, []byte{}); err == nil {
			t.Fatal("queued operation bypassed pending adoption guard")
		}
	}
	f.call("root", "default", "DELETE", base+"/"+id, nil, 409)
	f.call("root", "default", "POST", base+"/"+id+"/adopt", confirm, 502)
	f.call("root", "default", "POST", base+"/"+id+"/refresh", nil, 200)
	completed := f.call("root", "default", "POST", base+"/"+id+"/adopt", confirm, 200)
	if jsonString(t, completed, "status") != "complete" {
		t.Fatal("adoption did not complete")
	}
	before := calls
	f.call("root", "default", "POST", base+"/"+id+"/adopt", confirm, 200)
	if calls != before {
		t.Fatal("completed adoption executed again")
	}
	for _, resource := range resources {
		var view workloadView
		if json.Unmarshal(f.call("root", "default", "GET", "/api/workloads/"+resource, nil, 200).Body.Bytes(), &view) != nil || view.PendingAdoptionID != "" || !view.Deployed || !slices.Contains(view.Permissions, "read") || !slices.Contains(view.Permissions, "operate") || !slices.Contains(view.Permissions, "edit") || !slices.Contains(view.Permissions, "read_secrets") {
			t.Fatal("adopted runtime not recognized")
		}
	}
	var operations int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM operations WHERE action='adopt' AND state='succeeded'`).Scan(&operations); err != nil || operations != 2 {
		t.Fatal("truthful ownership operation history missing", err)
	}
}

func TestAdoptionRequiresInstanceAdminAndPersonalToken(t *testing.T) {
	f, _, cookie := publicFixture(t)
	f.sql(`UPDATE nodes SET capabilities='{"adoption":true}' WHERE id='variable-node'`)
	path := "/api/nodes/variable-node/adoptions"
	f.call(cookie, "default", "POST", path+"/discover", nil, 403)
	f.call(cookie, "default", "POST", "/api/access-tokens", map[string]any{"name": "Denied", "scope": "user", "scopes": []string{"migration:adopt"}, "days": 1}, 403)
	f.sql(`UPDATE users SET is_admin=true WHERE id='public-owner'`)
	f.call(cookie, "default", "POST", "/api/access-tokens", map[string]any{"name": "Denied team", "scope": "team", "scopes": []string{"migration:adopt"}, "days": 1}, 403)
	mintPublic(t, f, cookie, "user", "migration:adopt")
	token := randomToken()
	f.sql(`INSERT INTO access_tokens(id,name,user_id,team_id,scopes,token_hash,expires_at) VALUES('legacy-adoption-team-token','Fixture','public-owner','default',ARRAY['migration:adopt'],$1,now()+interval '1 day')`, tokenHash(token))
	publicCall(t, f, token, "default", "POST", "/api/v1/nodes/variable-node/adoptions/discover", nil, 403)
}
