package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestResourceDefaultsScopeDeploymentAndAdvisoryPlacement(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('defaults-project','Project','default')`)
	f.sql(`INSERT INTO environments(id,name,project_id,team_id) VALUES('defaults-environment','Prod','defaults-project','default')`)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('defaults-node','Fresh','localhost',22,'',$1,'ready','{"resource_defaults":true}')`, []byte{})
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('default','defaults-node')`)
	configuration, _ := seal(f.s.Key, []byte(`{"name":"App","kind":"service"}`))
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,environment_id,project_id,team_id) VALUES('defaults-resource','defaults-node','App',$1,'defaults-environment','defaults-project','default')`, configuration)
	user := f.user("defaults-editor", "defaults-editor@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','defaults-editor','member','[{"scope":"environment","scope_id":"defaults-environment","role":"editor"}]')`)
	f.call(user, "default", "PUT", "/api/resource-defaults", worker.ServiceLimits{CPUs: 1}, 403)
	f.call("root", "default", "PUT", "/api/resource-defaults", worker.ServiceLimits{CPUs: 1, MemoryMB: 128}, 200)
	f.call(user, "default", "PUT", "/api/environments/defaults-environment/resource-defaults", worker.ServiceLimits{CPUs: 0.5}, 200)
	response := f.call(user, "default", "GET", "/api/environments/defaults-environment/resource-defaults", nil, 200)
	var defaults struct{ Effective worker.ServiceLimits }
	if json.Unmarshal(response.Body.Bytes(), &defaults) != nil || defaults.Effective.CPUs != 0.5 || defaults.Effective.MemoryMB != 128 {
		t.Fatal("environment did not inherit the team memory default", response.Body.String())
	}
	request := worker.Request{Action: "deploy", Workload: "defaults-resource"}
	if err := f.s.prepareResourceDefaults(context.Background(), &request); err != nil || request.DefaultLimits == nil || *request.DefaultLimits != defaults.Effective {
		t.Fatal("deployment missed inherited defaults", request.DefaultLimits, err)
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='defaults-node'`)
	if err := f.s.prepareResourceDefaults(context.Background(), &request); err == nil {
		t.Fatal("old worker silently ignored defaults")
	}
	request.Action, request.DefaultLimits = "rollback", nil
	if err := f.s.prepareResourceDefaults(context.Background(), &request); err != nil || request.DefaultLimits != nil {
		t.Fatal("rollback acquired current defaults", err)
	}
	for _, in := range []worker.ServiceLimits{{CPUs: -1}, {CPUs: 0.001}, {MemoryMB: 1}} {
		f.call("root", "default", "PUT", "/api/resource-defaults", in, 400)
	}
	metrics, _ := json.Marshal(map[string]any{"metrics": worker.NodeMetrics{SampledAt: time.Now(), CPUs: 4, CPUPercent: 25, MemoryTotal: 1 << 30, MemoryAvailable: 512 << 20}})
	f.sql(`UPDATE nodes SET capabilities=$1 WHERE id='defaults-node'`, string(metrics))
	path := "/api/placement-suggestions?project_id=defaults-project&environment_id=defaults-environment&cpus=1&memory_mb=64"
	response = f.call(user, "default", "GET", path, nil, 200)
	if !strings.Contains(response.Body.String(), `"fits":true`) || !strings.Contains(response.Body.String(), `"available_cpus":3`) {
		t.Fatal("fresh free capacity was not suggested", response.Body.String())
	}
	f.call(user, "default", "GET", "/api/placement-suggestions?project_id=defaults-project", nil, 403)
	f.call("root", "default", "GET", "/api/placement-suggestions?project_id=defaults-project&cpus=NaN", nil, 400)
	f.sql(`UPDATE nodes SET state='unreachable' WHERE id='defaults-node'`)
	response = f.call(user, "default", "GET", path, nil, 200)
	if !strings.Contains(response.Body.String(), `"fits":null`) || !strings.Contains(response.Body.String(), `"available_cpus":null`) {
		t.Fatal("unreachable capacity appeared usable", response.Body.String())
	}
}
