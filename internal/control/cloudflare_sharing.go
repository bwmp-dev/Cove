package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type cloudflareShare struct {
	Team    string   `json:"team_id"`
	Domains []string `json:"domains"`
}

func validDNSName(name string) bool {
	if len(name) > 253 || !strings.Contains(name, ".") {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
				return false
			}
		}
	}
	return true
}

func cloudflareDomainAllowed(patterns []string, domain string) bool {
	if !validDNSName(domain) {
		return false
	}
	for _, pattern := range patterns {
		if pattern == domain || strings.HasPrefix(pattern, "*.") && strings.HasSuffix(domain, pattern[1:]) {
			return true
		}
	}
	return false
}

func (s *Server) cloudflareSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		s.listCloudflareConnections(w, r)
		return
	}
	var c cloudflareZone
	if !decode(w, r, &c) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var raw []byte
	var manage bool
	err = tx.QueryRow(r.Context(), "SELECT configuration,COALESCE($2 OR owner_id=$3,false) FROM cloudflare_zones WHERE zone_id=$1 FOR UPDATE", c.ZoneID, actor(r).Admin, actor(r).ID).Scan(&raw, &manage)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "DNS settings unavailable", 503)
		return
	}
	if exists && !manage {
		http.Error(w, "connection owner access required", 403)
		return
	}
	var old cloudflareZone
	if exists {
		data, e := unseal(s.Key, raw)
		if e != nil || json.Unmarshal(data, &old) != nil {
			http.Error(w, "DNS configuration unavailable", 503)
			return
		}
		if old.Domain != c.Domain {
			http.Error(w, "a connected zone's domain cannot be changed", 400)
			return
		}
		if c.Token == "" {
			c.Token = old.Token
		}
		c.ConnectionID = old.ConnectionID
	}
	if !exists && c.ConnectionID != "" {
		http.Error(w, "select domains from your Cloudflare connection", 400)
		return
	}
	if c.ConnectionID != "" {
		c.Token = ""
	}
	if err := validateCloudflare(c); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	for node, address := range c.Nodes {
		if old.Nodes[node] == address {
			continue
		}
		var available bool
		err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM nodes n WHERE n.id=$1 AND ($2 OR EXISTS(SELECT 1 FROM team_nodes t WHERE t.node_id=n.id AND cove_access($3,t.team_id,'','')>=3)))", node, actor(r).Admin, actor(r).ID).Scan(&available)
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		if !available {
			http.Error(w, "node is unavailable to your editable teams", 400)
			return
		}
	}
	var zone struct {
		Name string `json:"name"`
	}
	if err = s.cloudflareRequest(r.Context(), c, "GET", "", nil, &zone); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	if zone.Name != c.Domain {
		http.Error(w, "zone ID does not match the domain", 400)
		return
	}
	raw, err = json.Marshal(c)
	if err == nil {
		raw, err = seal(s.Key, raw)
	}
	if err != nil {
		http.Error(w, "could not protect credentials", 503)
		return
	}
	if exists {
		_, err = tx.Exec(r.Context(), "UPDATE cloudflare_zones SET configuration=$2,checked_at=NULL WHERE zone_id=$1", c.ZoneID, raw)
	} else {
		tag, insertErr := tx.Exec(r.Context(), "INSERT INTO cloudflare_zones(zone_id,domain,owner_id,configuration) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", c.ZoneID, c.Domain, actor(r).ID, raw)
		err = insertErr
		if err == nil && tag.RowsAffected() != 1 {
			http.Error(w, "zone is already connected", 409)
			return
		}

	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save DNS settings", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) listCloudflareConnections(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), `SELECT z.configuration,z.last_error,z.checked_at,COALESCE($1 OR z.owner_id=$2,false),
 COALESCE((SELECT json_agg(json_build_object('team_id',t.team_id,'domains',t.domains) ORDER BY t.team_id) FROM cloudflare_zone_teams t WHERE t.zone_id=z.zone_id AND ($1 OR cove_access($2,t.team_id,'','')>=3)),'[]')
 FROM cloudflare_zones z WHERE $1 OR z.owner_id=$2 OR EXISTS(SELECT 1 FROM cloudflare_zone_teams t WHERE t.zone_id=z.zone_id AND t.team_id=$3 AND cove_access($2,t.team_id,'','')>=3) ORDER BY z.domain`, actor(r).Admin, actor(r).ID, actor(r).Team)
	if err != nil {
		http.Error(w, "DNS settings unavailable", 503)
		return
	}
	defer rows.Close()
	type item struct {
		cloudflareZone
		Manage  bool              `json:"can_manage"`
		Shares  []cloudflareShare `json:"shares"`
		Error   string            `json:"error"`
		Checked *time.Time        `json:"checked_at"`
	}
	out := []item{}
	for rows.Next() {
		var raw, shares []byte
		var v item
		if err = rows.Scan(&raw, &v.Error, &v.Checked, &v.Manage, &shares); err != nil {
			break
		}
		raw, err = unseal(s.Key, raw)
		if err != nil {
			break
		}
		if err = json.Unmarshal(raw, &v.cloudflareZone); err != nil {
			break
		}
		if err = json.Unmarshal(shares, &v.Shares); err != nil {
			break
		}
		v.Token = ""
		if !v.Manage {
			v.Nodes = map[string]string{}
			if v.Error != "" {
				v.Error = "DNS reconciliation needs attention from the connection owner"
			}
		}
		out = append(out, v)
	}
	if err != nil || rows.Err() != nil {
		http.Error(w, "DNS configuration unavailable", 503)
		return
	}
	respond(w, 200, out)
}

func (s *Server) cloudflareShares(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Shares []cloudflareShare `json:"shares"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Shares == nil || len(in.Shares) > 100 {
		http.Error(w, "shares must contain at most 100 teams", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var domain string
	var manage bool
	err = tx.QueryRow(r.Context(), "SELECT domain,COALESCE($2 OR owner_id=$3,false) FROM cloudflare_zones WHERE zone_id=$1 FOR UPDATE", r.PathValue("zone"), actor(r).Admin, actor(r).ID).Scan(&domain, &manage)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !manage {
		http.Error(w, "connection owner access required", 403)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	seen := map[string]bool{}
	for _, share := range in.Shares {
		if share.Team == "" || seen[share.Team] || len(share.Domains) == 0 || len(share.Domains) > 100 {
			http.Error(w, "each team needs a unique share with 1 to 100 domain patterns", 400)
			return
		}
		seen[share.Team] = true
		patterns := map[string]bool{}
		for _, pattern := range share.Domains {
			name := strings.TrimPrefix(pattern, "*.")
			if !validDNSName(name) || (name != domain && !strings.HasSuffix(name, "."+domain)) || patterns[pattern] {
				http.Error(w, "use unique exact hostnames or *.subdomain patterns inside this zone", 400)
				return
			}
			patterns[pattern] = true
		}
		var allowed bool
		err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM teams WHERE id=$1 AND ($2 OR cove_access($3,id,'','')>=3))", share.Team, actor(r).Admin, actor(r).ID).Scan(&allowed)
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		if !allowed {
			http.Error(w, "team-wide editing access required for each share", 403)
			return
		}
	}
	// The zone lock is also held by DNS writers; revocation waits for in-flight writes.
	_, err = tx.Exec(r.Context(), "DELETE FROM cloudflare_zone_teams WHERE zone_id=$1 AND ($2 OR cove_access($3,team_id,'','')>=3)", r.PathValue("zone"), actor(r).Admin, actor(r).ID)
	for _, share := range in.Shares {
		if err != nil {
			break
		}
		_, err = tx.Exec(r.Context(), "INSERT INTO cloudflare_zone_teams(zone_id,team_id,domains) VALUES($1,$2,$3)", r.PathValue("zone"), share.Team, share.Domains)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "UPDATE cloudflare_zones SET checked_at=NULL WHERE zone_id=$1", r.PathValue("zone"))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save domain sharing", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
