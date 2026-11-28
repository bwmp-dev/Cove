package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestProjectResourcePlacement(t *testing.T) {
	f := newAccountFixture(t)
	editor := f.user("project-editor", "project@example.com")
	viewer := f.user("env-viewer", "environment@example.com")
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('p','Project','default'),('q','Other project','default')`)
	f.sql(`INSERT INTO environments(id,project_id,name,team_id) VALUES('e','p','Production','default'),('f','q','Production','default')`)
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','project-editor','member','[{"scope":"project","scope_id":"p","role":"editor"}]'),('default','env-viewer','member','[{"scope":"environment","scope_id":"e","role":"viewer"}]')`)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('n','Node','host',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('default','n')`)
	in := workloadInput{Name: "Shared database", NodeID: "n", Compose: "services:\n  app:\n    image: busybox:1.37\n"}
	f.call("root", "default", "POST", "/api/workloads", in, 400)
	in.ProjectID = "p"
	id := jsonString(t, f.call(editor, "default", "POST", "/api/workloads", in, 201), "id")
	path := "/api/workloads/" + id
	f.call(viewer, "default", "GET", path, nil, 403)
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES('direct-operation',$1,'deploy','succeeded',$2)`, id, []byte{})
	f.call(viewer, "default", "GET", "/api/operations/direct-operation", nil, 403)
	f.call(viewer, "default", "POST", "/api/tokens", map[string]any{"name": "Denied", "workload_id": id, "permission": "read", "days": 1}, 403)
	if strings.Contains(f.call(viewer, "default", "GET", "/api/operations", nil, 200).Body.String(), "direct-operation") {
		t.Fatal("environment viewer can list project resource operations")
	}
	if strings.Contains(f.call(viewer, "default", "GET", "/api/workloads", nil, 200).Body.String(), id) {
		t.Fatal("environment viewer can list project resource")
	}
	f.call(editor, "default", "PUT", path+"/environment", map[string]string{"project_id": "q"}, 403)
	f.call("root", "default", "PUT", path+"/environment", map[string]string{"project_id": "p", "environment_id": "f"}, 400)
	move := func(environment string) {
		t.Helper()
		in := resourceMoveInput{TeamID: "default", ProjectID: "p", EnvironmentID: environment}
		var review resourceMoveReview
		if json.Unmarshal(f.call(editor, "default", "POST", path+"/move/preview", in, 200).Body.Bytes(), &review) != nil {
			t.Fatal("invalid move review")
		}
		in.Version, in.Acknowledge = review.Version, true
		f.call(editor, "default", "POST", path+"/move", in, 200)
	}
	f.call(editor, "default", "PUT", path+"/environment", map[string]string{"project_id": "p", "environment_id": "e"}, 409)
	move("e")
	f.call(viewer, "default", "GET", path, nil, 200)
	move("")
	f.call(viewer, "default", "GET", path, nil, 403)
	f.call(editor, "default", "DELETE", "/api/projects/p", nil, 409)
	copy := jsonString(t, f.call("root", "default", "POST", "/api/projects/p/clone", map[string]string{"name": "Copy"}, 201), "id")
	var count int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM workloads WHERE project_id=$1 AND environment_id IS NULL`, copy).Scan(&count); err != nil || count != 1 {
		t.Fatal("project clone lost direct resource", count, err)
	}
	environmentCopy := jsonString(t, f.call("root", "default", "POST", "/api/environments/e/clone", map[string]string{"name": "Environment copy"}, 201), "id")
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM workloads WHERE environment_id=$1`, environmentCopy).Scan(&count); err != nil || count != 0 {
		t.Fatal("environment clone included project resource", count, err)
	}
}

func TestProjectPlacementMigration(t *testing.T) {
	f := newAccountFixture(t)
	configuration, err := seal(f.s.Key, []byte(`{"name":"Migration fixture","limits":{"app":{"cpus":1,"memory_mb":64}}}`))
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`ALTER TABLE workloads DROP COLUMN project_id CASCADE; DROP INDEX environments_id_project_team; DELETE FROM schema_migrations WHERE id='resource-project-placement-v1'`)
	f.sql(`INSERT INTO teams(id,name) VALUES('other','Other')`)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('p','Project','default'),('existing','Imported resources','other')`)
	f.sql(`INSERT INTO environments(id,project_id,name,team_id) VALUES('e','p','Production','default')`)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('n','Node','host',22,'',$1)`, []byte{})
	f.sql(`INSERT INTO workloads(id,name,node_id,configuration,team_id,environment_id) VALUES('placed','App','n',$1,'default','e'),('direct','Database','n',$1,'default',NULL),('other','Other','n',$1,'other',NULL)`, configuration)
	for range 2 {
		if err := f.s.Init(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for id, project := range map[string]string{"placed": "p", "other": "existing"} {
		var got string
		if err := f.s.DB.QueryRow(context.Background(), `SELECT project_id FROM workloads WHERE id=$1`, id).Scan(&got); err != nil || got != project {
			t.Fatal("incorrect migrated project", id, got, err)
		}
	}
	var preserved int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM workloads w JOIN projects p ON p.id=w.project_id AND p.team_id=w.team_id WHERE w.configuration=$1 AND (w.id!='direct' OR p.name='Imported resources')`, configuration).Scan(&preserved); err != nil || preserved != 3 {
		t.Fatal("migration lost resource configuration or team placement", preserved, err)
	}
	for _, query := range []string{`UPDATE workloads SET project_id=NULL WHERE id='direct'`, `UPDATE workloads SET project_id='existing' WHERE id='direct'`, `UPDATE workloads SET environment_id='e' WHERE id='direct'`} {
		if _, err := f.s.DB.Exec(context.Background(), query); err == nil {
			t.Fatal("invalid placement accepted", query)
		}
	}
}
