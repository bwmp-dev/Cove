package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNixpacksExplicitRecipeCapabilityPolicyAndFrozenBuild(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('nixpacks-node','Nixpacks node','localhost',22,'',$1,'ready','{"build_publish":true,"build_recipes":true,"os":"linux","arch":"amd64"}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('nixpacks-project','Nixpacks','default')`)
	configuration := workloadInput{Name: "Nixpacks fixture", ProjectID: "nixpacks-project", NodeID: "nixpacks-node", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n", Source: &worker.GitSource{Repository: "https://git.example.test/fixture/app.git", Branch: "main", InlineCompose: true}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", configuration, 201), "id")
	path := "/api/workloads/" + id + "/build-recipe"
	recipe := worker.BuildRecipe{Engine: "nixpacks", Service: "app", Directory: ".", BuildCommand: "echo fixture-command-not-for-viewers", StartCommand: "./fixture-server"}
	f.call("root", "default", "PUT", path, recipe, 409)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"nixpacks":true}' WHERE id='nixpacks-node'`)
	f.call("root", "default", "PUT", path, recipe, 200)
	status := f.call("root", "default", "GET", path, nil, 200)
	var response struct {
		Recipe   worker.BuildRecipe     `json:"recipe"`
		Nixpacks worker.NixpacksSupport `json:"nixpacks"`
	}
	if json.Unmarshal(status.Body.Bytes(), &response) != nil || response.Recipe != recipe || !response.Nixpacks.Supported || !response.Nixpacks.Maintenance || response.Nixpacks.ProxySupported || response.Nixpacks.PlannerSHA256 != worker.NixpacksPlannerDigest("amd64") || response.Nixpacks.BaseImage != worker.NixpacksBaseImage {
		t.Fatal("unexpected metadata", status.Body.String())
	}
	viewer := f.user("nixpacks-viewer", "nixpacks-viewer@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','nixpacks-viewer','member','[{"scope":"project","scope_id":"nixpacks-project","role":"viewer"}]')`)
	view := f.call(viewer, "default", "GET", path, nil, 200)
	if strings.Contains(view.Body.String(), "fixture-command-not-for-viewers") || !strings.Contains(view.Body.String(), `"build_command_set":true`) {
		t.Fatal("command visibility", view.Body.String())
	}
	f.call(viewer, "default", "PUT", path, worker.BuildRecipe{Engine: "compose"}, 403)
	registry := jsonString(t, f.call("root", "default", "POST", "/api/registries", connectionConfig{Name: "Nixpacks fixture registry", Registry: &worker.Registry{Server: "registry.example.test", Username: "fixture", Password: "fixture-only"}}, 201), "id")
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/registry", map[string]string{"registry_id": registry}, 200)
	for _, policy := range []string{"network: host", "args: {HTTP_PROXY: 'http://proxy.example.test'}"} {
		changed := configuration
		changed.ProjectID = ""
		changed.Compose = "services:\n  app:\n    image: busybox:1.37\n    build:\n      context: .\n      " + policy + "\n"
		f.call("root", "default", "PUT", "/api/workloads/"+id, changed, 200)
		f.call("root", "default", "PUT", path, recipe, 400)
		f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]any{"action": "build", "build": worker.BuildOptions{Repository: "fixture/app"}}, 400)
	}
	configuration.ProjectID = ""
	configuration.Compose = "services:\n  app:\n    image: busybox:1.37\n    build:\n      context: .\n      network: none\n"
	f.call("root", "default", "PUT", "/api/workloads/"+id, configuration, 200)
	f.call("root", "default", "PUT", path, recipe, 200)
	f.sql(`UPDATE nodes SET capabilities=capabilities-'nixpacks' WHERE id='nixpacks-node'`)
	f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]any{"action": "build", "build": worker.BuildOptions{Repository: "fixture/app"}}, 409)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"nixpacks":true}' WHERE id='nixpacks-node'`)
	op := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]any{"action": "build", "build": worker.BuildOptions{Repository: "fixture/app"}}, 202), "id")
	f.call("root", "default", "PUT", path, worker.BuildRecipe{Engine: "compose"}, 409)
	var submitted worker.Request
	f.s.workerRPC = func(_ context.Context, node string, request worker.RPCRequest) (worker.RPCResponse, error) {
		if request.Method == "status" {
			return worker.RPCResponse{}, errors.New("fixture operation not yet submitted")
		}
		if node != "nixpacks-node" || request.Method != "submit_build_recipe" || request.Request == nil {
			t.Fatalf("unexpected submission %s %s", node, request.Method)
		}
		submitted = *request.Request
		return worker.RPCResponse{Operation: &worker.Operation{ID: op, Workload: id, Action: "build", State: "running", BuildRecipe: worker.BuildRecipeSummary(request.Request.BuildRecipe)}}, nil
	}
	if err := f.s.dispatchOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if submitted.BuildRecipe == nil || *submitted.BuildRecipe != recipe || submitted.BuildPlatform != "linux/amd64" {
		t.Fatal("explicit recipe and platform were not frozen", submitted.BuildRecipe, submitted.BuildPlatform)
	}
	if err := f.s.failOperation(context.Background(), op, "Fixture finished"); err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='nixpacks-node'`)
	f.call("root", "default", "PUT", path, worker.BuildRecipe{Engine: "compose"}, 200)
}
