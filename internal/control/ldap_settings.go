package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/jackc/pgx/v5"
)

func (s *Server) ldapProviders(w http.ResponseWriter, r *http.Request) {
	tx, err := s.identityTransaction(r.Context())
	if err != nil {
		http.Error(w, "directory settings unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if r.Method == "GET" {
		rows, e := tx.Query(r.Context(), "SELECT id FROM ldap_providers ORDER BY id")
		if e != nil {
			http.Error(w, "directory settings unavailable", 503)
			return
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				break
			}
			ids = append(ids, id)
		}
		rows.Close()
		if e != nil || rows.Err() != nil {
			http.Error(w, "directory settings unavailable", 503)
			return
		}
		out := []map[string]any{}
		for _, id := range ids {
			p, e := s.loadLDAP(r.Context(), tx, id)
			if e != nil {
				http.Error(w, "directory settings unavailable", 503)
				return
			}
			mappings, e := loadDirectoryMappings(r.Context(), tx, "ldap", id)
			if e != nil {
				http.Error(w, "directory mappings unavailable", 503)
				return
			}
			configured := p.Config.Password != ""
			p.Config.Password = ""
			out = append(out, map[string]any{"id": id, "configuration": p.Config, "enabled": p.Enabled, "version": p.Version, "password_configured": configured, "mappings": mappings})
		}
		respond(w, 200, map[string]any{"providers": out, "network_policy_configured": strings.TrimSpace(os.Getenv("COVE_LDAP_ALLOWED_CIDRS")) != ""})
		return
	}
	var in ldapProviderInput
	if !decode(w, r, &in) {
		return
	}
	id := r.PathValue("provider")
	var previous ldapProvider
	if id != "" {
		previous, err = s.loadLDAP(r.Context(), tx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "directory settings unavailable", 503)
			return
		}
		if previous.Version != in.Version {
			http.Error(w, "directory settings changed; reload before saving", 409)
			return
		}
	} else {
		id = resourceID()
		var count int
		if tx.QueryRow(r.Context(), "SELECT count(*) FROM ldap_providers").Scan(&count) != nil {
			http.Error(w, "directory settings unavailable", 503)
			return
		}
		if count >= 8 {
			http.Error(w, "at most eight directory providers are allowed", 409)
			return
		}
	}
	if r.Method == "DELETE" {
		if !in.Confirm {
			http.Error(w, "confirm removal of the directory provider and its managed grants", 400)
			return
		}
		var linked bool
		if tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM ldap_identities WHERE provider_id=$1)", id).Scan(&linked) != nil {
			http.Error(w, "directory identities unavailable", 503)
			return
		}
		if linked {
			http.Error(w, "unlink directory identities before deleting; disable the provider temporarily instead", 409)
			return
		}
		_, err = syncDirectoryMemberships(r.Context(), tx, "ldap", id, "", nil, actor(r).ID)
		if err == nil {
			_, err = tx.Exec(r.Context(), "DELETE FROM identity_group_mappings WHERE source_kind='ldap' AND source_id=$1", id)
		}
		if err == nil {
			_, err = tx.Exec(r.Context(), "DELETE FROM ldap_providers WHERE id=$1", id)
		}
	} else {
		if in.Configuration.Password == "" {
			in.Configuration.Password = previous.Config.Password
		}
		if err = validateLDAP(in.Configuration); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		for i, mapping := range in.Mappings {
			dn, e := ldap.ParseDN(mapping.Group)
			if e != nil || mapping.Group == "" {
				http.Error(w, "valid group DNs required", 400)
				return
			}
			in.Mappings[i].Group = dn.String()
		}
		if err = saveDirectoryMappings(r.Context(), tx, "ldap", id, in.Mappings); err != nil {
			http.Error(w, "valid, unique directory group, team and role mappings required", 400)
			return
		}
		raw, e := json.Marshal(in.Configuration)
		if e == nil {
			raw, e = seal(s.Key, raw)
		}
		if e != nil {
			http.Error(w, "directory settings unavailable", 503)
			return
		}
		if r.Method == "POST" {
			_, err = tx.Exec(r.Context(), "INSERT INTO ldap_providers(id,configuration,enabled) VALUES($1,$2,$3)", id, raw, in.Enabled)
		} else {
			_, err = tx.Exec(r.Context(), "UPDATE ldap_providers SET configuration=$2,enabled=$3,version=version+1 WHERE id=$1", id, raw, in.Enabled)
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status) VALUES($1,$2,$3,200)", actor(r).ID, "LDAP provider "+r.Method, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save directory settings", 503)
		return
	}
	respond(w, 200, map[string]any{"id": id, "version": previous.Version + 1, "ok": true})
}

func (s *Server) ldapTest(w http.ResponseWriter, r *http.Request) {
	p, err := s.loadLDAP(r.Context(), s.DB, r.PathValue("provider"))
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "directory settings unavailable", 503)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c, err := dialLDAP(ctx, p.Config)
	if err == nil {
		defer c.Close()
		err = c.Bind(p.Config.BindDN, p.Config.Password)
	}
	if err != nil {
		http.Error(w, "directory connection or verified TLS bind failed; check the host network policy, certificate trust and saved bind credentials", 502)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

type ldapProviderInput struct {
	Configuration ldapConfiguration  `json:"configuration"`
	Enabled       bool               `json:"enabled"`
	Version       int64              `json:"version"`
	Confirm       bool               `json:"confirm"`
	Mappings      []directoryMapping `json:"mappings"`
}
