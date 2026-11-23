package control

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type environment struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	PreviewID   string   `json:"preview_id,omitempty"`
	Permissions []string `json:"permissions"`
}
type project struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Environments []environment `json:"environments"`
	Permissions  []string      `json:"permissions"`
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), "SELECT p.id,p.name,COALESCE(e.id,''),COALESCE(e.name,''),COALESCE(pv.id,''),cove_permission_names($1,p.team_id,p.id,'',$3,$4),cove_permission_names($1,p.team_id,p.id,COALESCE(e.id,''),$3,$4) FROM projects p LEFT JOIN environments e ON e.project_id=p.id LEFT JOIN environment_previews pv ON pv.environment_id=e.id WHERE p.team_id=$2 AND ($3 OR cove_access($1,p.team_id,p.id,COALESCE(e.id,''))>0) ORDER BY p.name,e.name", actor(r).ID, actor(r).Team, actor(r).Admin, permissionCatalogue)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	items := []project{}
	for rows.Next() {
		var id, name string
		var env environment
		var pl, el []string
		if err = rows.Scan(&id, &name, &env.ID, &env.Name, &env.PreviewID, &pl, &el); err != nil {
			http.Error(w, "could not read projects", 500)
			return
		}
		if len(items) == 0 || items[len(items)-1].ID != id {
			names := permissionMetadata(r, pl)
			items = append(items, project{ID: id, Name: name, Environments: []environment{}, Permissions: names})
		}
		if env.ID != "" {
			env.Permissions = permissionMetadata(r, el)
			items[len(items)-1].Environments = append(items[len(items)-1].Environments, env)
		}
	}
	if rows.Err() != nil {
		http.Error(w, "could not read projects", 500)
		return
	}
	respond(w, 200, items)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request)     { s.createGroup(w, r, false) }
func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) { s.createGroup(w, r, true) }
func (s *Server) createGroup(w http.ResponseWriter, r *http.Request, isEnvironment bool) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 120 {
		http.Error(w, "name must contain 1-120 characters", 400)
		return
	}
	id := resourceID()
	var err error
	if isEnvironment {
		_, err = s.DB.Exec(r.Context(), "INSERT INTO environments(id,project_id,name,team_id) VALUES($1,$2,$3,$4)", id, r.PathValue("id"), in.Name, actor(r).Team)
	} else {
		_, err = s.DB.Exec(r.Context(), "INSERT INTO projects(id,name,team_id) VALUES($1,$2,$3)", id, in.Name, actor(r).Team)
	}
	if err != nil {
		groupError(w, err)
		return
	}
	respond(w, 201, map[string]string{"id": id})
}

func groupError(w http.ResponseWriter, err error) {
	var constraint *pgconn.PgError
	if errors.As(err, &constraint) && (constraint.Code == "23505" || constraint.Code == "23503") {
		http.Error(w, "name already exists, group is in use, or referenced group no longer exists", 409)
		return
	}
	http.Error(w, "could not update group", 500)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	result, err := s.DB.Exec(r.Context(), "DELETE FROM projects WHERE id=$1", r.PathValue("id"))
	if err != nil {
		groupError(w, err)
		return
	}
	if result.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
	result, err := s.DB.Exec(r.Context(), "DELETE FROM environments WHERE id=$1", r.PathValue("id"))
	if err != nil {
		groupError(w, err)
		return
	}
	if result.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) assignEnvironment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Project     string `json:"project_id"`
		Environment string `json:"environment_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if _, ok := s.resourceProject(w, r, in.Project, in.Environment); !ok {
		return
	}
	http.Error(w, "review and confirm placement changes through the resource move endpoint", 409)
}

func (s *Server) resourceProject(w http.ResponseWriter, r *http.Request, project, environment string) (string, bool) {
	if environment != "" {
		var parent string
		if err := s.DB.QueryRow(r.Context(), "SELECT project_id FROM environments WHERE id=$1 AND team_id=$2", environment, actor(r).Team).Scan(&parent); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.NotFound(w, r)
			} else {
				http.Error(w, "database unavailable", 503)
			}
			return "", false
		}
		if project != "" && project != parent {
			http.Error(w, "environment must belong to the selected project", 400)
			return "", false
		}
		return parent, true
	}
	if project == "" {
		http.Error(w, "a project is required", 400)
		return "", false
	}
	var exists bool
	if err := s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1 AND team_id=$2)", project, actor(r).Team).Scan(&exists); err != nil {
		http.Error(w, "database unavailable", 503)
		return "", false
	}
	if !exists {
		http.NotFound(w, r)
		return "", false
	}
	return project, true
}
