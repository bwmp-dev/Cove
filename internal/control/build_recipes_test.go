package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestBuildRecipesPermissionsFrozenSubmissionAndHistory(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('recipe-node','Recipe node','localhost',22,'',$1,'ready','{"build_publish":true,"build_recipes":true,"os":"linux","arch":"amd64"}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('recipe-project','Recipes','default')`)
	config := workloadInput{Name: "Recipe app", NodeID: "recipe-node", ProjectID: "recipe-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	path := "/api/workloads/" + id + "/build-recipe"
	initial := f.call("root", "default", "GET", path, nil, 200)
	if !strings.Contains(initial.Body.String(), `"engine":"compose"`) || !strings.Contains(initial.Body.String(), `"source_ready":false`) {
		t.Fatal(initial.Body.String())
	}
	recipe := worker.BuildRecipe{Engine: "railpack", Service: "app", Directory: ".", BuildCommand: "printf fixture-private-command", StartCommand: "sh start.sh"}
	f.call("root", "default", "PUT", path, recipe, 400)
	config.Source = &worker.GitSource{Repository: "https://git.example.test/fixture/app.git", Branch: "main", InlineCompose: true}
	update := config
	update.ProjectID = ""
	f.call("root", "default", "PUT", "/api/workloads/"+id, update, 200)
	f.sql(`UPDATE nodes SET capabilities=capabilities-'build_recipes' WHERE id='recipe-node'`)
	f.call("root", "default", "PUT", path, recipe, 409)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"build_recipes":true}' WHERE id='recipe-node'`)
	f.call("root", "default", "PUT", path, recipe, 200)
	for _, definition := range []string{"network: none", "args: {HTTP_PROXY: 'http://proxy.example.test'}"} {
		changed := config
		changed.ProjectID = ""
		changed.Compose = "services:\n  app:\n    image: busybox:1.37\n    build:\n      context: .\n      " + definition + "\n"
		f.call("root", "default", "PUT", "/api/workloads/"+id, changed, 200)
		refused := f.call("root", "default", "PUT", path, recipe, 400)
		if !strings.Contains(refused.Body.String(), "pinned Railpack frontend does not apply") {
			t.Fatal(refused.Body.String())
		}
	}
	f.call("root", "default", "PUT", "/api/workloads/"+id, update, 200)
	viewer := f.user("recipe-viewer", "recipe-viewer@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','recipe-viewer','member','[{"scope":"project","scope_id":"recipe-project","role":"viewer"}]')`)
	read := f.call(viewer, "default", "GET", path, nil, 200)
	if strings.Contains(read.Body.String(), "fixture-private-command") || !strings.Contains(read.Body.String(), `"build_command_set":true`) {
		t.Fatal("command visibility", read.Body.String())
	}
	f.call(viewer, "default", "PUT", path, worker.BuildRecipe{Engine: "compose"}, 403)
	rid := jsonString(t, f.call("root", "default", "POST", "/api/registries", connectionConfig{Name: "Recipe registry", Registry: &worker.Registry{Server: "registry.example.test", Username: "fixture", Password: "fixture-only"}}, 201), "id")
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/registry", map[string]string{"registry_id": rid}, 200)
	changedPolicy := update
	changedPolicy.Compose = "services:\n  app:\n    image: busybox:1.37\n    build:\n      context: .\n      network: none\n"
	f.call("root", "default", "PUT", "/api/workloads/"+id, changedPolicy, 200)
	f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]any{"action": "build", "build": worker.BuildOptions{Repository: "fixture/app"}}, 400)
	f.call("root", "default", "PUT", "/api/workloads/"+id, update, 200)
	op := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]any{"action": "build", "build": worker.BuildOptions{Repository: "fixture/app"}}, 202), "id")
	f.call("root", "default", "PUT", path, worker.BuildRecipe{Engine: "compose"}, 409)
	queued := f.call(viewer, "default", "GET", "/api/operations/"+op, nil, 200)
	if !strings.Contains(queued.Body.String(), `"engine":"railpack"`) || strings.Contains(queued.Body.String(), "fixture-private-command") {
		t.Fatal("queued history", queued.Body.String())
	}
	var first worker.Request
	submits := 0
	f.s.workerRPC = func(_ context.Context, node string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method == "status" {
			return worker.RPCResponse{}, errors.New("fixture operation not yet found")
		}
		if r.Method != "submit_build_recipe" || node != "recipe-node" || r.Request == nil {
			t.Fatalf("wrong submission %s %s", node, r.Method)
		}
		submits++
		if submits == 1 {
			first = *r.Request
			return worker.RPCResponse{}, errors.New("fixture lost acknowledgement")
		}
		firstWire, firstErr := json.Marshal(first)
		nextWire, nextErr := json.Marshal(*r.Request)
		if firstErr != nil || nextErr != nil || !bytes.Equal(firstWire, nextWire) {
			t.Fatal("recipe changed across uncertain resubmission")
		}
		return worker.RPCResponse{Operation: &worker.Operation{ID: op, Workload: id, Action: "build", State: "running", BuildRecipe: worker.BuildRecipeSummary(r.Request.BuildRecipe)}}, nil
	}
	if err := f.s.dispatchOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first.BuildRecipe == nil || *first.BuildRecipe != recipe {
		t.Fatal("explicit recipe was not frozen")
	}
	changed := recipe
	changed.BuildCommand = "printf changed"
	encrypted, err := f.s.sealJSON(changed)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE workload_build_recipes SET configuration=$2 WHERE workload_id=$1`, id, encrypted)
	if err := f.s.dispatchOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if submits != 2 {
		t.Fatal("missing retry")
	}
	if err := f.s.failOperation(context.Background(), op, "Fixture failure"); err != nil {
		t.Fatal(err)
	}
	failed := f.call(viewer, "default", "GET", "/api/operations/"+op, nil, 200)
	if !strings.Contains(failed.Body.String(), `"engine":"railpack"`) || strings.Contains(failed.Body.String(), "printf") {
		t.Fatal("failed history", failed.Body.String())
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='recipe-node'`)
	f.call("root", "default", "PUT", path, worker.BuildRecipe{Engine: "compose"}, 200)
	var rows int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM workload_build_recipes WHERE workload_id=$1`, id).Scan(&rows); err != nil || rows != 0 {
		t.Fatal(rows, err)
	}
}

func TestBuildRecipesStaticDeploymentUsesOnlyPublicOutputRuntime(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('static-node','Static node','localhost',22,'',$1,'ready','{"build_publish":true,"build_recipes":true,"os":"linux","arch":"amd64"}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('static-project','Static','default')`)
	config := workloadInput{Name: "Static app", ProjectID: "static-project", NodeID: "static-node", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n    user: root\n    read_only: false\n", Source: &worker.GitSource{Repository: "https://git.example.test/fixture/static.git", Branch: "main", InlineCompose: true}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	recipe := worker.BuildRecipe{Engine: "static", Service: "app", Directory: ".", Output: "public"}
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/build-recipe", recipe, 200)
	request := worker.Request{ID: "static-deploy", Workload: id, Action: "deploy", Kind: "service", Compose: config.Compose, Source: config.Source}
	if err := f.s.prepareBuildRecipe(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(request.Compose, "read_only: true") || !strings.Contains(request.Compose, "65534:65534") || request.BuildRecipe != nil {
		t.Fatal("runtime safety not applied without triggering a build", request.Compose)
	}
	request.Compose = config.Compose
	request.FileSecrets = []worker.FileSecret{{Name: "TOKEN", Variable: "TOKEN", Services: []string{"app"}}}
	if err := f.s.prepareBuildRecipe(context.Background(), &request); err == nil {
		t.Fatal("runtime secret mount bypassed static restriction")
	}
	config.FileSecrets = request.FileSecrets
	protected, err := f.s.sealJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE workloads SET configuration=$2 WHERE id=$1`, id, protected)
	status := f.call("root", "default", "GET", "/api/workloads/"+id+"/build-recipe", nil, 200)
	if !strings.Contains(status.Body.String(), "static output cannot mount runtime file secrets") {
		t.Fatal("invalid static setting did not expose its blocking reason", status.Body.String())
	}
	var encoded []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT configuration FROM workload_build_recipes WHERE workload_id=$1`, id).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	if json.Valid(encoded) {
		t.Fatal("recipe was not sealed")
	}
}
