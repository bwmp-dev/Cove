package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestProjectConfigurationStrictSchema(t *testing.T) {
	valid := "api_version: cove/v1\nproject: App\nenvironments: []\nresources:\n  - key: app\n    configuration:\n      name: App\n      node_id: node\n      compose: 'services: {}'\n"
	if _, err := parseProjectConfiguration(valid); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{
		valid + "unknown: true\n", valid + "---\nproject: second\n", valid + "project: duplicate\n",
		strings.Replace(valid, "node_id: node", "node_id: 123", 1),
		strings.Replace(valid, "key: app", "key: ../escape", 1),
		strings.Replace(valid, "name: App", "source: {ssh_private_key: secret}\n      name: App", 1),
	} {
		if _, err := parseProjectConfiguration(content); err == nil {
			t.Fatal("invalid manifest accepted")
		}
	}
}

func TestProjectDraftReviewPreservesSecretsAndRefusesStaleApply(t *testing.T) {
	f, id := variablesFixture(t)
	path := "/api/projects/variable-project/configuration"
	get := f.call("root", "default", "GET", path, nil, 200)
	if strings.Contains(get.Body.String(), "saved-secret") || !strings.Contains(get.Body.String(), "SECRET") {
		t.Fatal("manifest metadata exposed secret values")
	}
	var response struct {
		Version     string               `json:"version"`
		ResourceIDs map[string]string    `json:"resource_ids"`
		Manifest    projectConfiguration `json:"manifest"`
	}
	if json.Unmarshal(get.Body.Bytes(), &response) != nil || response.Version == "" || len(response.Manifest.Resources) != 1 {
		t.Fatal("invalid manifest response")
	}
	if response.ResourceIDs[response.Manifest.Resources[0].Key] != id {
		t.Fatal("resource metadata does not identify its actual workload")
	}
	response.Manifest.Resources[0].Configuration.Name = "Updated"
	raw, _ := json.Marshal(response.Manifest)
	draft := jsonString(t, f.call("root", "default", "POST", path+"/drafts", map[string]string{"content": string(raw)}, 201), "id")
	var sealed []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT payload FROM configuration_drafts WHERE id=$1`, draft).Scan(&sealed); err != nil || strings.Contains(string(sealed), "Updated") {
		t.Fatal("draft not sealed", err)
	}
	var name string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT name FROM workloads WHERE id=$1`, id).Scan(&name); err != nil || name != "Variables" {
		t.Fatal("saving draft changed resource", err)
	}
	f.call("root", "default", "POST", path+"/drafts/"+draft+"/apply", nil, 200)
	export := f.call("root", "default", "POST", "/api/workloads/"+id+"/variables/export", nil, 200)
	if !strings.Contains(export.Body.String(), "saved-secret") {
		t.Fatal("redacted manifest lost existing secrets")
	}
	f.call("root", "default", "POST", path+"/drafts/"+draft+"/apply", nil, 409)
	stale := jsonString(t, f.call("root", "default", "POST", path+"/drafts", map[string]string{"content": string(raw)}, 201), "id")
	f.call("root", "default", "POST", "/api/workloads/"+id+"/variables/import", map[string]string{"dotenv": "SECRET=changed-after-review"}, 200)
	f.call("root", "default", "POST", path+"/drafts/"+stale+"/apply", nil, 409)
	list := f.call("root", "default", "GET", path+"/drafts", nil, 200)
	if strings.Contains(list.Body.String(), "saved-secret") || strings.Contains(list.Body.String(), "changed-after-review") {
		t.Fatal("draft list leaked secrets")
	}
	f.call("root", "default", "DELETE", path+"/drafts/"+stale, nil, 200)
	viewer := f.user("config-viewer", "config-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','config-viewer','member','[{"scope":"project","scope_id":"variable-project","role":"viewer"}]')`)
	f.call(viewer, "default", "GET", path, nil, 200)
	f.call(viewer, "default", "POST", path+"/drafts", map[string]string{"content": string(raw)}, 403)
}

func TestProjectDraftCreatesResourcesAndBindingsAtomically(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('manifest-node','Node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('manifest-project','Manifest','default')`)
	manifest := projectConfiguration{APIVersion: "cove/v1", Project: "Manifest", Environments: []configurationEnvironment{{Name: "Production"}}, Resources: []configurationResource{
		{Key: "database", Environment: "Production", Configuration: workloadInput{Name: "Database", NodeID: "manifest-node", Compose: "services: {}", Environment: map[string]string{"PASSWORD": "manifest-secret"}}},
		{Key: "web", Environment: "Production", Configuration: workloadInput{Name: "Web", NodeID: "manifest-node", Compose: "services: {}", Environment: map[string]string{"DATABASE_PASSWORD": "{{resource.DB_PASSWORD}}"}}, Bindings: []configurationBinding{{Name: "DB_PASSWORD", Resource: "database", Key: "PASSWORD"}}},
	}}
	base := "/api/projects/manifest-project/configuration"
	raw, _ := json.Marshal(manifest)
	draft := jsonString(t, f.call("root", "default", "POST", base+"/drafts", map[string]string{"content": string(raw)}, 201), "id")
	f.call("root", "default", "POST", base+"/drafts/"+draft+"/apply", nil, 200)
	var web, database string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT workload_id FROM configuration_resource_keys WHERE project_id='manifest-project' AND key='web'`).Scan(&web); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(context.Background(), `SELECT workload_id FROM configuration_resource_keys WHERE project_id='manifest-project' AND key='database'`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	values, err := f.s.resolveSharedVariables(context.Background(), web, map[string]string{"PASSWORD": "{{resource.DB_PASSWORD}}"})
	if err != nil || values["PASSWORD"] != "manifest-secret" {
		t.Fatal("manifest binding did not resolve", err)
	}
	var count int
	if err = f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM operations WHERE workload_id IN ($1,$2)`, web, database).Scan(&count); err != nil || count != 0 {
		t.Fatal("applying a configuration draft deployed a resource", err)
	}
	manifest.Resources[0].Configuration.Name = "Must roll back"
	manifest.Resources[1].Bindings[0].Key = "MISSING"
	raw, _ = json.Marshal(manifest)
	draft = jsonString(t, f.call("root", "default", "POST", base+"/drafts", map[string]string{"content": string(raw)}, 201), "id")
	f.call("root", "default", "POST", base+"/drafts/"+draft+"/apply", nil, 400)
	var name string
	if err = f.s.DB.QueryRow(context.Background(), `SELECT name FROM workloads WHERE id=$1`, database).Scan(&name); err != nil || name != "Database" {
		t.Fatal("failed apply was not atomic", err)
	}
}
