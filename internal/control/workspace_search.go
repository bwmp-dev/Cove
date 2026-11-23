package control

import (
	"net/http"
	"strings"
)

type searchResult struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
	Path   string `json:"path"`
}

func (s *Server) workspaceSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) > 200 {
		http.Error(w, "search must contain at most 200 bytes", 400)
		return
	}
	p := actor(r)
	rows, err := s.DB.Query(r.Context(), `
WITH visible AS (
 SELECT 'project' AS kind,p.name,'' AS detail,'/projects/'||p.id AS path FROM projects p
 WHERE p.team_id=$2 AND ($3 OR cove_access($1,p.team_id,p.id,'')>0)
 UNION ALL
 SELECT 'environment',e.name,p.name,'/projects/'||p.id||'/environments/'||e.id FROM environments e JOIN projects p ON p.id=e.project_id
 WHERE e.team_id=$2 AND ($3 OR cove_access($1,e.team_id,e.project_id,e.id)>0)
 UNION ALL
 SELECT 'workload',w.name,p.name||COALESCE(' / '||e.name,''),'/resources/'||w.id FROM workloads w JOIN projects p ON p.id=w.project_id LEFT JOIN environments e ON e.id=w.environment_id
 WHERE w.team_id=$2 AND ($3 OR cove_access($1,w.team_id,w.project_id,COALESCE(w.environment_id,''))>0)
 UNION ALL
 SELECT 'page',w.name||' Â· '||section.label,p.name,'/resources/'||w.id||'/'||section.path FROM workloads w JOIN projects p ON p.id=w.project_id
 CROSS JOIN (VALUES ('logs','Logs'),('metrics','Metrics'),('backups','Backups'),('troubleshooting','Troubleshooting'),('files','Files'),('database','Database'),('object-storage','Object storage')) section(path,label)
 WHERE w.team_id=$2 AND ($3 OR cove_access($1,w.team_id,w.project_id,COALESCE(w.environment_id,''))>0) AND (section.path<>'files' OR w.kind<>'database' AND ($3 OR cove_permission($1,w.team_id,w.project_id,COALESCE(w.environment_id,''),'edit_configuration') AND cove_permission($1,w.team_id,w.project_id,COALESCE(w.environment_id,''),'read_secrets'))) AND (section.path<>'database' OR w.kind='database') AND (section.path<>'object-storage' OR w.kind<>'database')
 UNION ALL
 SELECT 'node',n.name,n.state,CASE WHEN $3 THEN '/nodes/'||n.id ELSE '/nodes' END FROM nodes n
 WHERE $3 OR EXISTS(SELECT 1 FROM team_nodes t WHERE t.node_id=n.id AND t.team_id=$2)
 UNION ALL
 SELECT 'operation',w.name||' Â· '||o.action,o.state||' Â· '||left(o.id,8),'/operations/'||o.id FROM operations o JOIN workloads w ON w.id=o.workload_id
 WHERE w.team_id=$2 AND ($3 OR cove_access($1,w.team_id,w.project_id,COALESCE(w.environment_id,''))>0)
)
SELECT kind,name,detail,path FROM visible WHERE strpos(lower(name),lower($4))>0 OR (kind='operation' AND strpos(path,$4)>0)
ORDER BY CASE kind WHEN 'workload' THEN 0 WHEN 'project' THEN 1 WHEN 'environment' THEN 2 WHEN 'node' THEN 3 ELSE 4 END,lower(name),path LIMIT 40`, p.ID, p.Team, p.Admin, query)
	if err != nil {
		http.Error(w, "search unavailable; try again", 503)
		return
	}
	defer rows.Close()
	items := []searchResult{}
	for rows.Next() {
		var item searchResult
		if err := rows.Scan(&item.Kind, &item.Name, &item.Detail, &item.Path); err != nil {
			http.Error(w, "could not read search results", 500)
			return
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		http.Error(w, "could not read search results", 500)
		return
	}
	pages := []searchResult{
		{"page", "Dashboard", "Workspace", "/dashboard"},
		{"page", "Projects", "Workspace", "/projects"},
		{"page", "All resources", "Workspace", "/resources"},
		{"page", "Operations", "Workspace", "/operations"},
		{"page", "Nodes", "Infrastructure", "/nodes"},
		{"page", "Integrations", "Infrastructure", "/integrations"},
		{"page", "Docker registries", "Infrastructure", "/registries"},
		{"page", "Storage locations", "Infrastructure", "/storage"},
		{"page", "Team settings", "Manage", "/team"},
		{"page", "API keys and tokens", "Manage", "/tokens"},
		{"page", "Account", "Manage", "/account"},
	}
	if s.teamAdmin(r, p.Team) {
		pages = append(pages, searchResult{"page", "Notifications", "Manage", "/notifications"}, searchResult{"page", "Audit log", "Manage", "/audit"})
		pages = append(pages, searchResult{"page", "Log forwarding", "Notifications", "/notifications"}, searchResult{"page", "Threshold rules", "Notifications", "/notifications"})
	}
	if p.Admin {
		pages = append(pages, searchResult{"page", "Cloud server provisioning", "Infrastructure", "/nodes"}, searchResult{"page", "SSH bastions", "Infrastructure", "/nodes"}, searchResult{"page", "Host security updates", "Infrastructure", "/nodes"}, searchResult{"page", "Host firewall", "Infrastructure", "/nodes"})
		pages = append(pages, searchResult{"page", "Directory sign-in", "Settings", "/admin"}, searchResult{"page", "LDAP sign-in", "Settings", "/admin"}, searchResult{"page", "SCIM provisioning", "Settings", "/admin"}, searchResult{"page", "GitHub team sync", "Settings", "/admin"})
		pages = append(pages, searchResult{"page", "Settings", "Administration", "/admin"}, searchResult{"page", "Users", "Administration", "/users"}, searchResult{"page", "Terminal", "Administration", "/terminal"})
		pages = append(pages, searchResult{"page", "Panel backup and recovery", "Settings", "/admin"}, searchResult{"page", "Telemetry retention", "Settings", "/admin"}, searchResult{"page", "Prometheus export", "Settings", "/admin"})
	}
	for _, page := range pages {
		if strings.Contains(strings.ToLower(page.Name), strings.ToLower(query)) {
			items = append(items, page)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, map[string]any{"items": items, "limit": 40})
}
