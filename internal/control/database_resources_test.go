package control

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"cove/internal/catalog"
	"cove/internal/worker"
)

func databaseInput(t *testing.T, engineID, node, project string) workloadInput {
	t.Helper()
	engine, ok := findDatabaseEngine(engineID)
	if !ok {
		t.Fatal("unknown engine")
	}
	definition, revision, err := databaseTemplate(engine)
	if err != nil {
		t.Fatal(err)
	}
	return workloadInput{Name: engine.name, NodeID: node, ProjectID: project, Kind: "database", Database: &databaseResource{Engine: engineID, Version: databaseImageVersion(definition.Images[engine.service]), TemplateRevision: revision}}
}

func TestDatabaseAdminToolsUseTheCataloguePins(t *testing.T) {
	index, _, err := catalog.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"adminer", "mongo-express", "redisinsight"} {
		found := false
		for _, definition := range index.Templates {
			for _, image := range definition.Images {
				if image == worker.DatabaseToolImage(tool) {
					found = true
				}
			}
		}
		if !found {
			t.Fatal("tool image does not match a pinned catalogue image", tool)
		}
	}
}

func readDatabaseConfig(t *testing.T, f *accountFixture, id string) workloadInput {
	t.Helper()
	var encrypted []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT configuration FROM workloads WHERE id=$1", id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	var config workloadInput
	if err := f.s.unsealJSON(encrypted, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestEveryDatabaseUsesPinnedTemplateAndFreshCredentials(t *testing.T) {
	for _, engine := range databaseEngines {
		t.Run(engine.id, func(t *testing.T) {
			first := databaseInput(t, engine.id, "node", "project")
			second := databaseInput(t, engine.id, "node", "project")
			if err := prepareDatabase(&first, "11111111111111111111111111111111"); err != nil {
				t.Fatal(err)
			}
			if err := prepareDatabase(&second, "22222222222222222222222222222222"); err != nil {
				t.Fatal(err)
			}
			if first.Kind != "service" || resourceKind(first) != "database" || first.Environment[engine.password] == second.Environment[engine.password] || len(first.Environment[engine.password]) != 64 {
				t.Fatal("invalid database execution kind or credentials")
			}
			if err := validateWorkload(first); err != nil {
				t.Fatal(err)
			}
			values, err := databaseExports(first, nil)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(values["DB_URL"])
			if err != nil {
				t.Fatal(err)
			}
			password, _ := u.User.Password()
			if password != first.Environment[engine.password] || u.Hostname() != first.Database.Host || u.User.Username() != engine.user || values["DB_NAME"] != engine.database {
				t.Fatal("connection values disagree with generated configuration")
			}
			if engine.id == "mongodb" && u.Query().Get("authSource") != "admin" {
				t.Fatal("MongoDB authentication source missing")
			}
			if engine.id == "redis" || engine.id == "valkey" {
				if u.User.Username() != "default" || u.Path != "/0" {
					t.Fatal("Redis ACL/database URL invalid")
				}
			}
			if strings.Contains(first.Compose, "ports:") {
				t.Fatal("database publishes host ports")
			}
		})
	}
}

func TestDatabaseCreationMetadataSecretsAndManagedConfiguration(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('database-node','Node','localhost',22,'',$1,'ready','{"private_links":true,"logical_backups":true}')`, []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('database-project','Databases','default')")
	viewer := f.user("database-viewer", "viewer@example.com")
	editor := f.user("database-editor", "editor@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','database-viewer','member','[{"scope":"team","role":"viewer"}]'),('default','database-editor','member','[{"scope":"team","role":"editor"}]')`)
	f.sql("INSERT INTO team_nodes(team_id,node_id) VALUES('default','database-node')")
	f.call(viewer, "default", "GET", "/api/database-engines", nil, 200)
	for _, engine := range databaseEngines {
		input := databaseInput(t, engine.id, "database-node", "database-project")
		f.call(viewer, "default", "POST", "/api/workloads", input, 403)
		id := jsonString(t, f.call(editor, "default", "POST", "/api/workloads", input, 201), "id")
		config := readDatabaseConfig(t, f, id)
		w := f.call(viewer, "default", "GET", "/api/workloads/"+id, nil, 200)
		if !strings.Contains(w.Body.String(), `"kind":"database"`) || !strings.Contains(w.Body.String(), "DB_URL") || strings.Contains(w.Body.String(), config.Environment[engine.password]) {
			t.Fatal("database metadata missing or leaked credentials")
		}
		w = f.call(viewer, "default", "GET", "/api/workloads/"+id+"/database", nil, 200)
		if strings.Contains(w.Body.String(), config.Environment[engine.password]) {
			t.Fatal("metadata exposed credentials")
		}
		f.call(viewer, "default", "GET", "/api/workloads/"+id+"/database/credentials", nil, 403)
		w = f.call(editor, "default", "GET", "/api/workloads/"+id+"/database/credentials", nil, 200)
		if !strings.Contains(w.Body.String(), config.Environment[engine.password]) {
			t.Fatal("editor did not receive connection credentials")
		}
		config.Name = "Renamed " + engine.name
		config.Kind = "database"
		config.Database = nil
		config.Environment = nil
		f.call(editor, "default", "PUT", "/api/workloads/"+id, config, 200)
		config.EnvironmentPatch = map[string]*string{engine.password: nil}
		f.call(editor, "default", "PUT", "/api/workloads/"+id, config, 409)
		f.call(editor, "default", "PUT", "/api/workloads/"+id+"/backup-policy", map[string]any{"backup": nil}, 200)
		if readDatabaseConfig(t, f, id).Database == nil {
			t.Fatal("existing backup policy API discarded database metadata")
		}
	}
	var reads, creates int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FILTER(WHERE action='read database credentials'),count(*) FILTER(WHERE action='create database') FROM audit_events").Scan(&reads, &creates); err != nil || reads != 7 || creates != 7 {
		t.Fatal("database access/create audit missing", reads, creates, err)
	}
	input := databaseInput(t, "postgresql", "database-node", "database-project")
	input.Database.TemplateRevision = "stale"
	f.call(editor, "default", "POST", "/api/workloads", input, 409)
	input = databaseInput(t, "postgresql", "database-node", "database-project")
	input.Compose = "services: {}"
	f.call(editor, "default", "POST", "/api/workloads", input, 400)
}

