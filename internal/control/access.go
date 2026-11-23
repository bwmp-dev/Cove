package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

type identityKey struct{}
type grant struct {
	Scope   string `json:"scope"`
	ScopeID string `json:"scope_id"`
	Role    string `json:"role"`
}
type membership struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Role   string  `json:"role"`
	Grants []grant `json:"grants"`
}
type identity struct {
	Permissions  []string     `json:"permissions"`
	ID           string       `json:"user_id"`
	Email        string       `json:"email"`
	Name         string       `json:"name"`
	Admin        bool         `json:"is_admin"`
	HasPassword  bool         `json:"has_password"`
	GitHubLinked bool         `json:"github_linked"`
	Team         string       `json:"team_id"`
	Teams        []membership `json:"teams"`
	User         string       `json:"user"`
}

func actor(r *http.Request) *identity {
	p, _ := r.Context().Value(identityKey{}).(*identity)
	if p == nil {
		return &identity{Teams: []membership{}}
	}
	return p
}
func (s *Server) identity(r *http.Request, id string) (*identity, error) {
	return s.loadIdentity(r, id, false)
}
func (s *Server) loadIdentity(r *http.Request, id string, recovery bool) (*identity, error) {
	p := &identity{Teams: []membership{}}
	err := s.DB.QueryRow(r.Context(), "SELECT id,COALESCE(email,''),name,is_admin,password_hash IS NOT NULL,github_id IS NOT NULL FROM users WHERE id=$1 AND ($2 OR (NOT disabled AND email_verified))", id, recovery).Scan(&p.ID, &p.Email, &p.Name, &p.Admin, &p.HasPassword, &p.GitHubLinked)
	if err != nil {
		return nil, err
	}
	p.Admin = p.Admin || recovery
	p.User = p.Name
	rows, err := s.DB.Query(r.Context(), "SELECT t.id,t.name,COALESCE(m.role,'admin'),COALESCE(m.grants,'[]'::jsonb) FROM teams t LEFT JOIN team_members m ON m.team_id=t.id AND m.user_id=$1 WHERE m.user_id=$1 OR $2 ORDER BY t.created_at,t.id", id, p.Admin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m membership
		if err = rows.Scan(&m.ID, &m.Name, &m.Role, &m.Grants); err != nil {
			return nil, err
		}
		p.Teams = append(p.Teams, m)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	p.Team = r.Header.Get("X-Cove-Team")
	if p.Team == "" {
		if p.Admin {
			p.Team = "default"
		} else if len(p.Teams) > 0 {
			p.Team = p.Teams[0].ID
		}
	}
	if p.Team != "" {
		found := false
		for _, m := range p.Teams {
			if m.ID == p.Team {
				found = true
			}
		}
		if !found {
			if !p.Admin {
				return nil, pgx.ErrNoRows
			}
			var exists bool
			err = s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM teams WHERE id=$1)", p.Team).Scan(&exists)
			if err != nil {
				return nil, err
			}
			if !exists {
				return nil, pgx.ErrNoRows
			}
		}
	}
	return p, nil
}
func (s *Server) sessionIdentity(r *http.Request) (*identity, error) {
	c, err := r.Cookie("__Host-cove")
	if err != nil {
		return nil, pgx.ErrNoRows
	}
	var id string
	err = s.DB.QueryRow(r.Context(), "SELECT user_id FROM sessions WHERE token_hash=$1 AND expires_at>now() AND (second_factor_verified OR NOT EXISTS(SELECT 1 FROM account_totp WHERE user_id=sessions.user_id AND confirmed) AND NOT EXISTS(SELECT 1 FROM passkeys WHERE user_id=sessions.user_id))", tokenHash(c.Value)).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.identity(r, id)
}
func (s *Server) sessionInfo(w http.ResponseWriter, r *http.Request) {
	p := actor(r)
	p.Permissions = []string{}
	for _, permission := range permissionCatalogue {
		allowed, err := s.permission(r, "team", "", permission)
		if err != nil {
			http.Error(w, "authorization unavailable", 503)
			return
		}
		if allowed {
			p.Permissions = append(p.Permissions, permission)
		}
	}
	respond(w, 200, p)
}
func permissions(level int) []string {
	p := []string{}
	if level >= 1 {
		p = append(p, "read")
	}
	if level >= 2 {
		p = append(p, "operate")
	}
	if level >= 3 {
		p = append(p, "edit")
	}
	return p
}
func (s *Server) access(r *http.Request, kind, id string) (int, error) {
	p := actor(r)
	var level int
	var err error
	switch kind {
	case "team":
		err = s.DB.QueryRow(r.Context(), "SELECT CASE WHEN $3 THEN 3 ELSE cove_access($1,$2,'','') END", p.ID, p.Team, p.Admin).Scan(&level)
	case "project":
		err = s.DB.QueryRow(r.Context(), "SELECT CASE WHEN $3 THEN 3 ELSE cove_access($1,p.team_id,p.id,'') END FROM projects p WHERE p.id=$4 AND p.team_id=$2", p.ID, p.Team, p.Admin, id).Scan(&level)
	case "environment":
		err = s.DB.QueryRow(r.Context(), "SELECT CASE WHEN $3 THEN 3 ELSE cove_access($1,e.team_id,e.project_id,e.id) END FROM environments e WHERE e.id=$4 AND e.team_id=$2", p.ID, p.Team, p.Admin, id).Scan(&level)
	case "workload":
		err = s.DB.QueryRow(r.Context(), "SELECT CASE WHEN $3 THEN 3 ELSE cove_access($1,w.team_id,w.project_id,COALESCE(e.id,'')) END FROM workloads w LEFT JOIN environments e ON e.id=w.environment_id WHERE w.id=$4 AND w.team_id=$2", p.ID, p.Team, p.Admin, id).Scan(&level)
	}
	return level, err
}
func (s *Server) requireAccess(w http.ResponseWriter, r *http.Request, kind, id string, needed int) bool {
	permission := "view"
	if needed == 2 {
		permission = "operate"
	}
	if needed == 3 {
		permission = "edit_configuration"
		if r.Method == "GET" {
			permission = "read_secrets"
		}
	}
	if strings.HasSuffix(r.URL.Path, "/credentials") || strings.HasSuffix(r.URL.Path, "/variables/export") || strings.HasSuffix(r.URL.Path, "/variables/resolve") || strings.HasSuffix(r.URL.Path, "/compose-bundle/export") || strings.HasSuffix(r.URL.Path, "/configuration/export") {
		permission = "read_secrets"
	}
	if strings.HasSuffix(r.URL.Path, "/backup-policy") && r.Method != "GET" {
		permission = "manage_backups"
	}
	if strings.HasSuffix(r.URL.Path, "/operations") && r.Method == "POST" {
		fields, ok := bodyFields(w, r)
		if !ok {
			return false
		}
		if action := field(fields, "action"); action == "backup" || action == "restore" || action == "drill" {
			if action != "backup" && !s.requirePermission(w, r, kind, id, "edit_configuration") {
				return false
			}
			permission = "manage_backups"
		}
	}
	return s.requirePermission(w, r, kind, id, permission)
}
func (s *Server) teamAdmin(r *http.Request, team string) bool {
	p := actor(r)
	if p.Admin {
		return true
	}
	for _, m := range p.Teams {
		if m.ID == team && (m.Role == "owner" || m.Role == "admin") {
			return true
		}
	}
	return false
}
func bodyFields(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2*1024*1024))
	if err != nil {
		http.Error(w, "invalid request", 400)
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	var v map[string]json.RawMessage
	if json.Unmarshal(b, &v) != nil {
		http.Error(w, "invalid request", 400)
		return nil, false
	}
	// encoding/json matches struct fields case-insensitively. Authorization must
	// use the same names and reject ambiguous case variants before the handler runs.
	fields := map[string]json.RawMessage{}
	for key, value := range v {
		for _, name := range []string{"project_id", "environment_id", "node_id", "workload_id", "action"} {
			if strings.EqualFold(key, name) {
				if _, duplicate := fields[name]; duplicate {
					http.Error(w, "ambiguous request fields", 400)
					return nil, false
				}
				fields[name] = value
			}
		}
	}
	return fields, true
}
func field(v map[string]json.RawMessage, k string) string {
	var s string
	_ = json.Unmarshal(v[k], &s)
	return s
}
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	p := actor(r)
	path := r.URL.Path
	if bearer(r) != s.AdminToken && p.ID != "" {
		var locked bool
		if err := s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM authentication_failures WHERE key=$1 AND locked_until>now())", "account:"+p.ID).Scan(&locked); err != nil {
			http.Error(w, "authentication unavailable", 503)
			return false
		}
		if locked {
			w.Header().Set("Retry-After", "900")
			http.Error(w, "too many attempts; try again later", 429)
			return false
		}
	}
	if bearer(r) != s.AdminToken && path != "/api/session" && path != "/api/teams" && !strings.HasPrefix(path, "/api/account") {
		var required bool
		policyTeam := p.Team
		if strings.HasPrefix(path, "/api/teams/") {
			policyTeam = r.PathValue("id")
		}
		err := s.DB.QueryRow(r.Context(), `SELECT (c.require_second_factor OR COALESCE((SELECT require_second_factor FROM teams WHERE id=$2),false)) AND NOT EXISTS(SELECT 1 FROM account_totp WHERE user_id=$1 AND confirmed) AND NOT EXISTS(SELECT 1 FROM passkeys WHERE user_id=$1) FROM control_settings c WHERE id=1`, p.ID, policyTeam).Scan(&required)
		if err != nil {
			http.Error(w, "authorization unavailable", 503)
			return false
		}
		if required {
			respond(w, 403, map[string]string{"error": "second_factor_enrolment_required"})
			return false
		}
	}
	id := r.PathValue("id")
	deny := func() bool { http.Error(w, "permission denied", 403); return false }
	switch {
	case path == "/api/preferences/timezone":
		return true
	case strings.HasPrefix(path, "/api/projects/") && strings.HasSuffix(path, "/notes") && r.Method == "GET":
		return s.requireAccess(w, r, "project", id, 1)
	case path == "/api/search" || path == "/api/activity" || path == "/api/onboarding" || path == "/api/resource-browser" || path == "/api/bulk-actions" || path == "/api/pins":
		return true
	case path == "/api/access-tokens" || strings.HasPrefix(path, "/api/access-tokens/"):
		return true
	case path == "/api/mcp":
		return p.Admin || deny()
	case path == "/api/templates" || path == "/api/templates/draft":
		return true
	case path == "/api/cloudflare" || strings.HasPrefix(path, "/api/cloudflare/"):
		return true
	case path == "/api/github/app" || path == "/api/github/app/connect" || strings.HasPrefix(path, "/api/github/app/installations/") && r.Method == "PUT":
		return true
	case strings.HasPrefix(path, "/api/github/app/installations/") && r.Method == "DELETE":
		return s.requireAccess(w, r, "team", "", 3)
	case strings.HasPrefix(path, "/api/registries") || strings.HasPrefix(path, "/api/storage-locations"):
		if r.Method == "GET" {
			return true
		}
		if !s.teamAdmin(r, p.Team) {
			return deny()
		}
		return s.requirePermission(w, r, "team", "", "edit_configuration") && s.requirePermission(w, r, "team", "", "read_secrets")
	case strings.HasPrefix(path, "/api/notifications"):
		if !s.teamAdmin(r, p.Team) {
			return deny()
		}
		if r.Method == "GET" {
			return s.requirePermission(w, r, "team", "", "view")
		}
		return s.requirePermission(w, r, "team", "", "edit_configuration") && s.requirePermission(w, r, "team", "", "read_secrets")
	case strings.HasPrefix(path, "/api/terminal") || strings.HasPrefix(path, "/api/github/app"):
		if !p.Admin {
			return deny()
		}
		return true
	case strings.HasPrefix(path, "/api/git/") || path == "/api/secret-providers" || strings.HasPrefix(path, "/api/secret-providers/"):
		level := 1
		if r.Method != "GET" {
			level = 3
		}
		return s.requireAccess(w, r, "team", "", level)
	case path == "/api/github/repositories":
		return s.requireAccess(w, r, "team", "", 3)
	case path == "/api/shared-variables":
		if !s.teamAdmin(r, p.Team) {
			return deny()
		}
		return true
	case strings.HasPrefix(path, "/api/account/") || path == "/api/session" || path == "/api/account" || path == "/api/password" || path == "/api/teams":
		return true
	case path == "/api/settings/metrics-token":
		if !p.Admin {
			return deny()
		}
		if r.Method == "GET" {
			return s.requirePermission(w, r, "team", "", "view")
		}
		return s.requirePermission(w, r, "team", "", "read_secrets")
	case strings.HasPrefix(path, "/api/settings/panel-backup"):
		if !p.Admin {
			return deny()
		}
		if r.Method == "GET" {
			return s.requirePermission(w, r, "team", "", "view")
		}
		return s.requirePermission(w, r, "team", "", "manage_backups") && s.requirePermission(w, r, "team", "", "edit_configuration") && s.requirePermission(w, r, "team", "", "read_secrets")
	case strings.HasPrefix(path, "/api/users") || (path == "/api/settings" || strings.HasPrefix(path, "/api/settings/")) || path == "/api/updates":
		if !p.Admin {
			return deny()
		}
		permission := "view"
		if r.Method != "GET" {
			permission = "edit_configuration"
		}
		return s.requirePermission(w, r, "team", "", permission)
	case strings.HasPrefix(path, "/api/teams/"):
		if !s.teamAdmin(r, id) {
			if !strings.Contains(path, "/members") && !strings.Contains(path, "/invitations") && !(r.Method == "GET" && path == "/api/teams/"+id+"/roles") {
				return deny()
			}
			var allowed bool
			if s.DB.QueryRow(r.Context(), "SELECT cove_permission($1,$2,'','','manage_members')", p.ID, id).Scan(&allowed) != nil || !allowed {
				return deny()
			}
		}
		if strings.HasSuffix(path, "/nodes") && r.Method != "GET" && !p.Admin {
			return deny()
		}
		return true
	case strings.HasPrefix(path, "/api/nodes"):
		if r.Pattern == "GET /api/nodes" {
			return true
		}
		if !p.Admin {
			return deny()
		}
		permission := "view"
		if r.Method != "GET" {
			permission = "edit_configuration"
		}
		return s.requirePermission(w, r, "team", "", permission)
	case path == "/api/private-connections" || strings.HasPrefix(path, "/api/private-connections/"):
		// Handlers check access to both endpoints of the connection.
		return true
	case path == "/api/networks" || strings.HasSuffix(path, "/networks"):
		if !s.teamAdmin(r, p.Team) {
			return deny()
		}
		if id != "" {
			return s.requireAccess(w, r, "workload", id, 3)
		}
		return true
	case path == "/api/audit":
		if !s.teamAdmin(r, p.Team) {
			return deny()
		}
		return true
	case path == "/api/projects":
		if r.Method == "GET" {
			return true
		}
		return s.requireAccess(w, r, "team", "", 3)
	case strings.HasPrefix(path, "/api/environment-previews/"):
		// The handler authorizes against the preview's production environment.
		return true
	case strings.HasPrefix(path, "/api/projects/") && strings.HasSuffix(path, "/environment-previews") && r.Method == "GET":
		// Each preview is filtered by access to its production environment.
		return true
	case strings.HasPrefix(path, "/api/projects/") && strings.HasSuffix(path, "/configuration/compare") && r.Method == "GET":
		return true
	case strings.HasPrefix(path, "/api/projects/") && (strings.HasSuffix(path, "/configuration") || strings.HasSuffix(path, "/configuration/gitops")) && r.Method == "GET":
		return s.requireAccess(w, r, "project", id, 1)
	case strings.HasPrefix(path, "/api/projects/") && strings.HasSuffix(path, "/costs") && r.Method == "GET":
		return s.requireAccess(w, r, "project", id, 1)
	case strings.HasPrefix(path, "/api/projects/"):
		if strings.HasSuffix(path, "/configuration/export") {
			return true
		}
		if strings.HasSuffix(path, "/status-share") || strings.HasSuffix(path, "/status-page") || strings.Contains(path, "/configuration/drafts") {
			return s.requirePermission(w, r, "project", id, "edit_configuration")
		}
		if r.Method == "GET" && r.Context().Value(apiRouteKey{}) != nil {
			return s.requireAccess(w, r, "project", id, 1)
		}
		if r.Method == "GET" && strings.HasSuffix(path, "/connections") {
			// Environment-scoped members view their environment's graph; each resource is still filtered by access.
			if environment := r.URL.Query().Get("environment"); environment != "" {
				return s.requireAccess(w, r, "environment", environment, 1)
			}
			return s.requireAccess(w, r, "project", id, 1)
		}
		if r.Method == "GET" && strings.HasSuffix(path, "/dependencies") {
			return s.requireAccess(w, r, "project", id, 1)
		}
		return s.requireAccess(w, r, "project", id, 3)
	case strings.HasPrefix(path, "/api/environments/"):
		if r.Method == "GET" && r.Context().Value(apiRouteKey{}) != nil {
			return s.requireAccess(w, r, "environment", id, 1)
		}
		if r.Method != "GET" {
			var preview bool
			if err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM environment_previews WHERE environment_id=$1)`, id).Scan(&preview); err != nil {
				http.Error(w, "authorization unavailable", 503)
				return false
			}
			if preview {
				http.Error(w, "preview environments are managed by their preview; delete the preview instead", 409)
				return false
			}
		}
		return s.requireAccess(w, r, "environment", id, 3)
	case path == "/api/workloads":
		if r.Method == "GET" {
			return true
		}
		v, ok := bodyFields(w, r)
		if !ok {
			return false
		}
		env := field(v, "environment_id")
		if env != "" {
			if !s.allowManualPlacement(w, r, env) {
				return false
			}
			if !s.requireAccess(w, r, "environment", env, 3) {
				return false
			}
		} else {
			project := field(v, "project_id")
			if project == "" {
				http.Error(w, "a project is required", 400)
				return false
			}
			if !s.requireAccess(w, r, "project", project, 3) {
				return false
			}
		}
		var allowed bool
		err := s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM team_nodes WHERE team_id=$1 AND node_id=$2)", p.Team, field(v, "node_id")).Scan(&allowed)
		if err != nil {
			http.Error(w, "authorization unavailable", 503)
			return false
		}
		if !p.Admin && !allowed {
			return deny()
		}
		return true
	case strings.HasPrefix(path, "/api/workloads/"):
		if r.Method != "GET" {
			var preview bool
			if err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM previews WHERE workload_id=$1)`, id).Scan(&preview); err != nil {
				http.Error(w, "authorization unavailable", 503)
				return false
			}
			var environmentPreview bool
			if err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM environment_preview_resources WHERE resource_id=$1)`, id).Scan(&environmentPreview); err != nil {
				http.Error(w, "authorization unavailable", 503)
				return false
			}
			if environmentPreview {
				allowed := strings.HasSuffix(path, "/preflight") || strings.HasSuffix(path, "/database-access")
				if strings.HasSuffix(path, "/operations") {
					values, ok := bodyFields(w, r)
					if !ok {
						return false
					}
					action := field(values, "action")
					allowed = action == "stop" || action == "start" || action == "restart"
				}
				if !allowed {
					http.Error(w, "this resource is managed by its environment preview; change the preview recipe or use the preview's actions", 409)
					return false
				}
			}
			if preview {
				allowed := strings.HasSuffix(path, "/preflight")
				if strings.HasSuffix(path, "/operations") {
					values, ok := bodyFields(w, r)
					if !ok {
						return false
					}
					allowed = field(values, "action") == "stop"
				}
				if !allowed {
					http.Error(w, "preview configuration is managed by its source resource; close it from the Previews page", 409)
					return false
				}
			}
		}
		needed := 1
		if r.Method != "GET" {
			needed = 3
		}
		if strings.HasSuffix(path, "/operations") {
			v, ok := bodyFields(w, r)
			if !ok {
				return false
			}
			if field(v, "action") != "restore" && field(v, "action") != "drill" {
				needed = 2
			}
		}
		if !s.requireAccess(w, r, "workload", id, needed) {
			return false
		}
		if strings.Contains(path, "/files") {
			if !p.Admin {
				return deny()
			}
			if !s.requirePermission(w, r, "workload", id, "read_secrets") {
				return false
			}
		}
		if strings.HasSuffix(path, "/backups/snapshot-preview") || strings.Contains(path, "/backups/") && (strings.HasSuffix(path, "/download") || strings.HasSuffix(path, "/import") || strings.HasSuffix(path, "/import-s3")) {
			if !s.requirePermission(w, r, "workload", id, "manage_backups") {
				return false
			}
			if !strings.HasSuffix(path, "/snapshot-preview") && !s.requirePermission(w, r, "workload", id, "read_secrets") {
				return false
			}
		}
		if r.Method != "GET" && !s.requireAdoptionComplete(w, r, id) {
			return false
		}
		if strings.HasSuffix(path, "/environment") {
			v, ok := bodyFields(w, r)
			if !ok {
				return false
			}
			env := field(v, "environment_id")
			if env != "" {
				return s.allowManualPlacement(w, r, env) && s.requireAccess(w, r, "environment", env, 3)
			}
			project := field(v, "project_id")
			if project == "" {
				http.Error(w, "a project is required", 400)
				return false
			}
			return s.requireAccess(w, r, "project", project, 3)
		}
		return true
	case path == "/api/operations":
		return true
	case strings.HasPrefix(path, "/api/operations/"):
		var workload string
		err := s.DB.QueryRow(r.Context(), "SELECT workload_id FROM operations WHERE id=$1", id).Scan(&workload)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return false
		}
		if err != nil {
			http.Error(w, "authorization unavailable", 503)
			return false
		}
		needed := 1
		if r.Method != "GET" {
			needed = 2
		}
		return s.requireAccess(w, r, "workload", workload, needed)
	case path == "/api/tokens":
		if r.Method == "GET" {
			return true
		}
		v, ok := bodyFields(w, r)
		if !ok {
			return false
		}
		return s.requireAccess(w, r, "workload", field(v, "workload_id"), 3)
	case strings.HasPrefix(path, "/api/tokens/"):
		var workload, owner string
		err := s.DB.QueryRow(r.Context(), "SELECT workload_id,user_id FROM api_tokens WHERE id=$1 AND team_id=$2", id, p.Team).Scan(&workload, &owner)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return false
		}
		if err != nil {
			http.Error(w, "authorization unavailable", 503)
			return false
		}
		if owner != p.ID && !s.teamAdmin(r, p.Team) {
			return deny()
		}
		return s.requireAccess(w, r, "workload", workload, 1)
	}
	return deny()
}

// allowManualPlacement refuses to place resources into a preview environment by hand; previews
// contain exactly the copies their recipe creates.
func (s *Server) allowManualPlacement(w http.ResponseWriter, r *http.Request, environment string) bool {
	var preview bool
	if err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM environment_previews WHERE environment_id=$1)`, environment).Scan(&preview); err != nil {
		http.Error(w, "authorization unavailable", 503)
		return false
	}
	if preview {
		http.Error(w, "resources cannot be added to a preview environment", 409)
		return false
	}
	return true
}
