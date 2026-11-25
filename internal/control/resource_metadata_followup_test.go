package control

import (
	"encoding/json"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestManagedRegistryManagementFlagHonorsTokenScopes(t *testing.T) {
	f, _, owner := publicFixture(t)
	for _, scenario := range []struct {
		name, scope string
		manage      bool
	}{
		{"read only", "", false},
		{"unrelated write", "secrets:write", false},
		{"provision and remove", "workloads:write", true},
		{"configure", "configuration:write", true},
		{"maintenance", "workloads:operate", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			scopes := []string{"workloads:read"}
			if scenario.scope != "" {
				scopes = append(scopes, scenario.scope)
			}
			_, token := mintPublic(t, f, owner, "user", scopes...)
			response := publicCall(t, f, token, "default", "GET", "/api/v1/managed-registries", nil, 200)
			var status struct {
				CanManage bool `json:"can_manage"`
			}
			if json.Unmarshal(response.Body.Bytes(), &status) != nil || status.CanManage != scenario.manage {
				t.Fatal("management metadata did not respect the token's registry scopes", response.Body.String())
			}
			if scenario.scope == "" {
				publicCall(t, f, token, "default", "POST", "/api/v1/managed-registries", map[string]any{}, 403)
				publicCall(t, f, token, "default", "PUT", "/api/v1/managed-registries/missing", map[string]any{}, 403)
				publicCall(t, f, token, "default", "DELETE", "/api/v1/managed-registries/missing", map[string]any{}, 403)
				publicCall(t, f, token, "default", "POST", "/api/v1/managed-registries/missing/gc", nil, 403)
			}
		})
	}
	for _, cookie := range []string{"root", owner} {
		response := f.call(cookie, "default", "GET", "/api/managed-registries", nil, 200)
		if !strings.Contains(response.Body.String(), `"can_manage":true`) {
			t.Fatal("administrator session lost management", response.Body.String())
		}
	}
	_, token := mintPublic(t, f, owner, "user", "workloads:read", "workloads:write", "configuration:write", "workloads:operate")
	f.sql(`UPDATE team_members SET role='member',grants='[{"scope":"team","role":"viewer"}]' WHERE user_id='public-owner'`)
	response := publicCall(t, f, token, "default", "GET", "/api/v1/managed-registries", nil, 200)
	if !strings.Contains(response.Body.String(), `"can_manage":false`) {
		t.Fatal("token scopes overrode current team rights", response.Body.String())
	}
}

func TestProjectDraftOnlyRecordsChangedResourceConfiguration(t *testing.T) {
	f, service := variablesFixture(t)
	job := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Job", NodeID: "variable-node", ProjectID: "variable-project", Kind: "job", JobService: "task", Compose: "services:\n  task:\n    image: busybox:1.37\n    command: [true]\n"}, 201), "id")
	base := "/api/projects/variable-project/configuration"
	var current struct {
		Manifest projectConfiguration `json:"manifest"`
	}
	if json.Unmarshal(f.call("root", "default", "GET", base, nil, 200).Body.Bytes(), &current) != nil {
		t.Fatal("invalid project configuration")
	}
	var jobKey string
	for i := range current.Manifest.Resources {
		if current.Manifest.Resources[i].ID == job || current.Manifest.Resources[i].Configuration.Name == "Job" {
			current.Manifest.Resources[i].Configuration.Compose += "# changed job only\n"
			jobKey = current.Manifest.Resources[i].Key
		}
	}
	if jobKey == "" {
		t.Fatal("job missing from manifest")
	}
	count := func(id string) int {
		t.Helper()
		var n int
		if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM configuration_history WHERE workload_id=$1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	serviceBefore, jobBefore := count(service), count(job)
	raw, err := json.Marshal(current.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		response := f.call("root", "default", "POST", base+"/drafts", map[string]string{"content": string(raw)}, 201)
		if attempt == 0 {
			var draft struct {
				Changes []configurationChange `json:"changes"`
			}
			if json.Unmarshal(response.Body.Bytes(), &draft) != nil || len(draft.Changes) != 1 || draft.Changes[0].Resource != jobKey || len(draft.Changes[0].Fields) != 1 || draft.Changes[0].Fields[0] != "compose" || len(draft.Changes[0].Diffs) != 0 {
				t.Fatal("Compose metadata diff or secret-safe omission changed", response.Body.String())
			}
		}
		f.call("root", "default", "POST", base+"/drafts/"+jsonString(t, response, "id")+"/apply", nil, 200)
		if got := count(service); got != serviceBefore {
			t.Fatal("job-only draft created untouched-service history", got, serviceBefore)
		}
		if got := count(job); got != jobBefore+1 {
			t.Fatal("changed or no-op draft recorded the wrong history", got, jobBefore)
		}
	}
}

func TestPreviewCIMutableTagErrorNamesCIAndLeavesNoPreview(t *testing.T) {
	f, base, token, event := newCIPreviewFixture(t)
	event.Images[base]["web"] = "registry.example.test/team/app:latest"
	response := ciSubmit(t, f, token, event, 400)
	if !strings.Contains(response.Body.String(), "CI preview images") || !strings.Contains(response.Body.String(), "digests") || strings.Contains(response.Body.String(), "promotion") {
		t.Fatal("CI input refusal names the wrong feature", response.Body.String())
	}
	var count int
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM environment_previews`).Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid CI input created preview state", count, err)
	}
}

func TestBuildFullRegistryReferenceRefusedBeforeQueueing(t *testing.T) {
	f, id := variablesFixture(t)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"build_publish":true,"os":"linux","arch":"amd64"}'::jsonb WHERE id='variable-node'`)
	registry := jsonString(t, f.call("root", "default", "POST", "/api/registries", connectionConfig{Name: "Build", Registry: &worker.Registry{Server: "registry.example.test", Username: "fixture", Password: "fixture-password"}}, 201), "id")
	f.call("root", "default", "PUT", "/api/workloads/"+id+"/registry", map[string]string{"registry_id": registry}, 200)
	response := f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", apiOperationInput{Action: "build", Build: &worker.BuildOptions{Repository: "registry.example.test/team/app"}}, 400)
	if !strings.Contains(response.Body.String(), "without a registry host") {
		t.Fatal("qualified repository refusal is not actionable", response.Body.String())
	}
	var count int
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM operations WHERE workload_id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid build reached the operation queue", count, err)
	}
}

func TestImageUpdatePolicySaveWaitsForPostDeployCheckButCanDisable(t *testing.T) {
	f, id := variablesFixture(t)
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"image_updates":true,"image_lifecycle":true}'::jsonb WHERE id='variable-node'`)
	path := "/api/workloads/" + id + "/image-update-policy"
	policy := worker.ImageUpdatePolicy{Enabled: true, Mode: "notify", Rules: []worker.ImageUpdateRule{{Service: "app", Range: "^1.37.0"}}}
	f.call("root", "default", "PUT", path, policy, 200)
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES('post-deploy-check',$1,'image_check','running',$2)`, id, []byte{})
	response := f.call("root", "default", "PUT", path, policy, 409)
	if !strings.Contains(response.Body.String(), "wait for the active operation") {
		t.Fatal("active-check hold was not explained", response.Body.String())
	}
	policy.Enabled = false
	f.call("root", "default", "PUT", path, policy, 200)
	f.sql(`UPDATE operations SET state='succeeded' WHERE id='post-deploy-check'`)
	policy.Enabled = true
	f.call("root", "default", "PUT", path, policy, 200)
}
