package control

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

func scimUniqueness() error {
	return &scimProblem{Status: 409, Type: "uniqueness", Detail: "This identity already exists. An instance administrator must explicitly link an existing Cove account."}
}

func checkSCIMOwnerDisable(ctx context.Context, tx pgx.Tx, user string) error {
	rows, err := tx.Query(ctx, "SELECT t.id FROM teams t JOIN team_members m ON m.team_id=t.id WHERE m.user_id=$1 AND m.role='owner' ORDER BY t.id FOR UPDATE OF t", user)
	if err != nil {
		return err
	}
	teams := []string{}
	for rows.Next() {
		var team string
		if err = rows.Scan(&team); err != nil {
			break
		}
		teams = append(teams, team)
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		return errors.New("team owner policy unavailable")
	}
	for _, team := range teams {
		var another bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM team_members m JOIN users u ON u.id=m.user_id WHERE m.team_id=$1 AND m.user_id<>$2 AND m.role='owner' AND NOT u.disabled)", team, user).Scan(&another); err != nil {
			return err
		}
		if !another {
			return &scimProblem{Status: 409, Type: "mutability", Detail: "The last active team owner cannot be deprovisioned. Assign another active owner first."}
		}
	}
	return nil
}

func revokeSCIMAccess(ctx context.Context, tx pgx.Tx, user string) error {
	for _, query := range []string{
		"DELETE FROM sessions WHERE user_id=$1",
		"DELETE FROM api_tokens WHERE user_id=$1",
		"DELETE FROM account_actions WHERE user_id=$1",
		"DELETE FROM webauthn_challenges WHERE user_id=$1",
		"DELETE FROM oidc_states WHERE link_user_id=$1",
		"DELETE FROM oauth_states WHERE link_user_id=$1",
	} {
		if _, err := tx.Exec(ctx, query, user); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) createSCIMUser(ctx context.Context, tx pgx.Tx, profile scimUserProfile, provider string) (string, error) {
	rows, err := tx.Query(ctx, "SELECT id FROM scim_users WHERE user_name=$1 OR ($2<>'' AND external_id=$2) ORDER BY id", profile.Username, profile.External)
	if err != nil {
		return "", err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		return "", errors.New("provisioned identities unavailable")
	}
	if len(ids) > 1 {
		return "", scimUniqueness()
	}
	if len(ids) == 1 {
		previous, err := s.loadSCIMUser(ctx, tx, ids[0], true, true)
		if err != nil {
			return "", err
		}
		matching := profile.External != "" && profile.External == previous.External || profile.External == "" && previous.External == "" && profile.Username == previous.Username && profile.Email == previous.Email
		if previous.DeletedAt == nil || !matching {
			return "", scimUniqueness()
		}
		if err = updateSCIMUser(ctx, tx, previous, profile, true); err != nil {
			return "", err
		}
		return previous.ID, nil
	}
	if provider != "" && profile.External == "" {
		return "", &scimProblem{Status: 400, Type: "invalidValue", Detail: "The configured OIDC binding requires externalId to equal the OIDC subject."}
	}
	id := resourceID()
	raw, err := json.Marshal(profile.Document)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO users(id,email,name,email_verified,disabled) VALUES($1,$2,$3,true,$4)", id, profile.Email, profile.Name, !profile.Active); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO scim_users(id,external_id,user_name,document,oidc_provider_id,disabled_by_scim) VALUES($1,$2,$3,$4,NULLIF($5,''),$6)", id, profile.External, profile.Username, raw, provider, !profile.Active); err != nil {
		return "", err
	}
	if provider != "" {
		if _, err = tx.Exec(ctx, "INSERT INTO oidc_identities(provider_id,subject,user_id) VALUES($1,$2,$3)", provider, profile.External, id); err != nil {
			return "", err
		}
	}
	return id, nil
}

