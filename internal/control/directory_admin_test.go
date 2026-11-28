package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDirectoryAdminTargetsAndSourcePrivacy(t *testing.T) {
	f := newAccountFixture(t)
	admin := f.user("directory-admin", "admin@example.invalid")
	f.sql("UPDATE users SET is_admin=true WHERE id='directory-admin'")
	member := f.user("directory-member", "member@example.invalid")
	f.sql("INSERT INTO teams(id,name) VALUES('remote-team','Unjoined team')")
	f.call(member, "", "GET", "/api/settings/directory/teams", nil, 403)
	var listing struct{ Teams []map[string]string }
	if json.Unmarshal(f.call(admin, "", "GET", "/api/settings/directory/teams", nil, 200).Body.Bytes(), &listing) != nil {
		t.Fatal("invalid mapping target list")
	}
	found := false
	for _, team := range listing.Teams {
		if len(team) != 2 {
			t.Fatal("mapping target exposes extra fields")
		}
		if team["id"] == "remote-team" && team["name"] == "Unjoined team" {
			found = true
		}
	}
	if !found {
		t.Fatal("unjoined mapping team missing")
	}
	raw, _ := json.Marshal(ldapConfiguration{Name: "Engineering directory", Password: "fixture-secret-not-metadata"})
	encrypted, err := seal(f.s.Key, raw)
	if err != nil {
		t.Fatal(err)
	}
	f.sql("INSERT INTO ldap_providers(id,configuration) VALUES('directory',$1)", encrypted)
	tx, err := f.s.identityTransaction(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = syncDirectoryMemberships(t.Context(), tx, "ldap", "directory", "", []directoryClaim{{Group: "engineering", Team: "default", User: "directory-member", memberInput: memberInput{Role: "admin", Grants: []grant{}}}}, "Fixture")
	if err == nil {
		err = tx.Commit(t.Context())
	} else {
		tx.Rollback(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	metadata := f.call(admin, "default", "GET", "/api/teams/default/members", nil, 200).Body.String()
	if !strings.Contains(metadata, `"display_name":"Engineering directory"`) || strings.Contains(metadata, "fixture-secret-not-metadata") {
		t.Fatal("source metadata missing or exposes credentials")
	}
	ordinary := f.call(member, "default", "GET", "/api/teams/default/members", nil, 200).Body.String()
	if strings.Contains(ordinary, "Engineering directory") || strings.Contains(ordinary, "display_name") {
		t.Fatal("directory configuration metadata exposed outside instance administration")
	}
}

func TestSCIMActivityBoundedAndSafe(t *testing.T) {
	f := newSCIMFixture(t)
	u, _ := f.request("POST", "/scim/v2/Users", scimFixtureUser("activity@example.invalid", "activity"), 201)
	id := u["id"].(string)
	f.request("PATCH", "/scim/v2/Users/"+id, scimFixturePatch("password", "fixture-never-returned"), 400)
	var result struct {
		Activity []struct {
			ID        int64  `json:"id"`
			Operation string `json:"operation"`
			Target    string `json:"target_id"`
			Outcome   string `json:"outcome"`
			Status    int    `json:"status"`
		}
	}
	response := f.call("root", "", "GET", "/api/settings/scim/activity", nil, 200)
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Activity) != 2 {
		t.Fatal("provisioning activity missing", response.Body.String())
	}
	if result.Activity[0].Outcome != "failed" || result.Activity[0].Status != 400 || result.Activity[0].Target != id || result.Activity[1].Outcome != "succeeded" {
		t.Fatal("provisioning outcome incorrect")
	}
	if strings.Contains(response.Body.String(), f.token) || strings.Contains(response.Body.String(), "fixture-never-returned") {
		t.Fatal("provisioning activity leaked credentials")
	}
	f.sql("INSERT INTO audit_events(actor,action,subject,status) SELECT 'SCIM','PATCH SCIM users','fixture',200 FROM generate_series(1,120)")
	if json.Unmarshal(f.call("root", "", "GET", "/api/settings/scim/activity", nil, 200).Body.Bytes(), &result) != nil || len(result.Activity) != 100 {
		t.Fatal("activity is not bounded")
	}
	for i := 1; i < len(result.Activity); i++ {
		if result.Activity[i].ID >= result.Activity[i-1].ID {
			t.Fatal("activity ordering unstable")
		}
	}
	member := f.user("activity-viewer", "viewer@example.invalid")
	f.call(member, "", "GET", "/api/settings/scim/activity", nil, 403)
}
