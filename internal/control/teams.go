package control

import (
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) { respond(w, 200, actor(r).Teams) }
func (s *Server) createTeam(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if len(in.Name) == 0 || len(in.Name) > 120 {
		http.Error(w, "name must contain 1-120 characters", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	id := resourceID()
	_, err = tx.Exec(r.Context(), "INSERT INTO teams(id,name) VALUES($1,$2)", id, in.Name)
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO team_members(team_id,user_id,role) VALUES($1,$2,'owner')", id, actor(r).ID)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not create team", 500)
		return
	}
	respond(w, 201, map[string]string{"id": id})
}

type memberInput struct {
	Role   string  `json:"role"`
	Grants []grant `json:"grants"`
}

func (s *Server) validGrants(r *http.Request, team string, in memberInput) bool {
	if in.Role != "owner" && in.Role != "admin" && in.Role != "member" {
		return false
	}
	if len(in.Grants) > 100 {
		return false
	}
	if in.Role != "member" && len(in.Grants) > 0 {
		return false
	}
	for _, g := range in.Grants {
		if g.Role != "viewer" && g.Role != "operator" && g.Role != "editor" {
			var exists bool
			if s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM team_roles WHERE team_id=$1 AND id=$2)", team, g.Role).Scan(&exists) != nil || !exists {
				return false
			}
		}
		var found bool
		var err error
		switch g.Scope {
		case "team":
			if g.ScopeID != "" && g.ScopeID != team {
				return false
			}
			continue
		case "project":
			err = s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1 AND team_id=$2)", g.ScopeID, team).Scan(&found)
		case "environment":
			err = s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM environments WHERE id=$1 AND team_id=$2)", g.ScopeID, team).Scan(&found)
		default:
			return false
		}
		if err != nil || !found {
			return false
		}
	}
	return true
}
func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), `SELECT u.id,COALESCE(u.email,''),u.name,m.role,m.grants,
 COALESCE((SELECT jsonb_agg(v) FROM (SELECT DISTINCT source_kind,source_id FROM identity_membership_claims c WHERE c.team_id=m.team_id AND c.user_id=m.user_id ORDER BY source_kind,source_id) v),'[]'::jsonb)
 FROM team_members m JOIN users u ON u.id=m.user_id WHERE m.team_id=$1 ORDER BY u.name,u.id`, r.PathValue("id"))
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	type member struct {
		ID      string            `json:"user_id"`
		Email   string            `json:"email"`
		Name    string            `json:"name"`
		Role    string            `json:"role"`
		Grants  []grant           `json:"grants"`
		Sources []directorySource `json:"managed_by"`
	}
	out := []member{}
	for rows.Next() {
		var m member
		if err = rows.Scan(&m.ID, &m.Email, &m.Name, &m.Role, &m.Grants, &m.Sources); err != nil {
			http.Error(w, "could not read members", 500)
			return
		}
		out = append(out, m)
	}
	if rows.Err() != nil {
		http.Error(w, "could not read members", 500)
		return
	}
	rows.Close()
	if actor(r).Admin {
		names, e := s.directorySourceNames(r.Context())
		if e != nil {
			http.Error(w, "membership sources unavailable", 503)
			return
		}
		for i := range out {
			for j := range out[i].Sources {
				source := &out[i].Sources[j]
				source.Name = names[source.Kind+"\x00"+source.ID]
			}
		}
	}
	respond(w, 200, out)
}
func (s *Server) changeMember(w http.ResponseWriter, r *http.Request) {
	in := memberInput{Grants: []grant{}}
	team, user := r.PathValue("id"), r.PathValue("user")
	if r.Method == "PUT" {
		if !decode(w, r, &in) {
			return
		}
		if !s.validGrants(r, team, in) {
			http.Error(w, "invalid role or permission scope", 400)
			return
		}
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	// Serialize membership changes so concurrent demotions cannot remove every owner.
	var locked string
	err = tx.QueryRow(r.Context(), "SELECT id FROM teams WHERE id=$1 FOR UPDATE", team).Scan(&locked)
	var callerRole, targetRole string
	if err == nil && !actor(r).Admin {
		err = tx.QueryRow(r.Context(), "SELECT role FROM team_members WHERE team_id=$1 AND user_id=$2", team, actor(r).ID).Scan(&callerRole)
		if err == nil && callerRole != "owner" && callerRole != "admin" {
			var allowed bool
			err = tx.QueryRow(r.Context(), "SELECT cove_permission($1,$2,'','','manage_members')", actor(r).ID, team).Scan(&allowed)
			if err != nil || !allowed || in.Role == "admin" {
				http.Error(w, "permission denied", 403)
				return
			}
		}
	}
	if err == nil {
		err = tx.QueryRow(r.Context(), "SELECT role FROM team_members WHERE team_id=$1 AND user_id=$2", team, user).Scan(&targetRole)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if !actor(r).Admin && callerRole != "owner" && (targetRole == "owner" || in.Role == "owner" || (callerRole != "admin" && targetRole == "admin")) {
		http.Error(w, "only owners can manage owners", 403)
		return
	}
	if targetRole == "owner" && (r.Method == "DELETE" || in.Role != "owner") {
		var count int
		err = tx.QueryRow(r.Context(), "SELECT count(*) FROM team_members m JOIN users u ON u.id=m.user_id WHERE m.team_id=$1 AND m.user_id<>$2 AND m.role='owner' AND NOT u.disabled", team, user).Scan(&count)
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		if count < 1 {
			http.Error(w, "team must keep an owner", 409)
			return
		}
	}
	var managed bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM identity_membership_bases WHERE team_id=$1 AND user_id=$2)", team, user).Scan(&managed); err != nil {
		http.Error(w, "membership settings unavailable", 503)
		return
	}
	if managed {
		if r.Method != "PUT" || in.Role != "owner" {
			http.Error(w, "membership is managed by a directory; change its group mappings instead", 409)
			return
		}
		if _, err = tx.Exec(r.Context(), "UPDATE identity_membership_bases SET role='owner',grants='[]' WHERE team_id=$1 AND user_id=$2", team, user); err != nil {
			http.Error(w, "membership settings unavailable", 503)
			return
		}
	}
	if r.Method == "DELETE" {
		_, err = tx.Exec(r.Context(), "DELETE FROM team_members WHERE team_id=$1 AND user_id=$2", team, user)
	} else {
		if in.Grants == nil {
			in.Grants = []grant{}
		}
		_, err = tx.Exec(r.Context(), "UPDATE team_members SET role=$3,grants=$4 WHERE team_id=$1 AND user_id=$2", team, user, in.Role, in.Grants)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not update member", 500)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) teamNodes(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("id")
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), "SELECT node_id FROM team_nodes WHERE team_id=$1 ORDER BY node_id", team)
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		defer rows.Close()
		ids := []string{}
		for rows.Next() {
			var id string
			if rows.Scan(&id) != nil {
				http.Error(w, "could not read nodes", 500)
				return
			}
			ids = append(ids, id)
		}
		if rows.Err() != nil {
			http.Error(w, "could not read nodes", 500)
			return
		}
		respond(w, 200, ids)
		return
	}
	var in struct {
		IDs []string `json:"node_ids"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.IDs) > 10000 {
		http.Error(w, "too many nodes", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var id string
	err = tx.QueryRow(r.Context(), "SELECT id FROM teams WHERE id=$1 FOR UPDATE", team).Scan(&id)
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM team_nodes WHERE team_id=$1", team)
	}
	for _, id := range in.IDs {
		if err == nil {
			_, err = tx.Exec(r.Context(), "INSERT INTO team_nodes(team_id,node_id) VALUES($1,$2) ON CONFLICT DO NOTHING", team, id)
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not assign nodes", 409)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

type invitation struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	Team          string    `json:"team_name"`
	Role          string    `json:"role"`
	Grants        []grant   `json:"grants"`
	Expires       time.Time `json:"expires_at"`
	AccountExists bool      `json:"account_exists"`
}

func (s *Server) invitations(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("id")
	if r.Method == "DELETE" {
		res, err := s.DB.Exec(r.Context(), "DELETE FROM invitations WHERE id=$1 AND team_id=$2", r.PathValue("invite"), team)
		if err != nil {
			http.Error(w, "could not revoke invitation", 500)
			return
		}
		if res.RowsAffected() == 0 {
			http.NotFound(w, r)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), "SELECT id,email,role,grants,expires_at FROM invitations WHERE team_id=$1 AND consumed_at IS NULL AND expires_at>now() ORDER BY created_at DESC", team)
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		defer rows.Close()
		out := []invitation{}
		for rows.Next() {
			var v invitation
			if rows.Scan(&v.ID, &v.Email, &v.Role, &v.Grants, &v.Expires) != nil {
				http.Error(w, "could not read invitations", 500)
				return
			}
			out = append(out, v)
		}
		if rows.Err() != nil {
			http.Error(w, "could not read invitations", 500)
			return
		}
		respond(w, 200, out)
		return
	}
	var in struct {
		Email  string  `json:"email"`
		Role   string  `json:"role"`
		Grants []grant `json:"grants"`
	}
	if !decode(w, r, &in) {
		return
	}
	email, valid := normalizeEmail(in.Email)
	if !s.teamAdmin(r, team) && in.Role == "admin" {
		http.Error(w, "permission denied", 403)
		return
	}
	if !valid || in.Role == "owner" || !s.validGrants(r, team, memberInput{in.Role, in.Grants}) {
		http.Error(w, "valid email, role, and permission scopes required", 400)
		return
	}
	if in.Grants == nil {
		in.Grants = []grant{}
	}
	id, token := resourceID(), randomToken()
	expiry := time.Now().Add(7 * 24 * time.Hour)
	_, err := s.DB.Exec(r.Context(), "INSERT INTO invitations(id,token_hash,team_id,email,role,grants,created_by,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", id, tokenHash(token), team, email, in.Role, in.Grants, actor(r).ID, expiry)
	if err != nil {
		http.Error(w, "could not create invitation", 500)
		return
	}
	url := s.PublicURL + "/invite#" + token
	sent := false
	sendMail, mailErr := s.mailer(r.Context())
	if mailErr != nil {
		slog.Error("invitation email settings unavailable")
	}
	if sendMail != nil {
		sent = sendMail(r.Context(), email, "Invitation to Cove", "You have been invited to a Cove team. Accept within seven days:\n\n"+url+"\n\nShared workers do not isolate teams from a hostile container administrator.") == nil
	}
	respond(w, 201, map[string]any{"id": id, "url": url, "expires_at": expiry, "email_sent": sent})
}
func (s *Server) previewInvitation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &in) {
		return
	}
	var v invitation
	err := s.DB.QueryRow(r.Context(), `SELECT i.id,i.email,t.name,i.role,i.grants,i.expires_at,EXISTS(SELECT 1 FROM users WHERE email=i.email) FROM invitations i JOIN teams t ON t.id=i.team_id JOIN users creator ON creator.id=i.created_by WHERE i.token_hash=$1 AND i.consumed_at IS NULL AND i.expires_at>now() AND NOT creator.disabled AND (creator.is_admin OR (cove_permission(i.created_by,i.team_id,'','','manage_members') AND (i.role<>'admin' OR EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=i.team_id AND m.user_id=i.created_by AND m.role IN ('owner','admin')))))`, tokenHash(in.Token)).Scan(&v.ID, &v.Email, &v.Team, &v.Role, &v.Grants, &v.Expires, &v.AccountExists)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "invalid or expired invitation", 400)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	respond(w, 200, v)
}
func (s *Server) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var id, team, email, role string
	var grants json.RawMessage
	err = tx.QueryRow(r.Context(), `SELECT i.id,i.team_id,i.email,i.role,i.grants FROM invitations i JOIN users creator ON creator.id=i.created_by WHERE token_hash=$1 AND consumed_at IS NULL AND expires_at>now() AND NOT creator.disabled AND (creator.is_admin OR (cove_permission(i.created_by,i.team_id,'','','manage_members') AND (i.role<>'admin' OR EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=i.team_id AND m.user_id=i.created_by AND m.role IN ('owner','admin'))))) FOR UPDATE OF i`, tokenHash(in.Token)).Scan(&id, &team, &email, &role, &grants)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "invalid or expired invitation", 400)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	var user string
	err = tx.QueryRow(r.Context(), "SELECT id FROM users WHERE email=$1", email).Scan(&user)
	if err == nil {
		p, e := s.sessionIdentity(r)
		if e != nil || p.ID != user {
			http.Error(w, "sign in to the invited account first", 401)
			return
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		hash, valid := passwordHash(in.Password)
		if !valid || strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 {
			http.Error(w, "name and password of 16 to 72 bytes required", 400)
			return
		}
		user = resourceID()
		_, err = tx.Exec(r.Context(), "INSERT INTO users(id,email,name,password_hash,email_verified) VALUES($1,$2,$3,$4,true)", user, email, strings.TrimSpace(in.Name), hash)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO team_members(team_id,user_id,role,grants) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", team, user, role, grants)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "UPDATE invitations SET consumed_at=now() WHERE id=$1", id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not accept invitation", 409)
		return
	}
	s.issueSession(w, r, user)
}