func TestDatabaseBindingsResolveAndConnectionsStayExplicit(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('database-node','Node','localhost',22,'',$1,'ready','{"private_links":true}')`, []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('database-project','Databases','default')")
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", databaseInput(t, "postgresql", "database-node", "database-project"), 201), "id")
	config := readDatabaseConfig(t, f, id)
	app := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Consumer", NodeID: "database-node", ProjectID: "database-project", Compose: "services:\n  app:\n    image: busybox:1.37\n", Environment: map[string]string{"DATABASE_URL": "{{resource.DATABASE}}"}}, 201), "id")
	path := "/api/workloads/" + app + "/variable-bindings"
	f.call("root", "default", "PUT", path, resourceVariableBinding{Name: "DATABASE", SourceID: id, SourceKey: "DB_URL"}, 200)
	values, err := f.s.resolveSharedVariables(context.Background(), app, map[string]string{"DATABASE_URL": "{{resource.DATABASE}}"})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(values["DATABASE_URL"])
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsed.User.Password()
	if password != config.Environment["POSTGRES_PASSWORD"] || parsed.Hostname() != config.Database.Host {
		t.Fatal("database binding was not resolved")
	}
	var count int
	if err = f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM resource_connections").Scan(&count); err != nil || count != 0 {
		t.Fatal("binding implicitly granted network access", err)
	}
	connection := privateConnectionInput{SourceID: app, SourceService: "app", TargetID: id, TargetService: config.Database.Service, Port: config.Database.Port, Alias: "other.internal"}
	f.call("root", "default", "POST", "/api/private-connections", connection, 400)
	connection.Alias = config.Database.Host
	f.call("root", "default", "POST", "/api/private-connections", connection, 201)
	config.Environment["POSTGRES_PASSWORD"] = "{{project.SECRET}}"
	encrypted, err := f.s.sealJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("UPDATE workloads SET configuration=$2 WHERE id=$1", id, encrypted)
	f.call("root", "default", "PATCH", "/api/projects/database-project/shared-variables", map[string]any{"patch": map[string]string{"SECRET": "fixture-@:/password"}}, 200)
	values, err = f.s.resolveSharedVariables(context.Background(), app, map[string]string{"DATABASE_URL": "{{resource.DATABASE}}"})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = url.Parse(values["DATABASE_URL"])
	if err != nil {
		t.Fatal(err)
	}
	password, _ = parsed.User.Password()
	if password != "fixture-@:/password" {
		t.Fatal("source references were encoded before resolution")
	}
	for _, key := range databaseExportKeys {
		if !databaseExportKey(key) {
			t.Fatal("missing export")
		}
	}
}

func TestDatabaseCloneFreshensCredentialsAndHostname(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('database-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('database-project','Databases','default')")
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", databaseInput(t, "mysql", "database-node", "database-project"), 201), "id")
	copyID := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/clone", map[string]any{"name": "Copy", "include_secrets": false}, 201), "id")
	source, copied := readDatabaseConfig(t, f, id), readDatabaseConfig(t, f, copyID)
	if copied.Database == nil || copied.Database.Host == source.Database.Host || copied.Environment["DATABASE_PASSWORD"] == source.Environment["DATABASE_PASSWORD"] || copied.Environment["ROOT_PASSWORD"] == source.Environment["ROOT_PASSWORD"] {
		t.Fatal("clone inherited credentials or hostname")
	}
	var kind string
	if err := f.s.DB.QueryRow(context.Background(), "SELECT kind FROM workloads WHERE id=$1", copyID).Scan(&kind); err != nil || kind != "database" {
		t.Fatal("clone lost database kind", err)
	}
	if err := validateWorkload(copied); err != nil {
		t.Fatal(err)
	}
	request := worker.Request{ID: resourceID(), Workload: copyID, Action: "deploy", Kind: copied.Kind, Compose: copied.Compose, Environment: copied.Environment}
	if err := worker.ValidateRequest(request); err != nil {
		t.Fatal("database is not compatible with the service worker protocol", err)
	}
}
