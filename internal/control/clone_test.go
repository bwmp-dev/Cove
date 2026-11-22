package control

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCloneSharedVariablesAndInternalBindings(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('clone-node','Node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('clone-project','Original','default')`)
	f.sql(`INSERT INTO environments(id,name,project_id,team_id) VALUES('clone-env','Production','clone-project','default')`)
	input := workloadInput{Name: "Database", NodeID: "clone-node", ProjectID: "clone-project", EnvironmentID: "clone-env", Compose: "services: {}", Environment: map[string]string{"PASSWORD": "database-secret"}}
	database := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	input.Name = "App"
	input.Environment = map[string]string{"DB": "{{resource.DB}}", "URL": "{{project.URL}}", "LABEL": "{{environment.LABEL}}"}
	app := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	f.call("root", "default", "PUT", "/api/workloads/"+app+"/variable-bindings", resourceVariableBinding{Name: "DB", SourceID: database, SourceKey: "PASSWORD"}, 200)
	f.call("root", "default", "PATCH", "/api/projects/clone-project/shared-variables", map[string]any{"patch": map[string]string{"URL": "shared-project-value"}}, 200)
	f.call("root", "default", "PATCH", "/api/environments/clone-env/shared-variables", map[string]any{"patch": map[string]string{"LABEL": "shared-env-value"}}, 200)
	project := jsonString(t, f.call("root", "default", "POST", "/api/projects/clone-project/clone", map[string]any{"name": "With secrets", "include_secrets": true}, 201), "id")
	var copiedApp, copiedDB string
	if err := f.s.DB.QueryRow(ctx, `SELECT id FROM workloads WHERE project_id=$1 AND name='App'`, project).Scan(&copiedApp); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(ctx, `SELECT source_id FROM resource_variable_bindings WHERE workload_id=$1`, copiedApp).Scan(&copiedDB); err != nil || copiedDB == database {
		t.Fatal("binding still points to original database", copiedDB, err)
	}
	resolved, err := f.s.resolveSharedVariables(ctx, copiedApp, input.Environment)
	if err != nil || resolved["DB"] != "database-secret" || resolved["URL"] != "shared-project-value" || resolved["LABEL"] != "shared-env-value" {
		t.Fatal("copied references did not resolve", err)
	}
	without := jsonString(t, f.call("root", "default", "POST", "/api/projects/clone-project/clone", map[string]any{"name": "Without secrets", "include_secrets": false}, 201), "id")
	var count int
	if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM shared_variables WHERE project_id=$1`, without).Scan(&count); err != nil || count != 0 {
		t.Fatal("shared secrets copied without consent", count, err)
	}
	if err = f.s.DB.QueryRow(ctx, `SELECT count(*) FROM resource_variable_bindings b JOIN workloads w ON w.id=b.workload_id WHERE w.project_id=$1`, without).Scan(&count); err != nil || count != 0 {
		t.Fatal("bindings copied without consent", count, err)
	}
}

func TestCloneHierarchy(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("cloner", "cloner@example.com")
	viewer := f.user("reader", "reader@example.com")
	editor := f.user("editor", "editor@example.com")
	f.sql("INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','cloner','owner','[]'),('default','reader','member','[{\"scope\":\"team\",\"role\":\"viewer\"}]'),('default','editor','member','[{\"scope\":\"environment\",\"scope_id\":\"e\",\"role\":\"editor\"}]')")
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('n','Node','host',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO team_nodes(team_id,node_id) VALUES('default','n')")
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('p','Original','default')")
	f.sql("INSERT INTO environments(id,project_id,name,team_id) VALUES('e','p','Production','default'),('empty','p','Empty','default')")
	config := []byte(`{"name":"App","node_id":"n","kind":"service","compose":"services:\n  app:\n    image: nginx:alpine\n","environment":{"SECRET":"test-value"},"source":{"repository":"https://example.com/repo.git","token":"test-token"},"routes":[{"domain":"example.com"}],"backup":{"interval_hours":24}}`)
	encrypted, err := seal(f.s.Key, config)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,environment_id,team_id,backup_interval,webhook_secret,project_id) VALUES('w','n','App',$1,'e','default',24,$2,'p')", encrypted, []byte("webhook"))
	clone := func(user, path, name string, secrets bool, status int) string {
		w := f.call(user, "default", "POST", path, map[string]any{"name": name, "include_secrets": secrets}, status)
		if status != 201 {
			return ""
		}
		return jsonString(t, w, "id")
	}
	for _, path := range []string{"/api/projects/p/clone", "/api/environments/e/clone", "/api/workloads/w/clone"} {
		clone(viewer, path, "Denied", true, 403)
	}
	clone(editor, "/api/projects/p/clone", "Denied", true, 403)
	clone(editor, "/api/environments/e/clone", "Denied", true, 403)
	clone(owner, "/api/projects/p/clone", " ", false, 400)
	clone(owner, "/api/projects/p/clone", "Original", false, 409)
	check := func(id string, secrets bool) {
		t.Helper()
		var data []byte
		var interval, operations int
		var webhook bool
		if err := f.s.DB.QueryRow(context.Background(), "SELECT configuration,backup_interval,webhook_secret IS NOT NULL,(SELECT count(*) FROM operations WHERE workload_id=$1) FROM workloads WHERE id=$1", id).Scan(&data, &interval, &webhook, &operations); err != nil {
			t.Fatal(err)
		}
		raw, err := unseal(f.s.Key, data)
		if err != nil {
			t.Fatal(err)
		}
		var c workloadInput
		if err = json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		if len(c.Routes) != 0 || c.Backup != nil || interval != 0 || webhook || operations != 0 {
			t.Fatal("clone retained runtime or automation state")
		}
		if secrets {
			if c.Environment["SECRET"] != "test-value" || c.Source.Token != "test-token" {
				t.Fatal("secrets missing")
			}
		} else if len(c.Environment) != 0 || c.Source.Token != "" {
			t.Fatal("secrets copied without consent")
		}
	}
	check(clone(editor, "/api/workloads/w/clone", "Copy", false, 201), false)
	check(clone(owner, "/api/workloads/w/clone", "With secrets", true, 201), true)
	project := clone(owner, "/api/projects/p/clone", "Project copy", true, 201)
	var envs, resources int
	if err = f.s.DB.QueryRow(context.Background(), "SELECT count(DISTINCT e.id),count(w.id) FROM environments e LEFT JOIN workloads w ON w.environment_id=e.id WHERE e.project_id=$1", project).Scan(&envs, &resources); err != nil {
		t.Fatal(err)
	}
	if envs != 2 || resources != 3 {
		t.Fatalf("hierarchy: %d environments %d resources", envs, resources)
	}
	env := clone(owner, "/api/environments/empty/clone", "Empty copy", false, 201)
	if env == "" {
		t.Fatal("empty environment not copied")
	}
	populated := clone(owner, "/api/environments/e/clone", "Staging", false, 201)
	var copied string
	if err = f.s.DB.QueryRow(context.Background(), "SELECT id FROM workloads WHERE environment_id=$1 AND name='App'", populated).Scan(&copied); err != nil {
		t.Fatal(err)
	}
	check(copied, false)
	f.sql("INSERT INTO teams(id,name) VALUES('other','Other')")
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('foreign','Foreign','other')")
	clone(owner, "/api/projects/foreign/clone", "Forbidden", true, 404)
	f.sql("DELETE FROM team_nodes WHERE team_id='default' AND node_id='n'")
	clone(owner, "/api/projects/p/clone", "Rollback copy", true, 403)
	var exists bool
	if err = f.s.DB.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM projects WHERE name='Rollback copy')").Scan(&exists); err != nil || exists {
		t.Fatal("failed clone was not rolled back", err)
	}
}
