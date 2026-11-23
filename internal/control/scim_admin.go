package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (s *Server) scimGroupMappings(w http.ResponseWriter, r *http.Request) {
	var tx pgx.Tx
	var err error
	if r.Method == "GET" {
		tx, err = s.DB.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	} else {
		tx, err = s.identityTransaction(r.Context())
	}
	if err != nil {
		http.Error(w, "provisioning groups unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	mappings, err := loadDirectoryMappings(r.Context(), tx, "scim", "instance")
	if err != nil {
		http.Error(w, "provisioning groups unavailable", 503)
		return
	}
	if r.Method == "GET" {
		rows, err := tx.Query(r.Context(), "SELECT g.id,g.name,g.external_id,g.version,(SELECT count(*) FROM scim_group_members m WHERE m.group_id=g.id) FROM scim_groups g ORDER BY lower(g.name),g.id")
		if err != nil {
			http.Error(w, "provisioning groups unavailable", 503)
			return
		}
		defer rows.Close()
		groups := []map[string]any{}
		for rows.Next() {
			var id, name, external string
			var version int64
			var count int
			if rows.Scan(&id, &name, &external, &version, &count) != nil {
				http.Error(w, "provisioning groups unavailable", 503)
				return
			}
			selected := []directoryMapping{}
			for _, mapping := range mappings {
				if mapping.Group == id {
					selected = append(selected, mapping)
				}
			}
			groups = append(groups, map[string]any{"id": id, "display_name": name, "external_id": external, "version": version, "members": count, "mappings": selected})
		}
		if rows.Err() != nil {
			http.Error(w, "provisioning groups unavailable", 503)
			return
		}
		respond(w, 200, map[string]any{"groups": groups})
		return
	}
	var in scimMappingsInput
	if !decode(w, r, &in) {
		return
	}
	id := r.PathValue("group")
	var current int64
	if err = tx.QueryRow(r.Context(), "SELECT version FROM scim_groups WHERE id=$1 FOR UPDATE", id).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "provisioning groups unavailable", 503)
		}
		return
	}
	if !in.Confirm || current != in.Version {
		http.Error(w, "review the current group and confirm its Cove role mappings", 409)
		return
	}
	wanted := []directoryMapping{}
	for _, mapping := range mappings {
		if mapping.Group != id {
			wanted = append(wanted, mapping)
		}
	}
	for _, mapping := range in.Mappings {
		mapping.Group = id
		wanted = append(wanted, mapping)
	}
	if err = saveDirectoryMappings(r.Context(), tx, "scim", "instance", wanted); err != nil {
		http.Error(w, "valid, unique group, team and role mappings required", 400)
		return
	}
	_, err = tx.Exec(r.Context(), "UPDATE scim_groups SET version=version+1,modified_at=now() WHERE id=$1", id)
	if err == nil {
		err = syncSCIMMemberships(r.Context(), tx)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status) VALUES($1,'SCIM group mappings updated',$2,200)", actor(r).ID, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save provisioning group mappings", 503)
		return
	}
	respond(w, 200, map[string]any{"ok": true, "group_version": current + 1})
}

