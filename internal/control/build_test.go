package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"testing"
)

func TestBuildQueueAuthorizationAndInputs(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('build-node','Node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('build-project','Project','default')")
	config := workloadInput{Name: "App", NodeID: "build-node", ProjectID: "build-project", Kind: "service", Compose: "services:\n  app:\n    build: .\n", Routes: []worker.Route{{Domain: "build.example.com", Service: "app", Port: 80}}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	path := "/api/workloads/" + id
	input := map[string]any{"action": "build", "build": map[string]any{"repository": "team/app", "no_cache": true}}
	f.call("root", "default", "POST", path+"/operations", input, 409)
	f.sql(`UPDATE nodes SET capabilities='{"build_publish":true}' WHERE id='build-node'`)
	f.call("root", "default", "POST", path+"/operations", input, 409)
	registry := connectionConfig{Name: "Registry", Registry: &worker.Registry{Server: "registry.example.com", Username: "fixture", Password: "fixture-secret"}}
	rid := jsonString(t, f.call("root", "default", "POST", "/api/registries", registry, 201), "id")
	f.call("root", "default", "PUT", path+"/registry", map[string]string{"registry_id": rid}, 200)
	viewer := f.user("build-viewer", "build-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','build-viewer','member','[{"scope":"project","scope_id":"build-project","role":"viewer"}]')`)
	f.call(viewer, "default", "POST", path+"/operations", input, 403)
	f.call("root", "default", "POST", path+"/operations", map[string]any{"action": "build"}, 400)
	f.call("root", "default", "POST", path+"/operations", map[string]any{"action": "deploy", "build": map[string]string{"repository": "team/app"}}, 400)
	f.sql("UPDATE workloads SET manually_stopped=true WHERE id=$1", id)
	op := jsonString(t, f.call("root", "default", "POST", path+"/operations", input, 202), "id")
	var payload []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE id=$1", op).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	plain, err := unseal(f.s.Key, payload)
	if err != nil {
		t.Fatal(err)
	}
	var request worker.Request
	if err = json.Unmarshal(plain, &request); err != nil || request.Build == nil || !request.Build.NoCache || request.Compose != config.Compose || len(request.Routes) != 0 || request.GuardedDeploy != nil {
		t.Fatal(request, err)
	}
	if err = f.s.prepareInfrastructure(context.Background(), &request); err != nil || request.Registry == nil || request.Registry.Password != "fixture-secret" {
		t.Fatal(request.Registry, err)
	}
	var stopped bool
	if err = f.s.DB.QueryRow(context.Background(), "SELECT manually_stopped FROM workloads WHERE id=$1", id).Scan(&stopped); err != nil || !stopped {
		t.Fatal("build changed stopped intent", err)
	}
	f.call("root", "default", "POST", path+"/operations", input, 409)
}
