package control

import (
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"slices"
	"strings"
)

var permissionCatalogue = []string{"view", "read_secrets", "operate", "edit_configuration", "manage_backups", "manage_members"}

func (s *Server) permission(r *http.Request, kind, id, permission string) (bool, error) {
	p := actor(r)
	var allowed bool
	var err error
	switch kind {
	case "team":
		err = s.DB.QueryRow(r.Context(), "SELECT $3 OR cove_permission($1,$2,'','',$4)", p.ID, p.Team, p.Admin, permission).Scan(&allowed)
	case "project":
		err = s.DB.QueryRow(r.Context(), "SELECT $3 OR cove_permission($1,p.team_id,p.id,'',$5) FROM projects p WHERE p.id=$4 AND p.team_id=$2", p.ID, p.Team, p.Admin, id, permission).Scan(&allowed)
	case "environment":
		err = s.DB.QueryRow(r.Context(), "SELECT $3 OR cove_permission($1,e.team_id,e.project_id,e.id,$5) FROM environments e WHERE e.id=$4 AND e.team_id=$2", p.ID, p.Team, p.Admin, id, permission).Scan(&allowed)
	case "workload":
		err = s.DB.QueryRow(r.Context(), "SELECT $3 OR cove_permission($1,w.team_id,w.project_id,COALESCE(w.environment_id,''),$5) FROM workloads w WHERE w.id=$4 AND w.team_id=$2", p.ID, p.Team, p.Admin, id, permission).Scan(&allowed)
	}
	return allowed, err
}
func (s *Server) customRoles(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("id")
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), "SELECT id,name,permissions FROM team_roles WHERE team_id=$1 ORDER BY name,id", team)
		if err != nil {
			http.Error(w, "roles unavailable", 503)
			return
		}
		defer rows.Close()
		type role struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			Permissions []string `json:"permissions"`
		}
		out := []role{}
		for rows.Next() {
			var v role
			if rows.Scan(&v.ID, &v.Name, &v.Permissions) != nil {
				http.Error(w, "roles unavailable", 503)
				return
			}
			out = append(out, v)
		}
		if rows.Err() != nil {
			http.Error(w, "roles unavailable", 503)
			return
		}
		respond(w, 200, map[string]any{"roles": out, "permission_catalogue": permissionCatalogue, "built_in": map[string][]string{"viewer": {"view"}, "operator": {"view", "operate", "manage_backups"}, "editor": {"view", "operate", "read_secrets", "edit_configuration", "manage_backups"}}})
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "roles unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var locked string
	if err = tx.QueryRow(r.Context(), "SELECT id FROM teams WHERE id=$1 FOR UPDATE", team).Scan(&locked); err != nil {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("role")
	if r.Method == "DELETE" {
		var used bool
		err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM team_members m CROSS JOIN LATERAL jsonb_array_elements(m.grants) g WHERE m.team_id=$1 AND g->>'role'=$2) OR EXISTS(SELECT 1 FROM invitations i CROSS JOIN LATERAL jsonb_array_elements(i.grants) g WHERE i.team_id=$1 AND g->>'role'=$2 AND i.consumed_at IS NULL AND i.expires_at>now()) OR EXISTS(SELECT 1 FROM identity_group_mappings WHERE team_id=$1 AND role=$2)`, team, id).Scan(&used)
		if err != nil {
			http.Error(w, "roles unavailable", 503)
			return
		}
		if used {
			http.Error(w, "role is in use", 409)
			return
		}
		result, e := tx.Exec(r.Context(), "DELETE FROM team_roles WHERE team_id=$1 AND id=$2", team, id)
		err = e
		if err == nil && result.RowsAffected() == 0 {
			http.NotFound(w, r)
			return
		}
	} else {
		var in struct {
			Name        string   `json:"name"`
			Permissions []string `json:"permissions"`
		}
		if !decode(w, r, &in) {
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" || len(in.Name) > 120 || len(in.Permissions) > len(permissionCatalogue) || !slices.Contains(in.Permissions, "view") {
			http.Error(w, "name and permissions including view required", 400)
			return
		}
		for _, p := range in.Permissions {
			if !slices.Contains(permissionCatalogue, p) {
				http.Error(w, "unknown permission", 400)
				return
			}
		}
		if id == "" {
			id = resourceID()
			_, err = tx.Exec(r.Context(), "INSERT INTO team_roles(team_id,id,name,permissions) VALUES($1,$2,$3,$4)", team, id, in.Name, in.Permissions)
		} else {
			result, e := tx.Exec(r.Context(), "UPDATE team_roles SET name=$3,permissions=$4 WHERE team_id=$1 AND id=$2", team, id, in.Name, in.Permissions)
			err = e
			if err == nil && result.RowsAffected() == 0 {
				http.NotFound(w, r)
				return
			}
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "roles unavailable", 503)
		return
	}
	respond(w, 200, map[string]string{"id": id})
}
func (s *Server) requirePermission(w http.ResponseWriter, r *http.Request, kind, id, permission string) bool {
	allowed, err := s.permission(r, kind, id, permission)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return false
	}
	if err != nil {
		http.Error(w, "authorization unavailable", 503)
		return false
	}
	if !allowed {
		http.Error(w, "permission denied", 403)
		return false
	}
	return true
}
