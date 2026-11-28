package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const routeDNSWait = 2 * time.Minute

type routeDNSHold struct {
	Started  time.Time `json:"started"`
	Next     time.Time `json:"next"`
	Attempts int       `json:"attempts"`
	Hostname string    `json:"hostname"`
}

func exposesRequestRoutes(request worker.Request) bool {
	switch request.Action {
	case "deploy", "routes", "rollback", "adopt", "resume", "pitr_restore", "pitr_restore_copy":
		return slices.ContainsFunc(request.Routes, worker.Route.IsPublicHTTPRoute)
	}
	return false
}

// Preparation is checkpointed separately from the worker report: an ambiguous
// submit reply must never put an already executing operation back behind DNS.
func (s *Server) prepareRouteDNS(ctx context.Context, node string, request worker.Request, now time.Time) (bool, error) {
	if !exposesRequestRoutes(request) {
		return true, nil
	}
	var prepared bool
	var previous []byte
	var state string
	if err := s.DB.QueryRow(ctx, `SELECT dns_prepared,result,state FROM operations WHERE id=$1`, request.ID).Scan(&prepared, &previous, &state); err != nil {
		return false, err
	}
	if prepared || state != "queued" {
		return true, nil
	}
	var saved struct {
		DNS routeDNSHold `json:"dns_publication"`
	}
	if len(previous) > 0 && json.Unmarshal(previous, &saved) != nil {
		return false, errors.New("DNS publication checkpoint is unreadable")
	}
	hold := saved.DNS
	if hold.Started.IsZero() {
		hold.Started = now
	}
	if now.Before(hold.Next) {
		return false, nil
	}
	if !now.Before(hold.Started.Add(routeDNSWait)) {
		warning := fmt.Sprintf("DNS publication for %s could not be confirmed within two minutes. Deployment continued; Cove will keep reconciling DNS and retrying certificate issuance. Check the Cloudflare connection and hostname configuration.", hold.Hostname)
		_, err := s.DB.Exec(ctx, `UPDATE operations SET dns_prepared=true,dns_warning=$2,result=jsonb_build_object('dns_warning',$2::text) WHERE id=$1`, request.ID, warning)
		return err == nil, err
	}
	remaining := hold.Started.Add(routeDNSWait).Sub(now)
	if remaining > 10*time.Second {
		remaining = 10 * time.Second
	}
	probe, cancel := context.WithTimeout(ctx, remaining)
	host, publicationErr := s.publishRequestDNS(probe, node, request)
	cancel()
	if publicationErr == nil {
		_, err := s.DB.Exec(ctx, `UPDATE operations SET dns_prepared=true,result=NULL WHERE id=$1`, request.ID)
		return err == nil, err
	}
	hold.Hostname = host
	hold.Attempts++
	delay := 5 * time.Second
	for i := 1; i < hold.Attempts && delay < 30*time.Second; i++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	hold.Next = now.Add(delay)
	if deadline := hold.Started.Add(routeDNSWait); hold.Next.After(deadline) {
		hold.Next = deadline
	}
	message := fmt.Sprintf("Waiting for Cloudflare to publish DNS for %s before exposing its route (up to two minutes).", host)
	result, err := json.Marshal(map[string]any{"error": message, "hold_reason": message, "dns_publication": hold})
	if err != nil {
		return false, err
	}
	_, err = s.DB.Exec(ctx, `UPDATE operations SET result=$2,updated_at=now() WHERE id=$1`, request.ID, result)
	return false, err
}

func (s *Server) publishRequestDNS(ctx context.Context, node string, request worker.Request) (string, error) {
	public := []string{}
	seenPublic := map[string]bool{}
	for _, route := range request.Routes {
		if route.IsPublicHTTPRoute() && !seenPublic[route.Domain] {
			public = append(public, route.Domain)
			seenPublic[route.Domain] = true
		}
	}
	hostnames := strings.Join(public, ", ")
	if hostnames == "" {
		return "", nil
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return hostnames, err
	}
	defer tx.Rollback(ctx)
	var team string
	if err = tx.QueryRow(ctx, `SELECT team_id FROM workloads WHERE id=$1 FOR SHARE`, request.Workload).Scan(&team); err != nil {
		return hostnames, err
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", routeClaimLock); err != nil {
		return hostnames, err
	}
	rows, err := tx.Query(ctx, `SELECT z.configuration,t.domains FROM cloudflare_zones z JOIN cloudflare_zone_teams t ON t.zone_id=z.zone_id WHERE t.team_id=$1 AND cove_account_active(z.owner_id) ORDER BY length(z.domain) DESC,z.zone_id FOR SHARE OF z,t`, team)
	if err != nil {
		return hostnames, err
	}
	type zoneShare struct {
		zone    cloudflareZone
		domains []string
	}
	var zones []zoneShare
	for rows.Next() {
		var raw []byte
		var v zoneShare
		if err = rows.Scan(&raw, &v.domains); err == nil {
			err = s.unsealJSON(raw, &v.zone)
		}
		if err != nil {
			rows.Close()
			return hostnames, err
		}
		zones = append(zones, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return hostnames, err
	}
	seen := map[string]bool{}
	for _, route := range request.Routes {
		if !route.IsPublicHTTPRoute() || seen[route.Domain] {
			continue
		}
		seen[route.Domain] = true
		for _, v := range zones {
			c := v.zone
			if !c.Enabled || c.Nodes[node] == "" || !cloudflareDomainAllowed(v.domains, route.Domain) || route.Domain != c.Domain && !strings.HasSuffix(route.Domain, "."+c.Domain) {
				continue
			}
			var tunnel bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_cloudflare_tunnels WHERE node_id=$1 AND $2=ANY(hostnames))`, node, route.Domain).Scan(&tunnel); err != nil {
				return route.Domain, err
			}
			if !tunnel {
				if err = s.reconcileDNSRecord(ctx, c, route.Domain, c.Nodes[node]); err != nil {
					return route.Domain, err
				}
			}
			break
		}
	}
	return "", tx.Commit(ctx)
}

func (s *Server) routeDNSWarning(ctx context.Context, id string, result []byte) ([]byte, error) {
	var warning string
	if err := s.DB.QueryRow(ctx, `SELECT dns_warning FROM operations WHERE id=$1`, id).Scan(&warning); err != nil {
		return nil, err
	}
	if warning == "" {
		return result, nil
	}
	var report map[string]json.RawMessage
	if err := json.Unmarshal(result, &report); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(warning)
	if err != nil {
		return nil, err
	}
	report["dns_warning"] = encoded
	return json.Marshal(report)
}
