package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type cloudflareCredentials struct {
	Token    string    `json:"access_token"`
	Refresh  string    `json:"refresh_token,omitempty"`
	Expires  time.Time `json:"expires_at,omitempty"`
	ClientID string    `json:"client_id,omitempty"`
}

type discoveredCloudflareZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func validCloudflareToken(token string) bool {
	return len(token) >= 20 && len(token) <= 16384 && !strings.ContainsAny(token, "\r\n\t ")
}

func (s *Server) discoverCloudflareZones(ctx context.Context, token string) ([]discoveredCloudflareZone, error) {
	zones := []discoveredCloudflareZone{}
	seen := map[string]bool{}
	client := &http.Client{Timeout: 20 * time.Second, Transport: s.CloudflareTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for page := 1; page <= 100; page++ {
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("https://api.cloudflare.com/client/v4/zones?per_page=50&page=%d", page), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(req)
		if err != nil {
			return nil, errors.New("Cloudflare could not be reached")
		}
		var envelope struct {
			Success bool                       `json:"success"`
			Result  []discoveredCloudflareZone `json:"result"`
			Info    struct {
				TotalPages int `json:"total_pages"`
			} `json:"result_info"`
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&envelope)
		response.Body.Close()
		if response.StatusCode != 200 || err != nil || !envelope.Success {
			return nil, errors.New("could not list Cloudflare zones; check Zone Read permission and authorized zones")
		}
		for _, z := range envelope.Result {
			if !cloudflareID.MatchString(z.ID) || !validDNSName(z.Name) || seen[z.ID] {
				return nil, errors.New("Cloudflare returned an invalid zone list")
			}
			seen[z.ID] = true
			zones = append(zones, z)
		}
		if envelope.Info.TotalPages > 0 && page >= envelope.Info.TotalPages || envelope.Info.TotalPages == 0 && len(envelope.Result) < 50 {
			return zones, nil
		}
	}
	return nil, errors.New("too many Cloudflare zones; restrict the token to the zones Cove needs")
}

func (s *Server) cloudflareAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), "SELECT id,name,kind FROM cloudflare_accounts WHERE owner_id=$1 ORDER BY name,id", actor(r).ID)
		if err != nil {
			http.Error(w, "connections unavailable", 503)
			return
		}
		defer rows.Close()
		out := []map[string]string{}
		for rows.Next() {
			var id, name, kind string
			if err = rows.Scan(&id, &name, &kind); err != nil {
				break
			}
			out = append(out, map[string]string{"id": id, "name": name, "kind": kind})
		}
		if err != nil || rows.Err() != nil {
			http.Error(w, "connections unavailable", 503)
			return
		}
		respond(w, 200, out)
		return
	}
	var in struct {
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 || !validCloudflareToken(in.Token) {
		http.Error(w, "a connection name and valid API token are required", 400)
		return
	}
	zones, err := s.discoverCloudflareZones(r.Context(), in.Token)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	raw, err := json.Marshal(cloudflareCredentials{Token: in.Token})
	if err == nil {
		raw, err = seal(s.Key, raw)
	}
	id := r.PathValue("connection")
	if err == nil && r.Method == "PUT" {
		tx, e := s.DB.Begin(r.Context())
		if e != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		_, err = tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(718904526)")
		var owned string
		if err == nil {
			err = tx.QueryRow(r.Context(), "SELECT id FROM cloudflare_accounts WHERE id=$1 AND owner_id=$2 AND kind='token' FOR UPDATE", id, actor(r).ID).Scan(&owned)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "connection unavailable", 503)
			return
		}
		available := map[string]bool{}
		for _, zone := range zones {
			available[zone.ID] = true
		}
		rows, e := tx.Query(r.Context(), "SELECT configuration FROM cloudflare_zones WHERE owner_id=$1", actor(r).ID)
		if e != nil {
			http.Error(w, "domains unavailable", 503)
			return
		}
		for rows.Next() {
			var encrypted []byte
			var c cloudflareZone
			if err = rows.Scan(&encrypted); err != nil {
				break
			}
			encrypted, err = unseal(s.Key, encrypted)
			if err == nil {
				err = json.Unmarshal(encrypted, &c)
			}
			if err != nil {
				break
			}
			if c.ConnectionID == id && !available[c.ZoneID] {
				rows.Close()
				http.Error(w, "replacement token must retain access to all connected domains", 409)
				return
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err == nil {
			_, err = tx.Exec(r.Context(), "UPDATE cloudflare_accounts SET name=$2,credentials=$3 WHERE id=$1", id, in.Name, raw)
		}
		if err == nil {
			err = tx.Commit(r.Context())
		}
	} else if err == nil {
		id = randomToken()
		_, err = s.DB.Exec(r.Context(), "INSERT INTO cloudflare_accounts(id,owner_id,name,kind,credentials) VALUES($1,$2,$3,'token',$4)", id, actor(r).ID, in.Name, raw)
	}
	if err != nil {
		http.Error(w, "could not save connection", 503)
		return
	}
	respond(w, 200, map[string]any{"id": id, "zones": zones})
}

func (s *Server) cloudflareAccountZones(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("connection")
	var owned bool
	if err := s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM cloudflare_accounts WHERE id=$1 AND owner_id=$2)", id, actor(r).ID).Scan(&owned); err != nil {
		http.Error(w, "connections unavailable", 503)
		return
	}
	if !owned {
		http.NotFound(w, r)
		return
	}
	token, err := s.cloudflareConnectionToken(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	zones, err := s.discoverCloudflareZones(r.Context(), token)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	if r.Method == "GET" {
		respond(w, 200, zones)
		return
	}
	var in struct {
		Zones []string `json:"zones"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Zones) == 0 || len(in.Zones) > 100 {
		http.Error(w, "select between 1 and 100 domains", 400)
		return
	}
	selected := map[string]string{}
	available := map[string]string{}
	for _, z := range zones {
		available[z.ID] = z.Name
	}
	for _, z := range in.Zones {
		if available[z] == "" || selected[z] != "" {
			http.Error(w, "select unique domains available to this connection", 400)
			return
		}
		selected[z] = available[z]
	}
	addresses, err := s.cloudflareDefaultAddresses(r.Context(), actor(r).ID, actor(r).Admin)
	if err != nil {
		http.Error(w, "worker addresses unavailable", 503)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	// Serialize competing imports so a batch cannot partially adopt another owner's zones.
	_, err = tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(718904526)")
	for zone, domain := range selected {
		if err != nil {
			break
		}
		var raw []byte
		var owner string
		e := tx.QueryRow(r.Context(), "SELECT configuration,COALESCE(owner_id,'') FROM cloudflare_zones WHERE zone_id=$1 FOR UPDATE", zone).Scan(&raw, &owner)
		c := cloudflareZone{ZoneID: zone, Domain: domain, Enabled: true, PreserveProxy: true, Nodes: addresses}
		exists := e == nil
		if exists {
			if owner != actor(r).ID {
				http.Error(w, "one or more domains are already connected by another user", 409)
				return
			}
			raw, err = unseal(s.Key, raw)
			if err != nil {
				break
			}
			if err = json.Unmarshal(raw, &c); err != nil {
				break
			}
			if c.Domain != domain {
				http.Error(w, "the domain no longer matches the connected zone", 409)
				return
			}
		} else if !errors.Is(e, pgx.ErrNoRows) {
			err = e
			break
		}
		c.ConnectionID = id
		c.Token = ""
		raw, err = json.Marshal(c)
		if err == nil {
			raw, err = seal(s.Key, raw)
		}
		if err != nil {
			break
		}
		if exists {
			_, err = tx.Exec(r.Context(), "UPDATE cloudflare_zones SET configuration=$2,checked_at=NULL WHERE zone_id=$1", zone, raw)
		} else {
			tag, e := tx.Exec(r.Context(), "INSERT INTO cloudflare_zones(zone_id,domain,owner_id,configuration) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", zone, domain, actor(r).ID, raw)
			err = e
			if err == nil && tag.RowsAffected() != 1 {
				http.Error(w, "one or more domains are already connected", 409)
				return
			}
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not connect domains", 503)
		return
	}
	respond(w, 200, map[string]int{"connected": len(selected)})
}

func (s *Server) cloudflareDefaultAddresses(ctx context.Context, user string, admin bool) (map[string]string, error) {
	rows, err := s.DB.Query(ctx, `SELECT n.id,n.address FROM nodes n WHERE $1 OR EXISTS(SELECT 1 FROM team_nodes t WHERE t.node_id=n.id AND cove_access($2,t.team_id,'','')>=3)`, admin, user)
	if err != nil {
		return nil, err
	}
	type node struct{ id, address string }
	nodes := []node{}
	for rows.Next() {
		var n node
		if err = rows.Scan(&n.id, &n.address); err != nil {
			break
		}
		nodes = append(nodes, n)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	addresses := map[string]string{}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, n := range nodes {
		ips := []netip.Addr{}
		if ip, e := netip.ParseAddr(n.address); e == nil {
			ips = append(ips, ip)
		} else {
			resolved, e := net.DefaultResolver.LookupNetIP(ctx, "ip", n.address)
			if e != nil {
				continue
			}
			ips = resolved
		}
		if address := cloudflarePublicAddress(ips); address != "" {
			addresses[n.id] = address
		}
	}
	return addresses, nil
}

func cloudflarePublicAddress(ips []netip.Addr) string {
	var v4, v6 []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, ip := range ips {
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || seen[ip] {
			continue
		}
		seen[ip] = true
		if ip.Is4() {
			v4 = append(v4, ip)
		} else {
			v6 = append(v6, ip)
		}
	}
	if len(v4) == 1 {
		return v4[0].String()
	}
	if len(v4) == 0 && len(v6) == 1 {
		return v6[0].String()
	}
	return ""
}

func (s *Server) cloudflareConnectionToken(ctx context.Context, id string) (string, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", errors.New("Cloudflare connection unavailable")
	}
	defer tx.Rollback(ctx)
	var raw []byte
	// Refresh tokens rotate. Lock across exchange and persistence so concurrent zones reuse the result.
	err = tx.QueryRow(ctx, "SELECT credentials FROM cloudflare_accounts WHERE id=$1 FOR UPDATE", id).Scan(&raw)
	if err == nil {
		raw, err = unseal(s.Key, raw)
	}
	var c cloudflareCredentials
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return "", errors.New("Cloudflare connection unavailable")
	}
	if !c.Expires.IsZero() && time.Until(c.Expires) < 2*time.Minute {
		client, e := s.cloudflareOAuthClient(ctx, tx)
		if e != nil || c.ClientID != client.ClientID || c.Refresh == "" {
			return "", errors.New("reconnect this Cloudflare account")
		}
		next, e := s.exchangeCloudflareToken(ctx, client, map[string][]string{"grant_type": {"refresh_token"}, "refresh_token": {c.Refresh}})
		if e != nil {
			return "", errors.New("Cloudflare authorization expired; reconnect the account")
		}
		if next.Refresh == "" {
			next.Refresh = c.Refresh
		}
		c = next
		raw, err = json.Marshal(c)
		if err == nil {
			raw, err = seal(s.Key, raw)
		}
		if err == nil {
			_, err = tx.Exec(ctx, "UPDATE cloudflare_accounts SET credentials=$2 WHERE id=$1", id, raw)
		}
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return "", errors.New("could not save Cloudflare authorization")
	}
	if !validCloudflareToken(c.Token) {
		return "", errors.New("Cloudflare authorization is invalid")
	}
	return c.Token, nil
}
