package control

import (
	"strings"
	"testing"
)

func TestWorkspaceSearchFiltersCurrentGrantsAndTeams(t *testing.T) {
	f, id, owner := publicFixture(t)
	f.sql(`UPDATE workloads SET name='Search visible' WHERE id=$1`, id)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('hidden-search','Search hidden','default')`)
	f.sql(`INSERT INTO teams(id,name) VALUES('search-other','Other')`)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('other-search','Search other','search-other')`)
	viewer := f.user("search-viewer", "search@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','search-viewer','member','[{"scope":"environment","scope_id":"variable-env","role":"viewer"}]')`)
	result := f.call(viewer, "default", "GET", "/api/search?q=Search", nil, 200).Body.String()
	if !strings.Contains(result, "Search visible") || strings.Contains(result, "Search hidden") || strings.Contains(result, "Search other") {
		t.Fatal(result)
	}
	operation := jsonString(t, f.call(owner, "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "stop"}, 202), "id")
	result = f.call(viewer, "default", "GET", "/api/search?q="+operation, nil, 200).Body.String()
	if !strings.Contains(result, "/operations/"+operation) {
		t.Fatal(result)
	}
	f.sql(`UPDATE team_members SET grants='[]' WHERE user_id='search-viewer'`)
	result = f.call(viewer, "default", "GET", "/api/search?q=Search", nil, 200).Body.String()
	if strings.Contains(result, "Search visible") {
		t.Fatal(result)
	}
	result = f.call(viewer, "default", "GET", "/api/search?q="+operation, nil, 200).Body.String()
	if strings.Contains(result, operation) {
		t.Fatal("operation exposed after grant revocation", result)
	}
	result = f.call(owner, "default", "GET", "/api/search?q=Search", nil, 200).Body.String()
	if !strings.Contains(result, "Search hidden") || strings.Contains(result, "Search other") {
		t.Fatal(result)
	}
	f.call("", "default", "GET", "/api/search?q=Search", nil, 401)
	f.call(owner, "default", "GET", "/api/search?q="+strings.Repeat("x", 201), nil, 400)
	result = f.call(owner, "default", "GET", "/api/search?q=%25", nil, 200).Body.String()
	if strings.Contains(result, "Search visible") {
		t.Fatal("wildcard expanded", result)
	}
	result = f.call(viewer, "default", "GET", "/api/search?q=Settings", nil, 200).Body.String()
	if strings.Contains(result, "/admin") {
		t.Fatal(result)
	}
}
