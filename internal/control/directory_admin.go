package control

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type directorySource struct {
	Kind string `json:"source_kind"`
	ID   string `json:"source_id"`
	Name string `json:"display_name,omitempty"`
}

func (s *Server) directoryTeams(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), "SELECT id,name FROM teams ORDER BY lower(name),id")
	if err != nil {
		http.Error(w, "mapping teams unavailable", 503)
		return
	}
	defer rows.Close()
	teams := []map[string]string{}
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) != nil {
			http.Error(w, "mapping teams unavailable", 503)
			return
		}
		teams = append(teams, map[string]string{"id": id, "name": name})
	}
	if rows.Err() != nil {
		http.Error(w, "mapping teams unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"teams": teams})
}

func (s *Server) directorySourceNames(ctx context.Context) (map[string]string, error) {
	names := map[string]string{"scim\x00instance": "SCIM provisioning"}
	rows, err := s.DB.Query(ctx, "SELECT 'github',id,configuration FROM github_team_sync_sources UNION ALL SELECT 'ldap',id,configuration FROM ldap_providers")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id string
		var raw []byte
		if err = rows.Scan(&kind, &id, &raw); err != nil {
			return nil, err
		}
		plain, e := unseal(s.Key, raw)
		if e != nil {
			return nil, e
		}
		var c struct {
			Name    string `json:"name"`
			Display string `json:"display_name"`
			Account string `json:"account"`
		}
		if err = json.Unmarshal(plain, &c); err != nil {
			return nil, err
		}
		name := c.Display
		if kind == "github" {
			name = c.Name + " (" + c.Account + ")"
		}
		names[kind+"\x00"+id] = name
	}
	return names, rows.Err()
}

func (s *Server) scimActivity(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), `SELECT id,created_at,action,subject,status FROM audit_events WHERE actor='SCIM' AND action IN ('POST SCIM users','PUT SCIM users','PATCH SCIM users','DELETE SCIM users','POST SCIM groups','PUT SCIM groups','PATCH SCIM groups','DELETE SCIM groups') ORDER BY id DESC LIMIT 100`)
	if err != nil {
		http.Error(w, "provisioning activity unavailable", 503)
		return
	}
	defer rows.Close()
	activity := []map[string]any{}
	for rows.Next() {
		var id int64
		var at time.Time
		var operation, target string
		var status int
		if rows.Scan(&id, &at, &operation, &target, &status) != nil {
			http.Error(w, "provisioning activity unavailable", 503)
			return
		}
		outcome := "succeeded"
		if status >= 400 {
			outcome = "failed"
		}
		activity = append(activity, map[string]any{"id": id, "time": at, "operation": operation, "target_id": target, "status": status, "outcome": outcome})
	}
	if rows.Err() != nil {
		http.Error(w, "provisioning activity unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"activity": activity})
}
