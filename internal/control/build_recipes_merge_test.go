package control

import (
	"cove/internal/outbound"
	"cove/internal/worker"
	"strings"
	"testing"
)

func TestBuildRecipesMergeScopesAndProxy(t *testing.T) {
	f, id, cookie := publicFixture(t)
	f.sql(`UPDATE nodes SET state='ready',capabilities='{"build_publish":true,"build_recipes":true,"compose_bundles":true,"os":"linux","arch":"amd64"}' WHERE id='variable-node'`)
	config := workloadInput{Name: "Recipe", NodeID: "variable-node", ProjectID: "variable-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n", Source: &worker.GitSource{Repository: "https://git.example.test/fixture.git", Branch: "main", InlineCompose: true}}
	store := func() {
		t.Helper()
		sealed, err := f.s.sealJSON(config)
		if err != nil {
			t.Fatal(err)
		}
		f.sql(`UPDATE workloads SET configuration=$2 WHERE id=$1`, id, sealed)
	}
	store()
	_, read := mintPublic(t, f, cookie, "user", "workloads:read")
	_, write := mintPublic(t, f, cookie, "user", "configuration:write")
	path := "/api/v1/workloads/" + id + "/build-recipe"
	recipe := worker.BuildRecipe{Engine: "railpack", Service: "app", Directory: ".", BuildCommand: "printf fixture-private-command"}
	publicCall(t, f, read, "default", "GET", path, nil, 200)
	publicCall(t, f, read, "default", "PUT", path, recipe, 403)
	publicCall(t, f, write, "default", "PUT", path, recipe, 200)
	metadata := publicCall(t, f, read, "default", "GET", path, nil, 200)
	if strings.Contains(metadata.Body.String(), "fixture-private-command") || !strings.Contains(metadata.Body.String(), `"build_command_set":true`) {
		t.Fatal("read scope exposed a custom build command")
	}
	configEditor := f.user("recipe-config-editor", "recipe-config-editor@example.test")
	f.sql(`INSERT INTO team_roles(team_id,id,name,permissions) VALUES('default','recipe-config','Recipe configuration',ARRAY['view','edit_configuration'])`)
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','recipe-config-editor','member','[{"scope":"project","scope_id":"variable-project","role":"recipe-config"}]')`)
	custom := f.call(configEditor, "default", "GET", "/api/workloads/"+id+"/build-recipe", nil, 200)
	if !strings.Contains(custom.Body.String(), `"can_edit":true`) || !strings.Contains(custom.Body.String(), "fixture-private-command") {
		t.Fatal("Edit configuration was treated as legacy full-editor access")
	}
	f.s.outboundState.value.Store(&outbound.Config{HTTPS: "http://proxy.example.test:8080"})
	f.s.outboundState.saved.Store(true)
	publicCall(t, f, write, "default", "PUT", path, recipe, 409)
	status := publicCall(t, f, read, "default", "GET", path, nil, 200)
	if !strings.Contains(status.Body.String(), "pinned Railpack frontend") {
		t.Fatal("status concealed the inherited policy refusal")
	}
	publicCall(t, f, write, "default", "PUT", path, worker.BuildRecipe{Engine: "compose"}, 200)
}
