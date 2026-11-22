package control

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
)

func (s *Server) assignGithubInstallation(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("installation"), 10, 64)
	var in struct {
		Teams []string `json:"team_ids"`
	}
	if err != nil || id <= 0 {
		http.Error(w, "invalid installation", 400)
		return
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Teams == nil || len(in.Teams) > 100 {
		http.Error(w, "team_ids must be an array of at most 100 teams", 400)
		return
	}
	wanted := map[string]bool{}
	for _, team := range in.Teams {
		if team == "" || wanted[team] {
			http.Error(w, "team_ids must contain unique team IDs", 400)
			return
		}
		wanted[team] = true
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var allowed bool
	err = tx.QueryRow(r.Context(), "SELECT $2 OR EXISTS(SELECT 1 FROM github_installation_managers WHERE installation_id=$1 AND user_id=$3) FROM github_installations WHERE id=$1 FOR UPDATE", id, actor(r).Admin, actor(r).ID).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !allowed {
		http.Error(w, "connection management access required", 403)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	rows, err := tx.Query(r.Context(), "SELECT id FROM teams WHERE $1 OR cove_access($2,id,'','')>=3", actor(r).Admin, actor(r).ID)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	editable := map[string]bool{}
	for rows.Next() {
		var team string
		if err = rows.Scan(&team); err != nil {
			break
		}
		editable[team] = true
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	for team := range wanted {
		if !editable[team] {
			http.Error(w, "team-wide editing access required for each share", 403)
			return
		}
	}
	// Lock shares before checking their use, serializing with resource connections.
	rows, err = tx.Query(r.Context(), "SELECT team_id FROM github_installation_teams WHERE installation_id=$1 FOR UPDATE", id)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	var removed []string
	for rows.Next() {
		var team string
		if err = rows.Scan(&team); err != nil {
			break
		}
		if editable[team] && !wanted[team] {
			removed = append(removed, team)
		}
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	for _, team := range removed {
		var used bool
		err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM github_sources g JOIN workloads w ON w.id=g.workload_id WHERE g.installation_id=$1 AND w.team_id=$2)", id, team).Scan(&used)
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		if used {
			http.Error(w, "disconnect resources in the team before removing its access", 409)
			return
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM github_installation_teams WHERE installation_id=$1 AND team_id=$2", id, team); err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
	}
	for team := range wanted {
		if _, err = tx.Exec(r.Context(), "INSERT INTO github_installation_teams(installation_id,team_id) VALUES($1,$2) ON CONFLICT DO NOTHING", id, team); err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save team access", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
