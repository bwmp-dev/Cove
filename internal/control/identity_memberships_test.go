package control

import (
	"context"
	"encoding/json"
	"testing"
)

func TestDirectoryMembershipSourcesPreserveManualAccess(t *testing.T) {
	f := newAccountFixture(t)
	f.user("manual", "manual@example.com")
	f.user("directory", "directory@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','manual','member','[{"scope":"team","scope_id":"default","role":"viewer"}]')`)
	apply := func(source string, users ...string) {
		t.Helper()
		tx, err := f.s.identityTransaction(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		claims := []directoryClaim{}
		for _, user := range users {
			claims = append(claims, directoryClaim{Group: "engineering", Team: "default", User: user, memberInput: memberInput{Role: "member", Grants: []grant{{Scope: "team", ScopeID: "default", Role: "editor"}}}})
		}
		if _, err = syncDirectoryMemberships(t.Context(), tx, "github", source, "", claims, "GitHub fixture"); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	apply("one", "manual", "directory")
	apply("two", "directory")
	apply("one")
	var grants []grant
	if err := f.s.DB.QueryRow(t.Context(), "SELECT grants FROM team_members WHERE team_id='default' AND user_id='manual'").Scan(&grants); err != nil || len(grants) != 1 || grants[0].Role != "viewer" {
		t.Fatalf("manual access was not restored: %v %v", grants, err)
	}
	var exists bool
	if err := f.s.DB.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM team_members WHERE team_id='default' AND user_id='directory')").Scan(&exists); err != nil || !exists {
		t.Fatal("another source lost its membership", err)
	}
	apply("two")
	if err := f.s.DB.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM team_members WHERE team_id='default' AND user_id='directory')").Scan(&exists); err != nil || exists {
		t.Fatal("directory-only access was retained", err)
	}
	var events int
	if err := f.s.DB.QueryRow(t.Context(), "SELECT count(*) FROM audit_events WHERE action='identity membership reconciled'").Scan(&events); err != nil || events != 4 {
		t.Fatal("membership changes were not audited atomically", events, err)
	}
}

func TestDirectoryMembershipDryRunAndLastOwner(t *testing.T) {
	f := newAccountFixture(t)
	f.user("one", "one@example.com")
	f.user("two", "two@example.com")
	claims := []directoryClaim{}
	for _, user := range []string{"one", "two"} {
		claims = append(claims, directoryClaim{Group: "owners", Team: "default", User: user, memberInput: memberInput{Role: "owner", Grants: []grant{}}})
	}
	tx, err := f.s.identityTransaction(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	changes, err := syncDirectoryMemberships(t.Context(), tx, "github", "fixture", "", claims, "GitHub fixture")
	if err != nil || len(changes) != 2 {
		t.Fatal(changes, err)
	}
	tx.Rollback(t.Context())
	var count int
	if err = f.s.DB.QueryRow(t.Context(), "SELECT count(*) FROM identity_membership_claims").Scan(&count); err != nil || count != 0 {
		t.Fatal("preview changed access", count, err)
	}
	if err = f.s.DB.QueryRow(t.Context(), "SELECT count(*) FROM audit_events WHERE action='identity membership reconciled'").Scan(&count); err != nil || count != 0 {
		t.Fatal("preview committed audit deliveries", count, err)
	}
	tx, err = f.s.identityTransaction(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = syncDirectoryMemberships(t.Context(), tx, "github", "fixture", "", claims, "GitHub fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), "DELETE FROM team_members WHERE team_id='default' AND user_id='bootstrap'"); err != nil {
		t.Fatal(err)
	}
	changes, err = syncDirectoryMemberships(t.Context(), tx, "github", "fixture", "", nil, "GitHub fixture")
	if err != nil {
		t.Fatal(err)
	}
	warnings := 0
	for _, change := range changes {
		if change.Warning != "" {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatal("last owner was not preserved", changes)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRow(t.Context(), "SELECT count(*) FROM team_members WHERE team_id='default' AND role='owner'").Scan(&count); err != nil || count != 1 {
		t.Fatal("team lost its last owner", count, err)
	}
}

func TestDirectoryMappingCustomRoleAndValidation(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("INSERT INTO team_roles(team_id,id,name,permissions) VALUES('default','secrets','Secrets',ARRAY['view','read_secrets'])")
	tx, err := f.s.identityTransaction(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	mappings := []directoryMapping{{Group: "developers", Team: "default", Role: "secrets"}}
	if err = saveDirectoryMappings(t.Context(), tx, "github", "fixture", mappings); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadDirectoryMappings(t.Context(), tx, "github", "fixture")
	if err != nil || len(loaded) != 1 || loaded[0] != mappings[0] {
		t.Fatal(loaded, err)
	}
	claims, err := directoryMappingClaims(t.Context(), tx, mappings, map[string][]string{"developers": {"bootstrap", "bootstrap"}})
	if err != nil || len(claims) != 1 {
		t.Fatal(claims, err)
	}
	raw, _ := json.Marshal(claims[0].Grants)
	if string(raw) != `[{"scope":"team","scope_id":"default","role":"secrets"}]` {
		t.Fatal("custom role lost its scope", string(raw))
	}
	for _, invalid := range [][]directoryMapping{
		{{Group: "developers", Team: "missing", Role: "viewer"}},
		{{Group: "developers", Team: "default", Role: "missing"}},
		{mappings[0], mappings[0]},
	} {
		if saveDirectoryMappings(t.Context(), tx, "github", "fixture", invalid) == nil {
			t.Fatal("invalid mapping accepted", invalid)
		}
	}
}
