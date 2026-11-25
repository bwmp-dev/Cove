package control

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestManagedRegistryMergeScopedAdmissionAndPortableExports(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("registry-owner", "registry-owner@example.invalid")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','registry-owner','owner')`)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('registry-node','Registry','localhost',22,'',$1,'ready','{"managed_registry":true,"file_secrets":true}')`, []byte{})
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('default','registry-node')`)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('registry-project','Registry project','default')`)
	_, read := mintPublic(t, f, owner, "user", "workloads:read")
	_, write := mintPublic(t, f, owner, "user", "workloads:write")
	_, configure := mintPublic(t, f, owner, "user", "configuration:write")
	in := managedRegistryInput{Name: "Registry", Node: "registry-node", Project: "registry-project", Hostname: "registry.example.test"}
	publicCall(t, f, read, "default", "POST", "/api/v1/managed-registries", in, 403)
	created := publicCall(t, f, write, "default", "POST", "/api/v1/managed-registries", in, 202)
	id := jsonString(t, created, "id")
	publicCall(t, f, read, "default", "GET", "/api/v1/managed-registries", nil, 200)
	policy := map[string]any{"retention": map[string]int{"keep_per_repository": 10, "minimum_age_days": 30}}
	f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id=$1`, id)
	publicCall(t, f, read, "default", "PUT", "/api/v1/managed-registries/"+id, policy, 403)
	publicCall(t, f, configure, "default", "PUT", "/api/v1/managed-registries/"+id, policy, 200)
	f.call(owner, "default", "POST", "/api/projects/registry-project/clone", map[string]any{"name": "Registry copy", "include_secrets": true}, 409)
	view := f.call(owner, "default", "GET", "/api/workloads/"+id, nil, 200)
	var workload struct {
		Permissions []string `json:"permissions"`
	}
	if json.Unmarshal(view.Body.Bytes(), &workload) != nil || strings.Contains(strings.Join(workload.Permissions, ","), "edit") || strings.Contains(strings.Join(workload.Permissions, ","), "operate") {
		t.Fatal("managed resource acquired ordinary editor or operator controls")
	}
	tx, err := f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := f.s.readProjectConfiguration(t.Context(), tx, "default", "registry-project")
	_ = tx.Rollback(t.Context())
	if err != nil || len(manifest.Resources) != 0 {
		t.Fatal("registry infrastructure entered a portable service declaration", err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES('registry-maintenance',$1,'registry_maintenance',$2)`, id, []byte{})
	now, until := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	f.sql(`INSERT INTO deployment_policies(team_id,approval,freeze_from,freeze_until) VALUES('default',true,$1,$2)`, now, until)
	admitted, err := f.s.admitDeployment(t.Context(), "registry-maintenance")
	if err != nil || admitted {
		t.Fatal("registry maintenance bypassed the freeze", err)
	}
	f.sql(`UPDATE deployment_policies SET freeze_from=NULL,freeze_until=NULL WHERE team_id='default'`)
	admitted, err = f.s.admitDeployment(t.Context(), "registry-maintenance")
	if err != nil || admitted {
		t.Fatal("registry maintenance bypassed approval", err)
	}
	f.call(owner, "default", "POST", "/api/operations/registry-maintenance/approve", nil, 200)
	admitted, err = f.s.admitDeployment(t.Context(), "registry-maintenance")
	if err != nil || !admitted {
		t.Fatal("approved maintenance did not enter normal admission", err)
	}
}
