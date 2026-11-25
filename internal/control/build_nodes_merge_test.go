package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildNodesMergeScopesAndCrossTeamBinding(t *testing.T) {
	f, id, owner := publicFixture(t)
	f.sql(`UPDATE nodes SET state='ready',capabilities='{"build_publish":true,"remote_builds":true,"os":"linux","arch":"amd64"}' WHERE id='variable-node'`)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('remote-builder','Builder','localhost',22,'',$1,'ready','{"build_publish":true,"remote_builds":true,"os":"linux","arch":"amd64"}')`, []byte{})
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('default','remote-builder')`)
	_, read := mintPublic(t, f, owner, "user", "workloads:read")
	_, configure := mintPublic(t, f, owner, "user", "configuration:write")
	path := "/api/v1/workloads/" + id + "/build-node"
	publicCall(t, f, read, "default", "GET", path, nil, 200)
	publicCall(t, f, read, "default", "PUT", path, buildNodeInput{Node: "remote-builder"}, 403)
	publicCall(t, f, configure, "default", "PUT", path, buildNodeInput{Node: "remote-builder"}, 200)
	f.sql(`INSERT INTO teams(id,name) VALUES('builder-destination','Destination')`)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('builder-destination-project','Destination','builder-destination')`)
	input := resourceMoveInput{TeamID: "builder-destination", ProjectID: "builder-destination-project"}
	response := f.call("root", "default", "POST", "/api/workloads/"+id+"/move/preview", input, 200)
	var review resourceMoveReview
	if err := json.Unmarshal(response.Body.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(review.Blockers, " "), "Clear the designated build node") {
		t.Fatal("team-owned builder assignment would transfer silently", review)
	}
	publicCall(t, f, configure, "default", "PUT", path, buildNodeInput{}, 200)
	response = f.call("root", "default", "POST", "/api/workloads/"+id+"/move/preview", input, 200)
	if strings.Contains(response.Body.String(), "Clear the designated build node") {
		t.Fatal("cleared binding still blocks transfer", response.Body.String())
	}
}
