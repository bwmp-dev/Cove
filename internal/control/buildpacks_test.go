package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func buildpacksFixture(t *testing.T) (*accountFixture, string) {
	t.Helper()
	f := newAccountFixture(t)
	probe := worker.Probe{OS: "linux", Arch: "amd64", BuildPublish: true, BuildRecipes: true, Buildpacks: true, BuildpacksCapacity: &worker.BuildpacksCapacity{MemoryAvailable: 5 << 30, DiskAvailable: 24 << 30, StateAvailable: 10 << 30}}
	data, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('cnb-node','CNB node','localhost',22,'',$1,'ready',$2),('cnb-other','Other node','localhost',22,'',$1,'ready',$2)`, []byte{}, data)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('cnb-project','CNB','default')`)
	input := workloadInput{Name: "CNB app", NodeID: "cnb-node", ProjectID: "cnb-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n    build: .\n", Source: &worker.GitSource{Repository: "https://git.example.test/app.git", Branch: "main", InlineCompose: true}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	return f, id
}

func TestBuildpacksReservationQuotasAndRecovery(t *testing.T) {
	f, id := buildpacksFixture(t)
	ctx := context.Background()
	for _, item := range []struct{ operation, resource, node string }{{"cnb-first", id, "cnb-node"}, {"cnb-second", "cnb-resource-two", "cnb-node"}, {"cnb-third", "cnb-resource-three", "cnb-other"}, {"cnb-deploy", "cnb-resource-four", "cnb-other"}} {
		if item.resource != id {
			f.sql(`INSERT INTO workloads(id,node_id,name,configuration,team_id,project_id) SELECT $1,$2,$1,configuration,team_id,project_id FROM workloads WHERE id=$3`, item.resource, item.node, id)
		}
		action := "build"
		if item.operation == "cnb-deploy" {
			action = "deploy"
		}
		f.sql(`INSERT INTO operations(id,workload_id,action,payload,execution_node_id) VALUES($1,$2,$3,$4,$5)`, item.operation, item.resource, action, []byte{}, item.node)
		if action == "build" {
			tx, err := f.s.DB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := reserveBuildpacks(ctx, tx, item.operation, item.node); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.call("root", "default", "PUT", "/api/teams/default/quota", teamQuota{CPUs: 4, MemoryMB: 3000, Deployments: 1}, 200)
	if ok, _, err := f.s.admitBuildpacks(ctx, "cnb-first", "cnb-node"); err != nil || ok {
		t.Fatal("insufficient team memory was accepted", ok, err)
	}
	f.call("root", "default", "PUT", "/api/teams/default/quota", teamQuota{CPUs: 4, MemoryMB: 6000, Deployments: 1}, 200)
	ok, first, err := f.s.admitBuildpacks(ctx, "cnb-first", "cnb-node")
	if err != nil || !ok || first == nil || first.PhaseMemoryBytes != 3<<30 {
		t.Fatal(ok, first, err)
	}
	for _, candidate := range []struct{ id, node string }{{"cnb-second", "cnb-node"}, {"cnb-third", "cnb-other"}} {
		if ok, _, err := f.s.admitBuildpacks(ctx, candidate.id, candidate.node); err != nil || ok {
			t.Fatal("serialized/budgeted CNB build over-admitted", candidate, ok, err)
		}
	}
	if ok, err := f.s.admitDeployment(ctx, "cnb-deploy"); err != nil || ok {
		t.Fatal("deployment ignored active CNB slot", ok, err)
	}
	f.call("root", "default", "PUT", "/api/teams/default/quota", teamQuota{CPUs: 2}, 409)
	f.call("root", "default", "PUT", "/api/teams/default/quota", teamQuota{MemoryMB: 3000}, 409)
	if _, err := f.s.DB.Exec(ctx, `UPDATE workloads SET configured_memory_mb=4000 WHERE id=$1`, id); err == nil {
		t.Fatal("configured workload usage ignored reserved CNB memory")
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='cnb-node'`)
	ok, recovered, err := f.s.admitBuildpacks(ctx, "cnb-first", "cnb-node")
	if err != nil || !ok || !reflect.DeepEqual(recovered, first) {
		t.Fatal("recovery changed an admitted request", ok, recovered, err)
	}
	if _, _, err := f.s.admitBuildpacks(ctx, "cnb-first", "cnb-other"); err == nil {
		t.Fatal("reservation moved to a different node")
	}
	f.sql(`UPDATE operations SET cancel_requested=true WHERE id='cnb-first'`)
	if ok, admission, err := f.s.admitBuildpacks(ctx, "cnb-first", "cnb-node"); err != nil || ok || admission != nil {
		t.Fatal("cancelled admission changed", ok, admission, err)
	}
	f.sql(`UPDATE operations SET state='cancelled' WHERE id='cnb-first'`)
	if ok, _, err := f.s.admitBuildpacks(ctx, "cnb-third", "cnb-other"); err != nil || !ok {
		t.Fatal("terminal reservation was not released", ok, err)
	}
}

func TestBuildpacksExplicitRecipeAndCapacityRefusal(t *testing.T) {
	f, id := buildpacksFixture(t)
	path := "/api/workloads/" + id + "/build-recipe"
	recipe := worker.BuildRecipe{Engine: "buildpacks", Service: "app", Directory: "."}
	read := f.call("root", "default", "GET", path, nil, 200)
	for _, expected := range []string{`"engine":"compose"`, `"work_storage":"disk"`, `"work_hard_cap":false`, `"quota_cpu":2.5`, `"required_available_memory_bytes":4294967296`} {
		if !strings.Contains(read.Body.String(), expected) {
			t.Fatal("missing resource disclosure", expected, read.Body.String())
		}
	}
	f.call("root", "default", "PUT", path, recipe, 200)
	for _, candidate := range []struct {
		field string
		value uint64
	}{{"memory_available", (4 << 30) - 1}, {"disk_available", (24 << 30) - 1}, {"state_available", (10 << 30) - 1}} {
		f.sql(`UPDATE nodes SET capabilities=jsonb_set(capabilities,ARRAY['buildpacks_capacity',$1],to_jsonb($2::bigint)) WHERE id='cnb-node'`, candidate.field, candidate.value)
		f.call("root", "default", "PUT", path, recipe, 409)
		status := f.call("root", "default", "GET", path, nil, 200)
		if !strings.Contains(status.Body.String(), `"engine":"buildpacks"`) || !strings.Contains(status.Body.String(), `"supported":false`) {
			t.Fatal("saved engine silently changed or became eligible", status.Body.String())
		}
	}
	f.call("root", "default", "PUT", path, worker.BuildRecipe{Engine: "compose"}, 200)
}
