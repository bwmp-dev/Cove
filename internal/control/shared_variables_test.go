package control

import (
	"context"
	"cove/internal/worker"
	"strings"
	"testing"
)

func TestSharedVariablesScopesAndResolution(t *testing.T) {
	f := newAccountFixture(t)
	editor := f.user("shared-editor", "shared-editor@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','shared-editor','member','[{"scope":"environment","scope_id":"shared-env","role":"editor"}]')`)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('shared-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('shared-project','Project','default')")
	f.sql("INSERT INTO environments(id,name,project_id,team_id) VALUES('shared-env','Env','shared-project','default'),('shared-other','Other','shared-project','default')")
	in := workloadInput{Name: "App", NodeID: "shared-node", ProjectID: "shared-project", EnvironmentID: "shared-env", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", in, 201), "id")
	patch := func(path, name, value string) {
		f.call("root", "default", "PATCH", path, map[string]any{"patch": map[string]string{name: value}}, 200)
	}
	patch("/api/shared-variables", "HOST", "db.internal")
	patch("/api/projects/shared-project/shared-variables", "PASS", "secret-value")
	patch("/api/environments/shared-env/shared-variables", "DB", "production")
	patch("/api/environments/shared-other/shared-variables", "OTHER", "not-accessible")
	f.call(editor, "default", "GET", "/api/shared-variables", nil, 403)
	f.call(editor, "default", "GET", "/api/projects/shared-project/shared-variables", nil, 403)
	f.call(editor, "default", "GET", "/api/environments/shared-env/shared-variables", nil, 200)
	response := f.call("root", "default", "GET", "/api/projects/shared-project/shared-variables", nil, 200)
	if strings.Contains(response.Body.String(), "secret-value") {
		t.Fatal("secret exposed")
	}
	f.call("root", "default", "PATCH", "/api/shared-variables", map[string]any{"patch": map[string]string{"PATH": "bad"}}, 400)
	env := map[string]string{"URL": "postgres://{{project.PASS}}@{{team.HOST}}/{{environment.DB}}"}
	got, err := f.s.resolveSharedVariables(context.Background(), id, env)
	if err != nil || got["URL"] != "postgres://secret-value@db.internal/production" {
		t.Fatal(got, err)
	}
	if env["URL"] == got["URL"] {
		t.Fatal("mutated input")
	}
	if _, err = f.s.resolveSharedVariables(context.Background(), id, map[string]string{"X": "{{environment.OTHER}}"}); err == nil {
		t.Fatal("resolved another environment")
	}
	patch("/api/shared-variables", "LITERAL", "{{project.PASS}}")
	got, err = f.s.resolveSharedVariables(context.Background(), id, map[string]string{"X": "{{team.LITERAL}}"})
	if err != nil || got["X"] != "{{project.PASS}}" {
		t.Fatal("recursive expansion", got, err)
	}
	op := resourceID()
	f.sql("INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'deploy',$3)", op, id, []byte{})
	req := worker.Request{ID: op, Workload: id, Action: "deploy", Environment: env}
	if err = f.s.prepareInfrastructure(context.Background(), &req); err != nil {
		t.Fatal(err)
	}
	patch("/api/projects/shared-project/shared-variables", "PASS", "changed")
	if err = f.s.prepareInfrastructure(context.Background(), &req); err != nil || req.Environment["URL"] != "postgres://secret-value@db.internal/production" {
		t.Fatal("retry changed variables", err)
	}
	f.call("root", "default", "PATCH", "/api/projects/shared-project/shared-variables", map[string]any{"patch": map[string]any{"PASS": nil}}, 200)
	if _, err = f.s.resolveSharedVariables(context.Background(), id, env); err == nil {
		t.Fatal("deleted value resolved")
	}
}
