package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type cloudflareZone struct {
	ConnectionID  string            `json:"connection_id,omitempty"`
	PreserveProxy bool              `json:"preserve_existing_proxy,omitempty"`
	ZoneID        string            `json:"zone_id"`
	Domain        string            `json:"domain"`
	Token         string            `json:"token,omitempty"`
	Enabled       bool              `json:"enabled"`
	Proxied       bool              `json:"proxied"`
	Nodes         map[string]string `json:"nodes"`
}

var cloudflareID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func validateCloudflare(c cloudflareZone) error {
	if !cloudflareID.MatchString(c.ZoneID) || !validDNSName(c.Domain) {
		return errors.New("valid Cloudflare zone ID and lowercase domain required")
	}
	if c.ConnectionID == "" && (len(c.Token) < 20 || len(c.Token) > 4096 || strings.ContainsAny(c.Token, "\r\n")) {
		return errors.New("Cloudflare API token required")
	}
	if len(c.Nodes) > 2048 {
		return errors.New("too many node addresses")
	}
	for node, address := range c.Nodes {
		ip, err := netip.ParseAddr(address)
		if node == "" || err != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Is4In6() {
			return errors.New("node addresses must be public IPv4 or IPv6 addresses")
		}
	}
	return nil
}

type cloudflareStatusError int

func (e cloudflareStatusError) Error() string {
	return fmt.Sprintf("Cloudflare request failed (HTTP %d)", int(e))
}

func (s *Server) cloudflareRequest(ctx context.Context, c cloudflareZone, method, path string, body any, result any) error {
	if c.ConnectionID != "" {
		token, err := s.cloudflareConnectionToken(ctx, c.ConnectionID)
		if err != nil {
			return err
		}
		c.Token = token
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.cloudflare.com/client/v4/zones/"+c.ZoneID+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 20 * time.Second, Transport: s.CloudflareTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("Cloudflare could not be reached")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return cloudflareStatusError(response.StatusCode)
	}
	var envelope struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&envelope) != nil || !envelope.Success {
		return errors.New("Cloudflare returned an invalid or unsuccessful response")
	}
	if result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}

type cloudflareRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

func (s *Server) reconcileDNSRecord(ctx context.Context, c cloudflareZone, domain, address string) error {
	if domain != c.Domain && !strings.HasSuffix(domain, "."+c.Domain) {
		return errors.New("hostname is outside configured zone")
	}
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return errors.New("invalid node address")
	}
	kind := "A"
	if ip.Is6() {
		kind = "AAAA"
	}
	var records []cloudflareRecord
	if err = s.cloudflareRequest(ctx, c, "GET", "/dns_records?name="+url.QueryEscape(domain)+"&per_page=100", nil, &records); err != nil {
		return err
	}
	// Never silently remove a CNAME, a second address, or the other IP family.
	var matching *cloudflareRecord
	for i := range records {
		v := &records[i]
		if v.Name != domain {
			return errors.New("Cloudflare returned an unexpected hostname")
		}
		if v.Type == "CNAME" || v.Type == "A" || v.Type == "AAAA" {
			if v.Type != kind || matching != nil {
				return errors.New("conflicting DNS records require review")
			}
			matching = v
		}
	}
	body := map[string]any{"type": kind, "name": domain, "content": ip.String(), "ttl": 1, "proxied": c.Proxied}
	if matching == nil {
		return s.cloudflareRequest(ctx, c, "POST", "/dns_records", body, nil)
	}
	proxied := c.Proxied
	if c.PreserveProxy {
		proxied = matching.Proxied
		body["proxied"] = proxied
	}
	if matching.Content == ip.String() && matching.Proxied == proxied {
		return nil
	}
	if !cloudflareID.MatchString(matching.ID) {
		return errors.New("invalid Cloudflare record ID")
	}
	return s.cloudflareRequest(ctx, c, "PATCH", "/dns_records/"+matching.ID, body, nil)
}

