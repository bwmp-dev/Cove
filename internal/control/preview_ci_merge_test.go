package control

import (
	"strings"
	"testing"
)

func TestPreviewCIMergeCurrentOwnerPolicy(t *testing.T) {
	f, base, _, event := newCIPreviewFixture(t)
	owner := f.user("ci-policy-owner", "ci-policy-owner@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','ci-policy-owner','member','[{"scope":"project","scope_id":"p","role":"editor"}]')`)
	token := jsonString(t, f.call(owner, "default", "PUT", "/api/projects/p/preview-images", map[string]any{"resource_ids": []string{base}, "days": 7}, 201), "token")
	refused := func() {
		t.Helper()
		ciSubmit(t, f, token, event, 401)
		status := f.call("root", "default", "GET", "/api/projects/p/preview-images", nil, 200)
		if !strings.Contains(status.Body.String(), `"matches_scope":false`) {
			t.Fatal("inactive credential presented as usable", status.Body.String())
		}
	}
	f.sql(`INSERT INTO authentication_failures(key,failures,locked_until) VALUES('account:ci-policy-owner',5,now()+interval '1 hour')`)
	refused()
	f.sql(`DELETE FROM authentication_failures WHERE key='account:ci-policy-owner'`)
	f.sql(`UPDATE teams SET require_second_factor=true WHERE id='default'`)
	refused()
	f.sql(`UPDATE teams SET require_second_factor=false WHERE id='default'`)
	ciSubmit(t, f, token, event, 202)
	tx, err := f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = revokeSCIMAccess(t.Context(), tx, "ci-policy-owner"); err == nil {
		err = tx.Commit(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	ciSubmit(t, f, token, event, 401)
	var count int
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM preview_ci_credentials WHERE user_id='ci-policy-owner'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("deprovisioned credential survived", count, err)
	}
}

func TestPreviewCIMergeManagedDatabaseKeepsTypedCopy(t *testing.T) {
	f, base, token, event := newCIPreviewFixture(t)
	var encrypted []byte
	if err := f.s.DB.QueryRow(t.Context(), `SELECT configuration FROM workloads WHERE id=$1`, base).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	var config workloadInput
	if err := f.s.unsealJSON(encrypted, &config); err != nil {
		t.Fatal(err)
	}
	config.Database = &databaseResource{Engine: "postgres", Version: "17"}
	encrypted, err := f.s.sealJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE workloads SET configuration=$2 WHERE id=$1`, base, encrypted)
	response := f.call("root", "default", "PUT", "/api/projects/p/preview-images", map[string]any{"resource_ids": []string{base}, "days": 7}, 409)
	if !strings.Contains(response.Body.String(), "typed preview copy") {
		t.Fatal("managed database CI image replacement was not explained", response.Body.String())
	}
	ciSubmit(t, f, token, event, 409)
}
