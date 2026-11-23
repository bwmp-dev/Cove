package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"strings"
	"testing"
)

func TestReleaseConfigurationDiffNeverReturnsSecretValues(t *testing.T) {
	a := worker.Request{Compose: "services: {app: {environment: {PASSWORD: very-private-value}}}", Environment: map[string]string{"PASSWORD": "old-secret", "UNCHANGED": "same-secret"}, Source: &worker.GitSource{Repository: "https://git.example/repo", Token: "old-token"}}
	b := a
	b.Compose = "services: {app: {image: new}}"
	b.Environment = map[string]string{"PASSWORD": "new-secret", "UNCHANGED": "same-secret"}
	source := *a.Source
	source.Token = "new-token"
	b.Source = &source
	raw, err := json.Marshal(compareConfiguration(a, b))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"very-private-value", "old-secret", "same-secret", "new-secret", "old-token", "new-token"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("diff exposed a secret")
		}
	}
	if !strings.Contains(string(raw), `"field":"variables.PASSWORD","changed":true`) || !strings.Contains(string(raw), `"field":"variables.UNCHANGED","changed":false`) {
		t.Fatal(string(raw))
	}
}

func TestReleaseDiffScopesAndRollbackConfigurationIntegration(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('diff-node','Node','localhost',22,'',$1,'ready','{"release_diff":true}')`, []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('diff-project','Project','default')")
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "App", NodeID: "diff-node", ProjectID: "diff-project", Compose: "services: {}"}, 201), "id")
	for operation, request := range map[string]worker.Request{
		"diff-from":     {Action: "deploy", Compose: "services: {}", Environment: map[string]string{"PASSWORD": "{{team.PASSWORD}}"}},
		"diff-to":       {Action: "deploy", Compose: "services: {}", Environment: map[string]string{"PASSWORD": "{{team.PASSWORD}}"}},
		"diff-rollback": {Action: "rollback", Release: "diff-from", Environment: map[string]string{"PASSWORD": "second-secret"}},
	} {
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := seal(f.s.Key, raw)
		if err != nil {
			t.Fatal(err)
		}
		f.sql("INSERT INTO operations(id,workload_id,action,state,payload) VALUES($1,$2,$3,'succeeded',$4)", operation, id, request.Action, payload)
	}
	f.s.workerRPC = func(_ context.Context, node string, request worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "diff-node" || request.Method != "release_comparison" || request.Diff.From != "diff-from" || request.Diff.To != "diff-rollback" && request.Diff.To != "diff-to" {
			t.Fatal("wrong retained releases requested", node, request)
		}
		return worker.RPCResponse{Comparison: &worker.ReleaseComparison{EnvironmentChanges: map[string]bool{"PASSWORD": request.Diff.To == "diff-to"}, Images: []worker.ImageChange{}, Commits: []worker.ReleaseCommit{}}}, nil
	}
	path := "/api/workloads/" + id + "/release-diff?from=diff-from&to=diff-rollback"
	member := f.user("diff-member", "diff-member@example.com")
	f.sql("INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','diff-member','member','[]')")
	f.call(member, "default", "GET", path, nil, 403)
	w := f.call("root", "default", "GET", path, nil, 200)
	if strings.Contains(w.Body.String(), "first-secret") || strings.Contains(w.Body.String(), "second-secret") || !strings.Contains(w.Body.String(), `"field":"variables.PASSWORD","changed":false`) {
		t.Fatal("rollback must compare referenced configuration without disclosing values", w.Body.String())
	}
	w = f.call("root", "default", "GET", "/api/workloads/"+id+"/release-diff?from=diff-from&to=diff-to", nil, 200)
	if !strings.Contains(w.Body.String(), `"field":"variables.PASSWORD","changed":true`) || strings.Contains(w.Body.String(), "{{team.PASSWORD}}") || strings.Contains(w.Body.String(), "environment_changes") {
		t.Fatal("resolved variable changes must merge into configuration flags without exposing snapshots", w.Body.String())
	}
}
