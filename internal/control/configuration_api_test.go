package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestProjectConfigurationPublicScopesAndArchiveSpec(t *testing.T) {
	f, id, cookie := publicFixture(t)
	_, reader := mintPublic(t, f, cookie, "user", "projects:read", "workloads:read")
	path := "/api/v1/projects/variable-project/configuration"
	get := publicCall(t, f, reader, "default", "GET", path, nil, 200)
	var current struct {
		Manifest projectConfiguration `json:"manifest"`
	}
	if json.Unmarshal(get.Body.Bytes(), &current) != nil {
		t.Fatal("invalid canonical API response")
	}
	raw, _ := json.Marshal(current.Manifest)
	input := map[string]string{"content": string(raw)}
	publicCall(t, f, reader, "default", "POST", path+"/drafts", input, 403)
	publicCall(t, f, reader, "default", "POST", path+"/export", map[string]string{}, 403)
	_, writer := mintPublic(t, f, cookie, "user", "configuration:write")
	draft := jsonString(t, publicCall(t, f, writer, "default", "POST", path+"/drafts", input, 201), "id")
	publicCall(t, f, writer, "default", "GET", path+"/drafts", nil, 200)
	publicCall(t, f, writer, "default", "POST", path+"/export", map[string]string{}, 403)
	publicCall(t, f, writer, "default", "POST", path+"/drafts/"+draft+"/apply", nil, 200)
	_, secretReader := mintPublic(t, f, cookie, "user", "secrets:read")
	exported := publicCall(t, f, secretReader, "default", "POST", path+"/export", map[string]string{}, 200)
	if exported.Header().Get("Content-Type") != "application/zip" {
		t.Fatal("public API did not return a ZIP archive")
	}
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"drift":true}'::jsonb WHERE id='variable-node'`)
	f.s.workerRPC = func(_ context.Context, _ string, request worker.RPCRequest) (worker.RPCResponse, error) {
		if request.Method != "drift" || request.ID != id {
			t.Fatal("invalid drift request")
		}
		return worker.RPCResponse{Drift: &worker.DriftReport{Busy: true, Changes: []worker.DriftChange{}}}, nil
	}
	drift := publicCall(t, f, reader, "default", "GET", "/api/v1/workloads/"+id+"/drift", nil, 200)
	if !strings.Contains(drift.Body.String(), `"busy":true`) {
		t.Fatal("deferred drift inspection claimed a clean baseline")
	}
	document := f.s.openAPIDocument()
	paths := document["paths"].(map[string]any)
	operation := paths["/projects/{id}/configuration/export"].(map[string]any)["post"].(map[string]any)
	response := operation["responses"].(map[string]any)["200"].(map[string]any)
	if response["content"].(map[string]any)["application/zip"] == nil {
		t.Fatal("OpenAPI does not document the real binary export response")
	}
}
