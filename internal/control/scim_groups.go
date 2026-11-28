package control

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func saveSCIMGroup(ctx context.Context, tx pgx.Tx, id string, document scimDocument, create bool) (string, error) {
	name, external, members, err := validateSCIMGroup(document)
	if err != nil {
		return "", err
	}
	if len(members) > 0 {
		var count int
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM scim_users WHERE id=ANY($1) AND deleted_at IS NULL", members).Scan(&count); err != nil {
			return "", err
		}
		if count != len(members) {
			return "", &scimProblem{Status: 400, Type: "invalidValue", Detail: "Every group member must be an existing provisioned SCIM user."}
		}
	}
	if create {
		id = resourceID()
		_, err = tx.Exec(ctx, "INSERT INTO scim_groups(id,name,external_id) VALUES($1,$2,$3)", id, name, external)
	} else {
		_, err = tx.Exec(ctx, "UPDATE scim_groups SET name=$2,external_id=$3,version=version+1,modified_at=now() WHERE id=$1", id, name, external)
	}
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM scim_group_members WHERE group_id=$1", id); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO scim_group_members(group_id,user_id) SELECT $1,unnest($2::text[])", id, members); err != nil {
		return "", err
	}
	return id, nil
}

func deleteSCIMGroup(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, "DELETE FROM identity_group_mappings WHERE source_kind='scim' AND source_id='instance' AND group_id=$1", id); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, "DELETE FROM scim_groups WHERE id=$1", id)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return &scimProblem{Status: 404, Detail: "The SCIM group does not exist."}
	}
	return nil
}

func scimAudit(ctx context.Context, tx pgx.Tx, action, id string) error {
	if id == "" {
		return errors.New("SCIM audit subject required")
	}
	_, err := tx.Exec(ctx, "INSERT INTO audit_events(actor,action,subject,status) VALUES('SCIM',$1,$2,200)", action, id)
	return err
}
