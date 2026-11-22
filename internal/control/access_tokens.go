package control

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type apiScope struct {
	Name          string `json:"name"`
	Level         int    `json:"level"`
	Description   string `json:"description"`
	InstanceAdmin bool   `json:"instance_admin"`
	PersonalOnly  bool   `json:"personal_only"`
}

var apiScopes = []apiScope{
	{"projects:read", 1, "Read accessible projects and environments.", false, false},
	{"projects:write", 3, "Create, rename and delete projects and environments.", false, false},
	{"configuration:write", 3, "Save and apply project declarations, imports and resource moves; policies and ownership transfers also require team administration.", false, false},
	{"migration:adopt", 3, "Instance administrators only: inspect and adopt unmanaged Compose projects; personal tokens only.", true, true},
	{"workloads:read", 1, "Read accessible resources, runtime and release metadata.", false, false},
	{"workloads:write", 3, "Create, configure and delete resources and routes.", false, false},
	{"workloads:operate", 2, "Deploy, restart, stop, start, roll back and back up resources.", false, false},
	{"workloads:restore", 3, "Restore resource data.", false, false},
	{"logs:read", 1, "Read operation and container logs.", false, false},
	{"secrets:read", 3, "Read audited variable and configuration-file exports.", false, false},
	{"secrets:write", 3, "Update resource variables.", false, false},
	{"exec:run", 3, "Run a bounded one-off command in a resource container.", false, false},
	{"tunnels:write", 3, "Create and revoke temporary service tunnel leases.", false, false},
}

func apiScopeLevel(name string) int {
	for _, scope := range apiScopes {
		if scope.Name == name {
			return scope.Level
		}
	}
	return 0
}

type apiRouteKey struct{}
type apiCredential struct {
	ID     string
	Team   string
	Scopes []string
}
type apiCredentialKey struct{}

func (s *Server) authenticatePublicToken(next http.Handler, w http.ResponseWriter, r *http.Request, route publicRoute) {
	var id, owner, team string
	var scopes []string
	err := s.DB.QueryRow(r.Context(), `SELECT id,user_id,COALESCE(team_id,''),scopes FROM access_tokens WHERE token_hash=$1 AND expires_at>now()`, tokenHash(bearer(r))).Scan(&id, &owner, &team, &scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "unauthorized", 401)
		return
	}
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	if route.Scope != "" && !slices.Contains(scopes, route.Scope) {
		http.Error(w, "token scope does not permit this action", 403)
		return
	}
	if team != "" {
		if selected := r.Header.Get("X-Cove-Team"); selected != "" && selected != team {
			http.Error(w, "token belongs to another team", 403)
			return
		}
		r = r.Clone(r.Context())
		r.Header.Set("X-Cove-Team", team)
	}
	p, err := s.identity(r, owner)
	if err != nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	ctx := context.WithValue(r.Context(), identityKey{}, p)
	ctx = context.WithValue(ctx, apiCredentialKey{}, apiCredential{ID: id, Team: team, Scopes: scopes})
	r = r.WithContext(ctx)
	s.serveAuthorized(next, w, r, "API token "+id)
}

func (s *Server) accessTokens(w http.ResponseWriter, r *http.Request) {
	p := actor(r)
	w.Header().Set("Cache-Control", "no-store")
	if strings.HasSuffix(r.URL.Path, "/scopes") {
		respond(w, 200, apiScopes)
		return
	}
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), `SELECT id,name,user_id,COALESCE(team_id,''),scopes,expires_at FROM access_tokens WHERE user_id=$1 OR (team_id=$2 AND $3) ORDER BY created_at DESC`, p.ID, p.Team, s.teamAdmin(r, p.Team))
		if err != nil {
			http.Error(w, "tokens unavailable", 503)
			return
		}
		defer rows.Close()
		result := []map[string]any{}
		for rows.Next() {
			var id, name, owner, team string
			var scopes []string
			var expiry time.Time
			if rows.Scan(&id, &name, &owner, &team, &scopes, &expiry) != nil {
				http.Error(w, "tokens unavailable", 503)
				return
			}
			scope := "user"
			if team != "" {
				scope = "team"
			}
			result = append(result, map[string]any{"id": id, "name": name, "user_id": owner, "team_id": team, "scope": scope, "scopes": scopes, "expires_at": expiry})
		}
		if rows.Err() != nil {
			http.Error(w, "tokens unavailable", 503)
			return
		}
		respond(w, 200, result)
		return
	}
	if r.Method == "DELETE" {
		result, err := s.DB.Exec(r.Context(), `DELETE FROM access_tokens WHERE id=$1 AND (user_id=$2 OR (team_id=$3 AND $4))`, r.PathValue("token"), p.ID, p.Team, s.teamAdmin(r, p.Team))
		if err != nil {
			http.Error(w, "could not revoke token", 503)
			return
		}
		if result.RowsAffected() == 0 {
			http.NotFound(w, r)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	creator, err := s.identity(r, p.ID)
	if err != nil || creator.Email == "" {
		http.Error(w, "sign in with an individual verified account before creating a public API token", 409)
		return
	}
	p = creator
	r = r.WithContext(context.WithValue(r.Context(), identityKey{}, p))
	var in struct {
		Name   string   `json:"name"`
		Scope  string   `json:"scope"`
		Scopes []string `json:"scopes"`
		Days   int      `json:"days"`
	}
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 || (in.Scope != "user" && in.Scope != "team") || in.Days < 1 || in.Days > 365 || len(in.Scopes) == 0 || len(in.Scopes) > len(apiScopes) {
		http.Error(w, "name, user/team scope, explicit permissions and expiry of 1-365 days required", 400)
		return
	}
	required := 0
	seen := map[string]bool{}
	for _, scope := range in.Scopes {
		level := apiScopeLevel(scope)
		if level == 0 || seen[scope] {
			http.Error(w, "invalid or repeated token permission", 400)
			return
		}
		seen[scope] = true
		for _, permission := range apiScopes {
			if permission.Name == scope && (permission.InstanceAdmin && !p.Admin || permission.PersonalOnly && in.Scope != "user") {
				http.Error(w, "this permission requires an instance administrator's personal API key", 403)
				return
			}
		}
		required = max(required, level)
	}
	var available int
	err = s.DB.QueryRow(r.Context(), `SELECT CASE WHEN $3 THEN 3 ELSE GREATEST(cove_access($1,$2,'',''),COALESCE((SELECT max(cove_access($1,p.team_id,p.id,COALESCE(e.id,''))) FROM projects p LEFT JOIN environments e ON e.project_id=p.id WHERE p.team_id=$2),0)) END`, p.ID, p.Team, p.Admin).Scan(&available)
	if err != nil {
		http.Error(w, "authorization unavailable", 503)
		return
	}
	if available < required || (in.Scope == "team" && !s.teamAdmin(r, p.Team)) {
		http.Error(w, "permission denied", 403)
		return
	}
	var team any
	if in.Scope == "team" {
		team = p.Team
	}
	id, token := resourceID(), randomToken()
	expiry := time.Now().UTC().Add(time.Duration(in.Days) * 24 * time.Hour)
	if _, err = s.DB.Exec(r.Context(), `INSERT INTO access_tokens(id,name,user_id,team_id,scopes,token_hash,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, strings.TrimSpace(in.Name), p.ID, team, in.Scopes, tokenHash(token), expiry); err != nil {
		http.Error(w, "could not save token", 503)
		return
	}
	respond(w, 201, map[string]any{"id": id, "token": token, "expires_at": expiry})
}