// removeDNSRecord deletes the address record Cove created for a hostname. Records of other types,
// or pointing elsewhere, are left alone because Cove did not create them.
func (s *Server) removeDNSRecord(ctx context.Context, c cloudflareZone, domain, address string) error {
	if domain != c.Domain && !strings.HasSuffix(domain, "."+c.Domain) {
		return errors.New("hostname is outside configured zone")
	}
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return errors.New("invalid node address")
	}
	var records []cloudflareRecord
	if err = s.cloudflareRequest(ctx, c, "GET", "/dns_records?name="+url.QueryEscape(domain)+"&per_page=100", nil, &records); err != nil {
		return err
	}
	for _, record := range records {
		if record.Name != domain || (record.Type != "A" && record.Type != "AAAA") || record.Content != ip.String() {
			continue
		}
		if !cloudflareID.MatchString(record.ID) {
			return errors.New("invalid Cloudflare record ID")
		}
		if err = s.cloudflareRequest(ctx, c, "DELETE", "/dns_records/"+record.ID, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) reconcileDNS(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, "SELECT z.zone_id,t.team_id,z.configuration FROM (SELECT * FROM cloudflare_zones z WHERE (checked_at IS NULL OR checked_at<now()-interval '1 minute') AND EXISTS(SELECT 1 FROM cloudflare_zone_teams t WHERE t.zone_id=z.zone_id) ORDER BY checked_at NULLS FIRST,zone_id LIMIT 8) z JOIN cloudflare_zone_teams t ON t.zone_id=z.zone_id")
	if err != nil {
		return err
	}
	type entry struct {
		id, team string
		raw      []byte
	}
	entries := []entry{}
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.id, &e.team, &e.raw); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	messages := map[string]string{}
	for _, e := range entries {
		data, err := unseal(s.Key, e.raw)
		var c cloudflareZone
		if err != nil || json.Unmarshal(data, &c) != nil {
			return errors.New("DNS configuration unavailable")
		}
		message := ""
		if c.Enabled {
			resources, err := s.DB.Query(ctx, `SELECT w.id,w.node_id,w.configuration FROM workloads w WHERE w.team_id=$1 AND EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.action IN ('deploy','routes','rollback') AND o.state='succeeded') AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running'))`, e.team)
			if err != nil {
				return err
			}
			type target struct {
				id, node string
				raw      []byte
			}
			targets := []target{}
			for resources.Next() {
				var t target
				if err = resources.Scan(&t.id, &t.node, &t.raw); err != nil {
					resources.Close()
					return err
				}
				targets = append(targets, t)
			}
			err = resources.Err()
			resources.Close()
			if err != nil {
				return err
			}
			for _, t := range targets {
				address := c.Nodes[t.node]
				if address == "" {
					continue
				}
				if err = s.reconcileResourceDNS(ctx, e.id, e.team, e.raw, t.id); err != nil {
					message = err.Error()
				}
				if message != "" {
					break
				}
			}
		}
		if message != "" {
			messages[e.id] = message
		}
		message = messages[e.id]
		if _, err = s.DB.Exec(ctx, "UPDATE cloudflare_zones SET checked_at=now(),last_error=$2 WHERE zone_id=$1 AND configuration=$3", e.id, message, e.raw); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) reconcileResourceDNS(ctx context.Context, zone, team string, expected []byte, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	var node string
	err = tx.QueryRow(ctx, `SELECT node_id,configuration FROM workloads WHERE id=$1 AND team_id=$2 FOR SHARE`, id, team).Scan(&node, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Keep route ownership stable until the external write completes.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", routeClaimLock); err != nil {
		return err
	}
	var active bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))", id).Scan(&active); err != nil {
		return err
	}
	if active {
		return nil
	}
	var zoneRaw []byte
	err = tx.QueryRow(ctx, "SELECT configuration FROM cloudflare_zones WHERE zone_id=$1 FOR SHARE", zone).Scan(&zoneRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var domains []string
	err = tx.QueryRow(ctx, "SELECT domains FROM cloudflare_zone_teams WHERE zone_id=$1 AND team_id=$2 FOR SHARE", zone, team).Scan(&domains)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(zoneRaw, expected) {
		return nil
	}
	data, err := unseal(s.Key, zoneRaw)
	var c cloudflareZone
	if err != nil || json.Unmarshal(data, &c) != nil {
		return errors.New("DNS configuration unavailable")
	}
	if !c.Enabled || c.Nodes[node] == "" {
		return nil
	}
	data, err = unseal(s.Key, raw)
	var config workloadInput
	if err != nil || json.Unmarshal(data, &config) != nil {
		return errors.New("resource configuration unavailable")
	}
	for _, route := range config.Routes {
		if !route.IsTransportRoute() && (route.Policy == nil || route.Policy.Visibility == "" || route.Policy.Visibility == "public") && cloudflareDomainAllowed(domains, route.Domain) {
			var reserved bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM node_cloudflare_tunnels WHERE node_id=$1 AND $2=ANY(hostnames))", node, route.Domain).Scan(&reserved); err != nil {
				return err
			}
			if reserved {
				continue
			}
			if err = s.reconcileDNSRecord(ctx, c, route.Domain, c.Nodes[node]); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func (s *Server) runDNS(ctx context.Context) error {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		if err := s.reconcileDNS(ctx); err != nil && ctx.Err() == nil {
			slog.Error("DNS reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
	}
}
