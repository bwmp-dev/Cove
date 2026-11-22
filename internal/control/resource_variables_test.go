package control

import (
	"context"
	"strings"
	"testing"
)

func TestResourceBindingsAuthorizationResolutionAndDeletion(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('binding-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('binding-project','Project','default')")
	create := func(name string, env map[string]string) string {
		return jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: name, NodeID: "binding-node", ProjectID: "binding-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n", Environment: env}, 201), "id")
	}
	db := create("database", map[string]string{"PASSWORD": "binding-secret", "HOST": "{{project.HOST}}"})
	app := create("application", map[string]string{"PASSWORD": "{{resource.DB}}"})
	f.call("root", "default", "PATCH", "/api/projects/binding-project/shared-variables", map[string]any{"patch": map[string]string{"HOST": "database.internal"}}, 200)
	binding := resourceVariableBinding{Name: "DB", SourceID: db, SourceKey: "PASSWORD"}
	path := "/api/workloads/" + app + "/variable-bindings"
	editor := f.user("binding-editor", "binding-editor@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','binding-editor','member',$1)`, `[{"scope":"workload","scope_id":"`+app+`","role":"editor"}]`)
	// A project-scoped editor may bind resources in their project; a viewer cannot export secrets.
	f.sql(`UPDATE team_members SET grants=$1::jsonb WHERE user_id='binding-editor'`, `[{"scope":"project","scope_id":"binding-project","role":"viewer"}]`)
	f.call(editor, "default", "PUT", path, binding, 403)
	f.call("root", "default", "PUT", path, binding, 200)
	graph := f.call("root", "default", "GET", "/api/projects/binding-project/dependencies", nil, 200).Body.String()
	if !strings.Contains(graph, `"kind":"variable"`) || strings.Contains(graph, "binding-secret") {
		t.Fatal(graph)
	}
	body := f.call("root", "default", "GET", path, nil, 200).Body.String()
	if strings.Contains(body, "binding-secret") {
		t.Fatal("binding exposed value")
	}
	got, err := f.s.resolveSharedVariables(context.Background(), app, map[string]string{"URL": "db://{{resource.DB}}"})
	if err != nil || got["URL"] != "db://binding-secret" {
		t.Fatal(got, err)
	}
	f.call("root", "default", "DELETE", "/api/workloads/"+db, nil, 409)
	binding.SourceKey = "HOST"
	f.call("root", "default", "PUT", path, binding, 200)
	got, err = f.s.resolveSharedVariables(context.Background(), app, map[string]string{"HOST": "{{resource.DB}}"})
	if err != nil || got["HOST"] != "database.internal" {
		t.Fatal(got, err)
	}
	// Export a value which refers back to this resource; deployment must terminate with an error.
	f.call("root", "default", "PUT", "/api/workloads/"+db+"/variable-bindings", resourceVariableBinding{Name: "APP", SourceID: app, SourceKey: "PASSWORD"}, 200)
	f.call("root", "default", "PUT", "/api/workloads/"+db, workloadInput{Name: "database", NodeID: "binding-node", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n", Environment: map[string]string{"PASSWORD": "{{resource.APP}}"}}, 200)
	binding.SourceKey = "PASSWORD"
	f.call("root", "default", "PUT", path, binding, 200)
	if _, err = f.s.resolveSharedVariables(context.Background(), app, map[string]string{"PASSWORD": "{{resource.DB}}"}); err == nil {
		t.Fatal("cycle resolved")
	}
	f.call("root", "default", "DELETE", path, map[string]string{"name": "DB"}, 200)
	if _, err = f.s.resolveSharedVariables(context.Background(), app, map[string]string{"PASSWORD": "{{resource.DB}}"}); err == nil {
		t.Fatal("missing binding resolved")
	}
}
