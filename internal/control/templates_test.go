package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"cove/internal/catalog"
	"cove/internal/outbound"
)

func TestTemplateCatalogueScopesRefreshAndDraft(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("catalog-owner", "owner@example.com")
	viewer := f.user("catalog-viewer", "viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','catalog-owner','owner','[]'),('default','catalog-viewer','member','[{"scope":"team","role":"viewer"}]')`)
	f.sql("INSERT INTO teams(id,name) VALUES('other','Other')")
	f.call("", "", "GET", "/api/templates", nil, 401)
	f.call(viewer, "default", "POST", "/api/settings/template-repositories", nil, 403)
	f.call(viewer, "default", "GET", "/api/teams/default/template-repositories", nil, 403)
	f.call(owner, "default", "POST", "/api/teams/other/template-repositories", nil, 403)
	for _, address := range []string{"http://example.com/index.json", "https://127.0.0.1/index.json", "https://[::1]/index.json", "https://localhost/index.json", "https://example.com/index.json?token=hidden", "https://user:pass@example.com/index.json"} {
		f.call("root", "", "POST", "/api/settings/template-repositories", map[string]string{"name": "Unsafe", "kind": "index", "url": address}, 400)
	}
	index, builtinRevision, err := catalog.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	custom := index.Templates[0]
	custom.ID = "team-postgres"
	data, err := json.Marshal(catalog.Index{SchemaVersion: 1, Templates: []catalog.Template{custom}})
	if err != nil {
		t.Fatal(err)
	}
	f.s.templateFetch = func(context.Context, string, string, string) ([]byte, string, error) { return data, "pinned-one", nil }
	path := "/api/teams/default/template-repositories"
	id := jsonString(t, f.call(owner, "default", "POST", path, map[string]string{"name": "Team catalogue", "kind": "index", "url": "https://example.com/index.json"}, 200), "id")
	f.call(owner, "default", "POST", path+"/"+id+"/refresh", nil, 200)
	w := f.call(viewer, "default", "GET", "/api/templates", nil, 200)
	if !strings.Contains(w.Body.String(), `"id":"team-postgres"`) || !strings.Contains(w.Body.String(), `"revision":"pinned-one"`) || !strings.Contains(w.Body.String(), `"repository_name":"Team catalogue"`) || !strings.Contains(w.Body.String(), `"repository_url":"https://example.com/index.json"`) {
		t.Fatal("validated team snapshot not offered")
	}
	w = f.call("root", "other", "GET", "/api/templates", nil, 200)
	if strings.Contains(w.Body.String(), "team-postgres") {
		t.Fatal("team catalogue leaked across teams")
	}
	f.s.templateFetch = func(context.Context, string, string, string) ([]byte, string, error) {
		return []byte(`{"schema_version":2}`), "bad", nil
	}
	f.call(owner, "default", "POST", path+"/"+id+"/refresh", nil, 422)
	w = f.call(viewer, "default", "GET", "/api/templates", nil, 200)
	if !strings.Contains(w.Body.String(), "pinned-one") || !strings.Contains(w.Body.String(), "last validated revision") {
		t.Fatal("bad refresh replaced last good snapshot or omitted warning")
	}
	f.sql("UPDATE template_repositories SET refresh_until=now()+interval '1 minute' WHERE id=$1", id)
	f.call(owner, "default", "POST", path+"/"+id+"/refresh", nil, 409)
	f.sql("UPDATE template_repositories SET refresh_until=now(),next_refresh_at=now() WHERE id=$1", id)
	f.s.templateFetch = func(context.Context, string, string, string) ([]byte, string, error) { return data, "pinned-two", nil }
	if err = f.s.refreshTemplateRepositories(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.call(owner, "default", "POST", "/api/templates/draft", map[string]any{"origin": "repository", "repository_id": id, "id": custom.ID, "revision": "pinned-one"}, 409)
	draft := map[string]any{"origin": "builtin", "id": index.Templates[0].ID, "revision": builtinRevision, "hostname": "app.example.com"}
	first := f.call(viewer, "default", "POST", "/api/templates/draft", draft, 200).Body.String()
	second := f.call(viewer, "default", "POST", "/api/templates/draft", draft, 200).Body.String()
	if first == second {
		t.Fatal("draft did not generate independent secrets")
	}
	f.call(owner, "default", "DELETE", path+"/"+id, nil, 200)
	var audits int
	if err = f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE subject=$1 AND action IN ('add template repository','refresh template repository','remove template repository')", id).Scan(&audits); err != nil || audits != 5 {
		t.Fatal("repository changes were not audited", audits, err)
	}
	globalPath := "/api/settings/template-repositories"
	globalID := jsonString(t, f.call("root", "", "POST", globalPath, map[string]string{"name": "Instance catalogue", "kind": "git", "url": "https://example.com/catalogue.git", "ref": "main"}, 200), "id")
	f.call("root", "", "POST", globalPath+"/"+globalID+"/refresh", nil, 200)
	for _, team := range []string{"default", "other"} {
		w = f.call("root", team, "GET", "/api/templates", nil, 200)
		if !strings.Contains(w.Body.String(), `"repository_id":"`+globalID+`"`) {
			t.Fatal("instance catalogue was not visible to all teams")
		}
	}
	f.call(owner, "default", "DELETE", path+"/"+globalID, nil, 404)
	f.s.templateFetch = func(context.Context, string, string, string) ([]byte, string, error) {
		f.call("root", "", "DELETE", globalPath+"/"+globalID, nil, 200)
		return data, "removed-during-refresh", nil
	}
	f.call("root", "", "POST", globalPath+"/"+globalID+"/refresh", nil, 409)
}

func TestSaveWorkloadAsTemplateStripsSecretsAndPinsSource(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("template-owner", "owner@example.com")
	editor := f.user("template-editor", "editor@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','template-owner','owner','[]'),('default','template-editor','member','[{"scope":"team","role":"editor"}]')`)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('template-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('template-project','Project','default')")
	config := workloadInput{Name: "Source", NodeID: "template-node", ProjectID: "template-project", Compose: "services:\n  app:\n    image: postgres:17.12-alpine\n    environment:\n      PASSWORD: very-private-compose-value\n    volumes: [data:/var/lib/postgresql/data]\nvolumes:\n  data: {}\n", Environment: map[string]string{"PASSWORD": "very-private-environment-value"}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	index, _, _ := catalog.Builtin()
	metadata := index.Templates[0]
	metadata.ID = "saved-postgres"
	input := map[string]any{"template": metadata}
	path := "/api/workloads/" + id + "/template"
	f.call(editor, "default", "POST", path, input, 403)
	preview := f.call(owner, "default", "POST", path, input, 200)
	if strings.Contains(preview.Body.String(), "very-private") || !strings.Contains(preview.Body.String(), "SETTING_1") {
		t.Fatal("source secrets leaked or were not converted")
	}
	revision := jsonString(t, preview, "configuration_revision")
	input["confirm"], input["configuration_revision"] = true, "stale"
	f.call(owner, "default", "POST", path, input, 409)
	input["configuration_revision"] = revision
	f.call(owner, "default", "POST", path, input, 201)
	f.call(owner, "default", "POST", path, input, 409)
	w := f.call(owner, "default", "GET", "/api/templates", nil, 200)
	if !strings.Contains(w.Body.String(), `"id":"saved-postgres"`) || strings.Contains(w.Body.String(), "very-private") {
		t.Fatal("saved catalogue invalid or leaked secrets")
	}
	config.Environment["PASSWORD"] = "changed-private-value"
	config.ProjectID = ""
	f.call("root", "default", "PUT", "/api/workloads/"+id, config, 200)
	metadata.ID = "changed-postgres"
	input["template"] = metadata
	f.call(owner, "default", "POST", path, input, 409)
	f.call(editor, "default", "DELETE", "/api/teams/default/templates/saved-postgres", nil, 403)
	f.call(owner, "default", "DELETE", "/api/teams/default/templates/saved-postgres", nil, 200)
	var audits int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE subject='saved-postgres' AND action IN ('save workload as template','delete team template')").Scan(&audits); err != nil || audits != 2 {
		t.Fatal("team template writes were not audited", audits, err)
	}
	f.sql("INSERT INTO environments(id,project_id,name,team_id) VALUES('template-preview-env','template-project','Preview','default')")
	f.sql("INSERT INTO previews(source_id,number,workload_id,environment_id,revision,expires_at) VALUES($1,1,$1,'template-preview-env','revision',now()+interval '1 hour')", id)
	f.call(owner, "default", "POST", path, map[string]any{"template": metadata}, 409)
}

func TestTemplateFetchBlocksPrivateDestinationsWithProxy(t *testing.T) {
	transport := publicWebhookTransport(outbound.Config{HTTPS: "http://127.0.0.1:1"})
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	for _, target := range []string{"https://127.0.0.1/index.json", "https://[::1]/index.json", "https://169.254.169.254/latest/meta-data", "https://localhost/index.json"} {
		_, err := client.Get(target)
		if err == nil {
			t.Fatal("private destination accepted through proxy", target)
		}
		if !strings.Contains(err.Error(), "public") && !strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "forbidden") {
			t.Fatal("request reached proxy instead of rejecting private destination", err)
		}
	}
	s := &Server{}
	for _, args := range [][3]string{{"file:///etc/passwd", "main", "compose.yaml"}, {"https://example.com/repo", "--upload-pack=evil", "compose.yaml"}, {"https://example.com/repo", "main", "../secret"}} {
		if _, _, err := s.fetchTemplateGitFile(context.Background(), args[0], args[1], args[2], "", transport); err == nil {
			t.Fatal("unsafe Git input accepted")
		}
	}
}

func TestPublicTemplateGitFetch(t *testing.T) {
	if os.Getenv("COVE_TEST_TEMPLATE_GIT") != "1" {
		t.Skip("opt-in public GitHub integration: COVE_TEST_TEMPLATE_GIT=1")
	}
	transport := publicWebhookTransport(outbound.Environment())
	defer transport.CloseIdleConnections()
	s := &Server{}
	data, revision, err := s.fetchTemplateGitFile(context.Background(), "https://github.com/plausible/community-edition.git", "v3.2.1", "compose.yml", "", transport)
	if err != nil || len(revision) != 40 || !strings.Contains(string(data), "plausible:") {
		t.Fatal("public pinned Git fetch failed", err)
	}
	if _, _, err = s.fetchTemplateGitFile(context.Background(), "https://127.0.0.1/repo", "main", "compose.yaml", "", transport); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("private Git destination was not rejected before fetching", err)
	}
}
