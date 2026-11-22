package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"strings"
	"testing"
)

func TestResourceMoveReviewTransferAndRevalidation(t *testing.T) {
	f, id := variablesFixture(t)
	f.sql(`INSERT INTO teams(id,name) VALUES('move-team','Destination')`)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('move-project','Destination','move-team')`)
	f.sql(`INSERT INTO environments(id,name,project_id,team_id) VALUES('move-env','Staging','move-project','move-team')`)
	path := "/api/workloads/" + id + "/move"
	in := resourceMoveInput{TeamID: "move-team", ProjectID: "move-project", EnvironmentID: "move-env"}
	preview := func() resourceMoveReview {
		t.Helper()
		response := f.call("root", "default", "POST", path+"/preview", in, 200)
		if strings.Contains(response.Body.String(), "saved-secret") {
			t.Fatal("review exposed secrets")
		}
		var review resourceMoveReview
		if err := json.Unmarshal(response.Body.Bytes(), &review); err != nil || review.Version == "" {
			t.Fatal("invalid review", err)
		}
		return review
	}
	review := preview()
	if !review.CrossTeam || len(review.Blockers) > 0 {
		t.Fatal(review)
	}
	in.Version, in.Acknowledge = review.Version, true

	f.call("root", "default", "POST", "/api/workloads/"+id+"/variables/import", map[string]string{"dotenv": "SECRET=changed"}, 200)
	f.call("root", "default", "POST", path, in, 409)
	review = preview()
	in.Version = review.Version
	f.sql(`INSERT INTO api_tokens(id,name,token_hash,workload_id,team_id,permission,expires_at) VALUES('move-token','old',$1,$2,'default','read',now()+interval '1 day')`, []byte("hash"), id)
	f.call("root", "default", "POST", path, in, 200)
	var team, project, environment string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT team_id,project_id,environment_id FROM workloads WHERE id=$1`, id).Scan(&team, &project, &environment); err != nil || team != in.TeamID || project != in.ProjectID || environment != in.EnvironmentID {
		t.Fatal("placement not transferred", err)
	}
	f.call("root", "default", "GET", "/api/workloads/"+id, nil, 404)
	response := f.call("root", "move-team", "POST", "/api/workloads/"+id+"/variables/export", nil, 200)
	if !strings.Contains(response.Body.String(), "changed") {
		t.Fatal("local secret lost")
	}
	var tokens, operations, audits int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM api_tokens WHERE workload_id=$1),(SELECT count(*) FROM operations WHERE workload_id=$1),(SELECT count(*) FROM audit_events WHERE subject=$1 AND action='resource.move')`, id).Scan(&tokens, &operations, &audits); err != nil || tokens != 0 || operations != 0 || audits != 2 {
		t.Fatal("transfer effects incorrect", tokens, operations, audits, err)
	}
}

func TestResourceMoveBlocksReferencesAndRequiresBothSides(t *testing.T) {
	f, id := variablesFixture(t)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('move-other','Other','default')`)
	user := f.user("move-editor", "move-editor@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','move-editor','member','[{"scope":"project","scope_id":"variable-project","role":"editor"}]')`)
	in := resourceMoveInput{TeamID: "default", ProjectID: "move-other"}
	path := "/api/workloads/" + id + "/move"
	f.call(user, "default", "POST", path+"/preview", in, 403)
	f.call("root", "default", "POST", "/api/workloads/"+id+"/variables/import", map[string]string{"dotenv": "SHARED={{environment.KEY}}"}, 200)
	var review resourceMoveReview
	response := f.call("root", "default", "POST", path+"/preview", in, 200)
	if json.Unmarshal(response.Body.Bytes(), &review) != nil || len(review.Blockers) != 1 || !strings.Contains(review.Blockers[0], "shared variable") {
		t.Fatal(response.Body.String())
	}
	in.Version, in.Acknowledge = review.Version, true
	f.call("root", "default", "POST", path, in, 409)
}

