package control

import (
	"net/http"
	"time"
)

func (s *Server) workspaceActivity(w http.ResponseWriter, r *http.Request) {
	p := actor(r)
	rows, err := s.DB.Query(r.Context(), `WITH events AS (
 SELECT 'operation' AS kind,o.id,o.action AS action,o.state AS state,w.name,'/operations/'||o.id AS path,o.created_at
 FROM operations o JOIN workloads w ON w.id=o.workload_id
 WHERE w.team_id=$2 AND ($3 OR cove_access($1,w.team_id,w.project_id,COALESCE(w.environment_id,''))>0)
 UNION ALL
 SELECT 'configuration',a.id::text,a.action,CASE WHEN a.status<400 THEN 'succeeded' ELSE 'failed' END,w.name,'/resources/'||w.id,a.created_at
 FROM audit_events a JOIN workloads w ON w.id=a.subject
 WHERE w.team_id=$2 AND a.team_id=$2 AND a.action NOT LIKE '%/operations%' AND a.action NOT LIKE '%/preflight%'
 AND a.action NOT LIKE '%/exec%' AND a.action NOT LIKE '%/logs%' AND a.action NOT LIKE '%/variables/export%'
 AND ($3 OR cove_access($1,w.team_id,w.project_id,COALESCE(w.environment_id,''))>0)
 UNION ALL
 SELECT 'configuration',a.id::text,a.action,CASE WHEN a.status<400 THEN 'succeeded' ELSE 'failed' END,p.name,'/projects/'||p.id,a.created_at
 FROM audit_events a JOIN projects p ON p.id=a.subject
 WHERE p.team_id=$2 AND a.team_id=$2 AND ($3 OR cove_access($1,p.team_id,p.id,'')>0)
) SELECT kind,id,action,state,name,path,created_at FROM events ORDER BY created_at DESC,kind,id DESC LIMIT 50`, p.ID, p.Team, p.Admin)
	if err != nil {
		http.Error(w, "activity unavailable", 503)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var kind, id, action, state, name, path string
		var created time.Time
		if rows.Scan(&kind, &id, &action, &state, &name, &path, &created) != nil {
			http.Error(w, "activity unavailable", 503)
			return
		}
		items = append(items, map[string]any{"kind": kind, "id": id, "action": action, "state": state, "name": name, "path": path, "created_at": created})
	}
	if rows.Err() != nil {
		http.Error(w, "activity unavailable", 503)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, map[string]any{"items": items})
}

func (s *Server) onboarding(w http.ResponseWriter, r *http.Request) {
	p := actor(r)
	if r.Method == "PUT" {
		var in struct {
			Dismissed bool `json:"dismissed"`
		}
		if !decode(w, r, &in) {
			return
		}
		if _, err := s.DB.Exec(r.Context(), `INSERT INTO user_onboarding(user_id,team_id,dismissed) VALUES($1,$2,$3) ON CONFLICT(user_id,team_id) DO UPDATE SET dismissed=EXCLUDED.dismissed`, p.ID, p.Team, in.Dismissed); err != nil {
			http.Error(w, "setup progress unavailable", 503)
			return
		}
	}
	var dismissed, ready, project, resource, deployed bool
	err := s.DB.QueryRow(r.Context(), `SELECT
 COALESCE((SELECT dismissed FROM user_onboarding WHERE user_id=$1 AND team_id=$2),false),
 EXISTS(SELECT 1 FROM nodes n WHERE n.state='ready' AND ($3 OR EXISTS(SELECT 1 FROM team_nodes t WHERE t.node_id=n.id AND t.team_id=$2))),
 EXISTS(SELECT 1 FROM projects p WHERE p.team_id=$2 AND ($3 OR cove_access($1,p.team_id,p.id,'')>0)),
 EXISTS(SELECT 1 FROM workloads w WHERE w.team_id=$2 AND ($3 OR cove_access($1,w.team_id,w.project_id,COALESCE(w.environment_id,''))>0)),
 EXISTS(SELECT 1 FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE w.team_id=$2 AND o.action IN ('deploy','rollback','adopt') AND o.state='succeeded' AND ($3 OR cove_access($1,w.team_id,w.project_id,COALESCE(w.environment_id,''))>0))`, p.ID, p.Team, p.Admin).Scan(&dismissed, &ready, &project, &resource, &deployed)
	if err != nil {
		http.Error(w, "setup progress unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"dismissed": dismissed, "complete": deployed, "can_enroll_node": p.Admin, "steps": []map[string]any{
		{"key": "account", "name": "Sign in to Cove", "complete": true, "path": "/account"},
		{"key": "node", "name": "Enroll and verify a worker", "complete": ready, "path": "/nodes"},
		{"key": "project", "name": "Create a project", "complete": project, "path": "/projects"},
		{"key": "resource", "name": "Configure an app", "complete": resource, "path": "/resources/new"},
		{"key": "deployment", "name": "Deploy your first app", "complete": deployed, "path": "/resources"},
	}})
}
