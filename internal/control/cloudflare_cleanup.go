package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) dnsRouteClaims(ctx context.Context, tx pgx.Tx) (map[string]bool, error) {
	claims := map[string]bool{}
	if panel, err := url.Parse(s.PublicURL); err == nil && panel.Hostname() != "" {
		claims[strings.ToLower(panel.Hostname())] = true
	}
	add := func(routes []worker.Route) {
		for _, route := range routes {
			if route.IsPublicHTTPRoute() {
				claims[route.Domain] = true
			}
		}
	}
	rows, err := tx.Query(ctx, `SELECT w.configuration FROM workloads w
	 WHERE NOT EXISTS(SELECT 1 FROM managed_registries m WHERE m.id=w.id AND m.state='removed')`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw []byte
		var config workloadInput
		if err = rows.Scan(&raw); err == nil {
			err = s.unsealJSON(raw, &config)
		}
		if err != nil {
			rows.Close()
			return nil, errors.New("Saved route inventory is unreadable; DNS cleanup was withheld.")
		}
		add(config.Routes)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT payload FROM operations WHERE state IN ('queued','running')
	 AND action IN ('deploy','routes','rollback','adopt','resume','pitr_restore','pitr_restore_copy','move')
	 UNION ALL
	 SELECT payload FROM (SELECT DISTINCT ON (o.workload_id) o.payload,o.workload_id FROM operations o
	 JOIN workloads w ON w.id=o.workload_id WHERE o.state='succeeded'
	 AND o.action IN ('deploy','routes','rollback','adopt','resume','pitr_restore','pitr_restore_copy','move')
	 AND NOT EXISTS(SELECT 1 FROM managed_registries m WHERE m.id=w.id AND m.state='removed')
	 ORDER BY o.workload_id,o.updated_at DESC,o.id DESC) deployed`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw []byte
		var request worker.Request
		if err = rows.Scan(&raw); err == nil {
			err = s.unsealJSON(raw, &request)
		}
		if err != nil {
			rows.Close()
			return nil, errors.New("Pending or deployed route inventory is unreadable; DNS cleanup was withheld.")
		}
		if exposesRequestRoutes(request) {
			add(request.Routes)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT routes FROM environment_preview_resources`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw []byte
		var routes []worker.Route
		if err = rows.Scan(&raw); err == nil {
			err = json.Unmarshal(raw, &routes)
		}
		if err != nil {
			rows.Close()
			return nil, errors.New("Preview route inventory is unreadable; DNS cleanup was withheld.")
		}
		add(routes)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT hostnames FROM node_cloudflare_tunnels UNION ALL SELECT domains FROM route_certificate_issuers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var names []string
		if err = rows.Scan(&names); err != nil {
			return nil, err
		}
		for _, name := range names {
			claims[name] = true
		}
	}
	return claims, rows.Err()
}

func (s *Server) reconcileDNSCleanup(ctx context.Context, zone string, expected []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Claim writers take this same lock. Read their rows without row locks: a
	// publisher may already hold a workload row while waiting for the claim lock.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", routeClaimLock); err != nil {
		return err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT configuration FROM cloudflare_zones WHERE zone_id=$1 AND cove_account_active(owner_id) FOR SHARE`, zone).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, expected) {
		return nil
	}
	var config cloudflareZone
	if s.unsealJSON(raw, &config) != nil {
		return errors.New("DNS configuration unavailable")
	}
	if !config.Enabled {
		return nil
	}
	var records []cloudflareRecord
	for page := 1; ; page++ {
		if page > 64 {
			return errors.New("Too many marked DNS records to verify ownership; cleanup was withheld.")
		}
		var batch []cloudflareRecord
		query := url.Values{"comment.contains": {dnsOwnershipPrefix}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err = s.cloudflareRequest(ctx, config, "GET", "/dns_records?"+query.Encode(), nil, &batch); err != nil {
			return err
		}
		if len(batch) > 100 {
			return errors.New("DNS record inventory exceeded its bound; cleanup was withheld.")
		}
		for _, record := range batch {
			if s.ownsDNSRecord(config, record) {
				records = append(records, record)
			}
		}
		if len(batch) < 100 {
			break
		}
	}
	if len(records) == 0 {
		return tx.Commit(ctx)
	}
	claims, err := s.dnsRouteClaims(ctx, tx)
	if err != nil {
		return err
	}
	removed := 0
	for _, record := range records {
		if !s.ownsDNSRecord(config, record) {
			continue
		}
		claimed := false
		for name := range claims {
			if worker.RouteDomainsOverlap(name, record.Name) {
				claimed = true
				break
			}
		}
		if claimed {
			continue
		}
		if err = s.removeOwnedDNSRecord(ctx, config, record); err != nil {
			return errors.New("Could not remove owned DNS for " + record.Name + "; cleanup will retry.")
		}
		removed++
		if removed == 32 {
			break
		}
	}
	return tx.Commit(ctx)
}