func TestResourceMoveCannotExpandTeamScopedToken(t *testing.T) {
	f, id, cookie := publicFixture(t)
	f.sql(`INSERT INTO teams(id,name) VALUES('move-target','Target')`)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('move-target-project','Target','move-target')`)
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('move-target','public-owner','owner')`)
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('move-target','variable-node')`)
	_, teamToken := mintPublic(t, f, cookie, "team", "configuration:write")
	_, userToken := mintPublic(t, f, cookie, "user", "configuration:write")
	input := resourceMoveInput{TeamID: "move-target", ProjectID: "move-target-project"}
	path := "/api/v1/workloads/" + id + "/move/preview"
	publicCall(t, f, teamToken, "default", "POST", path, input, 403)
	publicCall(t, f, userToken, "default", "POST", path, input, 200)
	f.sql(`UPDATE team_members SET role='member',grants='[]' WHERE team_id='move-target'`)
	publicCall(t, f, userToken, "default", "POST", path, input, 403)
}

func TestResourceMoveWaitsForDeployedPrivateNetworkRemoval(t *testing.T) {
	f, id := variablesFixture(t)
	f.sql(`INSERT INTO teams(id,name) VALUES('network-target','Target')`)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('network-target-project','Target','network-target')`)
	payload, err := f.s.sealJSON(worker.Request{ID: "old-private", Workload: id, Action: "deploy", Private: &worker.PrivateNetwork{}})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES('old-private',$1,'deploy','succeeded',$2)`, id, payload)
	in := resourceMoveInput{TeamID: "network-target", ProjectID: "network-target-project"}
	path := "/api/workloads/" + id + "/move"
	var review resourceMoveReview
	if json.Unmarshal(f.call("root", "default", "POST", path+"/preview", in, 200).Body.Bytes(), &review) != nil || len(review.Blockers) != 1 || !strings.Contains(review.Blockers[0], "applied on the worker") {
		t.Fatal("saved settings alone released the source network")
	}
	in.Version, in.Acknowledge = review.Version, true
	f.call("root", "default", "POST", path, in, 409)
	cleared, err := f.s.sealJSON(worker.Request{ID: "cleared-private", Workload: id, Action: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES('cleared-private',$1,'deploy','succeeded',$2)`, id, cleared)
	if json.Unmarshal(f.call("root", "default", "POST", path+"/preview", in, 200).Body.Bytes(), &review) != nil || len(review.Blockers) != 0 {
		t.Fatal("applied network removal did not unblock transfer")
	}
}

func TestResourceMoveAllowsDisabledPreviewPolicy(t *testing.T) {
	f, id := variablesFixture(t)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('preview-move-target','Target','default')`)
	payload, err := f.s.sealJSON(previewPolicy{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO preview_policies(workload_id,configuration) VALUES($1,$2)`, id, payload)
	in := resourceMoveInput{TeamID: "default", ProjectID: "preview-move-target"}
	path := "/api/workloads/" + id + "/move/preview"
	var review resourceMoveReview
	if json.Unmarshal(f.call("root", "default", "POST", path, in, 200).Body.Bytes(), &review) != nil || len(review.Blockers) != 1 || !strings.Contains(review.Blockers[0], "Disable the resource preview policy") {
		t.Fatal("enabled previews did not block movement")
	}
	payload, err = f.s.sealJSON(previewPolicy{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE preview_policies SET configuration=$2 WHERE workload_id=$1`, id, payload)
	if json.Unmarshal(f.call("root", "default", "POST", path, in, 200).Body.Bytes(), &review) != nil || len(review.Blockers) != 0 {
		t.Fatal("disabled policy could not be cleared through the existing UI")
	}
}

func TestResourceMoveExcludesInFlightAuthorizedChanges(t *testing.T) {
	f, id := variablesFixture(t)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('guard-target','Target','default')`)
	input := resourceMoveInput{TeamID: "default", ProjectID: "guard-target"}
	path := "/api/workloads/" + id + "/move"
	var review resourceMoveReview
	if json.Unmarshal(f.call("root", "default", "POST", path+"/preview", input, 200).Body.Bytes(), &review) != nil {
		t.Fatal("invalid review")
	}
	input.Version, input.Acknowledge = review.Version, true
	tx, err := f.s.DB.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(context.Background(), `SELECT pg_advisory_xact_lock_shared(hashtextextended($1,0))`, "cove-resource-access:"+id); err != nil {
		t.Fatal(err)
	}
	f.call("root", "default", "POST", path, input, 409)
	if err = tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.call("root", "default", "POST", path, input, 200)
}

func TestResourceChangesRespectPlacementGuardForAllCredentials(t *testing.T) {
	f, id, cookie := publicFixture(t)
	_, token := mintPublic(t, f, cookie, "user", "workloads:write")
	legacy := jsonString(t, f.call(cookie, "default", "POST", "/api/tokens", map[string]any{"name": "Guard fixture", "workload_id": id, "permission": "operate", "days": 1}, 201), "token")
	tx, err := f.s.DB.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(context.Background(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "cove-resource-access:"+id); err != nil {
		t.Fatal(err)
	}
	in := workloadInput{Name: "Must not write", NodeID: "variable-node", Compose: "services: {}"}
	path := "/api/workloads/" + id
	f.call(cookie, "default", "PUT", path, in, 409)
	f.call("root", "default", "PUT", path, in, 409)
	publicCall(t, f, token, "default", "PUT", "/api/v1/workloads/"+id, in, 409)
	publicCall(t, f, legacy, "default", "POST", path+"/operations", apiOperationInput{Action: "stop"}, 409)
	var name string
	var operations int
	if err = f.s.DB.QueryRow(context.Background(), `SELECT name,(SELECT count(*) FROM operations WHERE workload_id=$1) FROM workloads WHERE id=$1`, id).Scan(&name, &operations); err != nil || name != "Variables" || operations != 0 {
		t.Fatal("a stale authorization crossed the ownership boundary", err)
	}
}
