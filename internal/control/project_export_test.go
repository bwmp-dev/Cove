package control

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestProjectExportAndEnvironmentComparison(t *testing.T) {
	f, id := variablesFixture(t)
	base := "/api/projects/variable-project/configuration"
	exported := f.call("root", "default", "POST", base+"/export", map[string]string{}, 200)
	if exported.Header().Get("Content-Type") != "application/zip" || exported.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("archive response must be private")
	}
	reader, err := zip.NewReader(bytes.NewReader(exported.Body.Bytes()), int64(exported.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, entry := range reader.File {
		if entry.Mode().Perm() != 0600 {
			t.Fatal("archive secret files have public permissions")
		}
		stream, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, readErr := io.ReadAll(stream)
		closeErr := stream.Close()
		if readErr != nil || closeErr != nil {
			t.Fatal(readErr, closeErr)
		}
		files[entry.Name] = string(raw)
	}
	manifest, err := parseProjectConfiguration(files["cove.yaml"])
	if err != nil || len(manifest.Resources) != 1 {
		t.Fatal("exported canonical schema cannot be imported", err)
	}
	if !strings.Contains(files["resources/"+id+"/.env"], "saved-secret") || !strings.Contains(files["resources/"+id+"/compose.yaml"], "busybox:1.37") {
		t.Fatal("plain Compose and environment not exported")
	}
	var audit bool
	if err = f.s.DB.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action='project configuration and secrets exported' AND subject='variable-project')`).Scan(&audit); err != nil || !audit {
		t.Fatal("project export not audited", err)
	}
	f.sql(`INSERT INTO environments(id,name,project_id,team_id) VALUES('compare-dev','Development','variable-project','default')`)
	config := manifest.Resources[0].Configuration
	config.EnvironmentID, config.ProjectID = "compare-dev", "variable-project"
	config.Environment = map[string]string{"KEEP": "different-value", "SECRET": "comparison-secret"}
	f.call("root", "default", "POST", "/api/workloads", config, 201)
	result := f.call("root", "default", "GET", base+"/compare?from=variable-env&to=compare-dev", nil, 200)
	if !strings.Contains(result.Body.String(), "environment.SECRET") || strings.Contains(result.Body.String(), "comparison-secret") || strings.Contains(result.Body.String(), "saved-secret") {
		t.Fatal("environment comparison leaked values or missed differences")
	}
	viewer := f.user("export-viewer", "export-viewer@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','export-viewer','member','[{"scope":"environment","scope_id":"variable-env","role":"viewer"}]')`)
	f.call(viewer, "default", "POST", base+"/export", map[string]string{}, 403)
	f.call(viewer, "default", "GET", base+"/compare?from=variable-env&to=compare-dev", nil, 403)
	f.sql(`UPDATE team_members SET grants='[{"scope":"environment","scope_id":"variable-env","role":"viewer"},{"scope":"environment","scope_id":"compare-dev","role":"viewer"}]' WHERE user_id='export-viewer'`)
	f.call(viewer, "default", "GET", base+"/compare?from=variable-env&to=compare-dev", nil, 200)
}

func TestProjectExportPinsRepositoryFilesWithoutCloneCredentials(t *testing.T) {
	f, id := variablesFixture(t)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"source_files":true}'::jsonb WHERE id='variable-node'`)
	config := workloadInput{Name: "Variables", NodeID: "variable-node", Source: &worker.GitSource{Repository: "https://fixture.invalid/app.git", Branch: "main", ComposePath: "compose.yaml", Token: "clone-only-private-credential"}}
	f.call("root", "default", "PUT", "/api/workloads/"+id, config, 200)
	revision := strings.Repeat("a", 40)
	f.s.workerRPC = func(_ context.Context, _ string, request worker.RPCRequest) (worker.RPCResponse, error) {
		if request.Method != "source_files" || request.Request.Source.Token != "clone-only-private-credential" {
			t.Fatal("source export did not use private worker clone credentials")
		}
		return worker.RPCResponse{SourceFiles: &worker.SourceFiles{Revision: revision, Files: map[string]string{"compose.yaml": "services:\n  app:\n    image: busybox\n    networks: [private]\nnetworks:\n  private:\n    name: null\n"}}}, nil
	}
	exported := f.call("root", "default", "POST", "/api/projects/variable-project/configuration/export", map[string]string{}, 200)
	reader, err := zip.NewReader(bytes.NewReader(exported.Body.Bytes()), int64(exported.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range reader.File {
		stream, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, readErr := io.ReadAll(stream)
		closeErr := stream.Close()
		if readErr != nil || closeErr != nil || strings.Contains(string(raw), "clone-only-private-credential") {
			t.Fatal("export exposed managed clone credentials", readErr, closeErr)
		}
		if entry.Name == "cove.yaml" {
			manifest, err := parseProjectConfiguration(string(raw))
			if err != nil || manifest.Resources[0].Configuration.Source.Revision != revision || manifest.Resources[0].Configuration.Source.Token != "" || manifest.Resources[0].Revision != "" {
				t.Fatal("export did not pin portable source metadata", err)
			}
			found = true
		}
		if strings.HasSuffix(entry.Name, "/compose.yaml") && strings.Contains(string(raw), "name: null") {
			t.Fatal("plain Compose export retained an incompatible null network name")
		}
	}
	if !found {
		t.Fatal("canonical source declaration missing")
	}
}