func (s *Server) scimAccountLink(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), "SELECT s.id,s.user_name,s.external_id,COALESCE(s.oidc_provider_id,''),NOT u.disabled,s.deleted_at,s.version FROM scim_users s JOIN users u ON u.id=s.id ORDER BY s.user_name,s.id")
		if err != nil {
			http.Error(w, "provisioning links unavailable", 503)
			return
		}
		defer rows.Close()
		links := []map[string]any{}
		for rows.Next() {
			var user, username, external, provider string
			var active bool
			var deleted *time.Time
			var version int64
			if rows.Scan(&user, &username, &external, &provider, &active, &deleted, &version) != nil {
				http.Error(w, "provisioning links unavailable", 503)
				return
			}
			links = append(links, map[string]any{"user_id": user, "user_name": username, "external_id": external, "oidc_provider_id": provider, "active": active, "deleted_at": deleted, "version": version})
		}
		if rows.Err() != nil {
			http.Error(w, "provisioning links unavailable", 503)
			return
		}
		respond(w, 200, map[string]any{"links": links})
		return
	}
	var in scimLinkInput
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		http.Error(w, "confirm that provisioning may disable this account and revoke all of its sessions and tokens", 400)
		return
	}
	tx, err := s.identityTransaction(r.Context())
	if err != nil {
		http.Error(w, "provisioning links unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	id := in.User
	if r.Method == "DELETE" {
		id = r.PathValue("user")
		var found string
		if err = tx.QueryRow(r.Context(), "SELECT id FROM scim_users WHERE id=$1 FOR UPDATE", id).Scan(&found); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "provisioned account not found", 404)
			} else {
				http.Error(w, "provisioning links unavailable", 503)
			}
			return
		}
		if _, err = tx.Exec(r.Context(), `WITH removed AS (DELETE FROM scim_group_members WHERE user_id=$1 RETURNING group_id)
 UPDATE scim_groups SET version=version+1,modified_at=now() WHERE id IN (SELECT group_id FROM removed)`, id); err == nil {
			_, err = tx.Exec(r.Context(), "DELETE FROM scim_users WHERE id=$1", id)
		}
		if err == nil {
			err = syncSCIMMemberships(r.Context(), tx)
		}
	} else {
		var email, name string
		var admin, disabled, verified bool
		err = tx.QueryRow(r.Context(), "SELECT COALESCE(email,''),name,is_admin,disabled,email_verified FROM users WHERE id=$1 FOR UPDATE", id).Scan(&email, &name, &admin, &disabled, &verified)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "provisioning links unavailable", 503)
			return
		}
		if err != nil || admin || !verified || email == "" {
			http.Error(w, "select a verified, non-administrator Cove account", 409)
			return
		}
		state, e := loadSCIMState(r.Context(), tx, s.Key)
		if e != nil {
			http.Error(w, "provisioning settings unavailable", 503)
			return
		}
		if in.Username == "" {
			in.Username = email
		}
		profile, e := validateSCIMUser(scimDocument{"schemas": []any{scimUserSchema}, "userName": in.Username, "externalId": in.External, "displayName": name, "active": !disabled, "emails": []any{map[string]any{"value": email, "primary": true}}})
		if e != nil || state.Config.OIDCProvider != "" && profile.External == "" {
			http.Error(w, "valid user name and the configured OIDC subject external ID required", 400)
			return
		}
		raw, e := json.Marshal(profile.Document)
		if e != nil {
			http.Error(w, "provisioning links unavailable", 503)
			return
		}
		_, err = tx.Exec(r.Context(), "INSERT INTO scim_users(id,external_id,user_name,document,oidc_provider_id) VALUES($1,$2,$3,$4,NULLIF($5,''))", id, profile.External, profile.Username, raw, state.Config.OIDCProvider)
		if err == nil && state.Config.OIDCProvider != "" {
			var linked bool
			err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM oidc_identities WHERE provider_id=$1 AND subject=$2 AND user_id=$3)", state.Config.OIDCProvider, profile.External, id).Scan(&linked)
			if err == nil && !linked {
				_, err = tx.Exec(r.Context(), "INSERT INTO oidc_identities(provider_id,subject,user_id) VALUES($1,$2,$3)", state.Config.OIDCProvider, profile.External, id)
			}
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status) VALUES($1,$2,$3,200)", actor(r).ID, "SCIM account link "+r.Method, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		var postgres *pgconn.PgError
		if errors.As(err, &postgres) && postgres.Code == "23505" {
			http.Error(w, "this account or identity is already assigned", 409)
		} else {
			http.Error(w, "could not link or detach the account", 503)
		}
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

type scimMappingsInput struct {
	Version  int64              `json:"group_version"`
	Confirm  bool               `json:"confirm"`
	Mappings []directoryMapping `json:"mappings"`
}

type scimLinkInput struct {
	User     string `json:"user_id"`
	Username string `json:"user_name"`
	External string `json:"external_id"`
	Confirm  bool   `json:"confirm"`
}