func updateSCIMUser(ctx context.Context, tx pgx.Tx, previous scimUserRecord, profile scimUserProfile, restoring bool) error {
	if previous.Administrator {
		return &scimProblem{Status: 409, Type: "mutability", Detail: "SCIM cannot modify instance administrators."}
	}
	if previous.OIDCProvider != nil && profile.External != previous.External {
		return &scimProblem{Status: 400, Type: "mutability", Detail: "externalId is bound to an OIDC subject. An administrator must explicitly detach and relink this identity."}
	}
	managed := previous.ManagedDisable
	if profile.Active {
		if previous.Disabled && !managed {
			return &scimProblem{Status: 409, Type: "mutability", Detail: "The account was disabled locally. An instance administrator must re-enable it."}
		}
		managed = false
	} else if !previous.Disabled {
		if err := checkSCIMOwnerDisable(ctx, tx, previous.ID); err != nil {
			return err
		}
		managed = true
	}
	disabled := !profile.Active
	revoke := previous.Email != profile.Email || previous.Disabled != disabled || disabled || restoring
	versionIncrement := 0
	if revoke {
		versionIncrement = 1
	}
	if _, err := tx.Exec(ctx, "UPDATE users SET email=$2,name=$3,disabled=$4,auth_version=auth_version+$5 WHERE id=$1", previous.ID, profile.Email, profile.Name, disabled, versionIncrement); err != nil {
		return err
	}
	raw, err := json.Marshal(profile.Document)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE scim_users SET external_id=$2,user_name=$3,document=$4,disabled_by_scim=$5,deleted_at=CASE WHEN $6 THEN NULL ELSE deleted_at END,version=version+1,modified_at=now() WHERE id=$1", previous.ID, profile.External, profile.Username, raw, managed, restoring); err != nil {
		return err
	}
	if revoke {
		return revokeSCIMAccess(ctx, tx, previous.ID)
	}
	return nil
}

func deleteSCIMUser(ctx context.Context, tx pgx.Tx, user scimUserRecord) error {
	if user.Administrator {
		return &scimProblem{Status: 409, Type: "mutability", Detail: "SCIM cannot deprovision instance administrators."}
	}
	if !user.Disabled {
		if err := checkSCIMOwnerDisable(ctx, tx, user.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, "UPDATE users SET disabled=true,auth_version=auth_version+1 WHERE id=$1", user.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE scim_users SET deleted_at=now(),disabled_by_scim=$2,version=version+1,modified_at=now() WHERE id=$1", user.ID, user.ManagedDisable || !user.Disabled); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `WITH removed AS (DELETE FROM scim_group_members WHERE user_id=$1 RETURNING group_id)
 UPDATE scim_groups SET version=version+1,modified_at=now() WHERE id IN (SELECT group_id FROM removed)`, user.ID); err != nil {
		return err
	}
	return revokeSCIMAccess(ctx, tx, user.ID)
}

func syncSCIMMemberships(ctx context.Context, tx pgx.Tx) error {
	mappings, err := loadDirectoryMappings(ctx, tx, "scim", "instance")
	if err != nil {
		return err
	}
	members := map[string][]string{}
	rows, err := tx.Query(ctx, "SELECT m.group_id,m.user_id FROM scim_group_members m JOIN scim_users s ON s.id=m.user_id JOIN users u ON u.id=s.id WHERE s.deleted_at IS NULL AND NOT u.disabled ORDER BY m.group_id,m.user_id")
	if err != nil {
		return err
	}
	for rows.Next() {
		var group, user string
		if err = rows.Scan(&group, &user); err != nil {
			break
		}
		members[group] = append(members[group], user)
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		return errors.New("provisioned group membership unavailable")
	}
	claims, err := directoryMappingClaims(ctx, tx, mappings, members)
	if err != nil {
		return err
	}
	_, err = syncDirectoryMemberships(ctx, tx, "scim", "instance", "", claims, "SCIM")
	return err
}
