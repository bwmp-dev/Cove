package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestGithubConnectionSharing(t *testing.T) {
	f := newAccountFixture(t)
	manager := f.user("manager", "manager@example.test")
	recipient := f.user("recipient", "recipient@example.test")
	personal := f.user("personal", "personal@example.test")
	f.sql(`INSERT INTO teams(id,name) VALUES('second','Second'),('hidden','Private team')`)
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','manager','owner','[]'),('second','manager','member','[{"scope":"team","role":"editor"}]'),('default','recipient','owner','[]')`)
	credentials, err := seal(f.s.Key, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO github_app(id,app_id,slug,credentials) VALUES(1,12,'fixture',$1)`, credentials)
	f.sql(`INSERT INTO github_installations(id,account) VALUES(41,'shared-account'),(42,'personal-account')`)
	f.sql(`INSERT INTO github_installation_managers VALUES(41,'manager'),(42,'personal')`)
	f.sql(`INSERT INTO github_installation_teams VALUES(41,'hidden')`)
	share := func(user string, teams []string, status int) {
		f.call(user, "", "PUT", "/api/github/app/installations/41", map[string]any{"team_ids": teams}, status)
	}
	share(recipient, []string{"default"}, 403)
	share(manager, []string{"hidden"}, 403)
	share(manager, []string{"default", "default"}, 400)
	share(manager, []string{"default", "second"}, 200)
	var count int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM github_installation_teams WHERE installation_id=41`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("existing hidden share lost: %d %v", count, err)
	}
	info := f.call(manager, "default", "GET", "/api/github/app", nil, 200).Body.String()
	if strings.Contains(info, "hidden") || strings.Contains(info, "personal-account") || !strings.Contains(info, `"can_manage":true`) {
		t.Fatal("incorrect manager visibility", info)
	}
	info = f.call(recipient, "default", "GET", "/api/github/app", nil, 200).Body.String()
	if strings.Contains(info, "second") || strings.Contains(info, "hidden") || !strings.Contains(info, `"can_manage":false`) {
		t.Fatal("incorrect recipient visibility", info)
	}
	share(recipient, []string{"default"}, 403)
	info = f.call(personal, "", "GET", "/api/github/app", nil, 200).Body.String()
	if !strings.Contains(info, "personal-account") || strings.Contains(info, "shared-account") {
		t.Fatal("personal connection requires team", info)
	}
	f.call(personal, "", "PUT", "/api/github/app/installations/42", map[string]any{"team_ids": []string{"default"}}, 403)
	share(manager, []string{}, 200)
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM github_installation_teams WHERE installation_id=41`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("hidden share changed: %d %v", count, err)
	}
	f.call(recipient, "default", "DELETE", "/api/github/app/installations/41", nil, 404)
	// Personal management outlives team membership, without retaining sharing rights.
	f.sql(`DELETE FROM team_members WHERE user_id='manager'`)
	info = f.call(manager, "", "GET", "/api/github/app", nil, 200).Body.String()
	var result struct {
		Installations []struct {
			Teams []string `json:"team_ids"`
		} `json:"installations"`
	}
	if err := json.Unmarshal([]byte(info), &result); err != nil || len(result.Installations) != 1 || len(result.Installations[0].Teams) != 0 {
		t.Fatal("personal ownership or team privacy lost", info)
	}
	share(manager, []string{"default"}, 403)
}

func TestGithubSharingMigrationPreservesExistingAccessOnce(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO github_installations(id,account,team_id) VALUES(41,'legacy','default')`)
	f.sql(`DELETE FROM schema_migrations WHERE id='github-team-sharing-v1'`)
	if err := f.s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM github_installation_teams WHERE installation_id=41 AND team_id='default'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("legacy share not migrated: %d %v", count, err)
	}
	f.sql(`DELETE FROM github_installation_teams WHERE installation_id=41`)
	if err := f.s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM github_installation_teams WHERE installation_id=41`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("revoked share resurrected: %d %v", count, err)
	}
}
