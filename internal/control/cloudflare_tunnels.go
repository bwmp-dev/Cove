package control

import (
	"context"
	"cove/internal/worker"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type tunnelDNSState struct {
	Proof     tunnelDNSProof `json:"proof"`
	Attempted bool           `json:"attempted"`
	Phase     string         `json:"phase"`
	Cleanup   bool           `json:"cleanup"`
	LastError string         `json:"last_error,omitempty"`
}

type nodeTunnelPrivate struct {
	AccountID string                     `json:"account_id,omitempty"`
	Identity  cloudflareTunnelIdentity   `json:"identity"`
	Records   map[string]*tunnelDNSState `json:"records"`
}

type nodeTunnelConfiguration struct {
	ZoneID    string   `json:"zone_id"`
	Hostnames []string `json:"hostnames"`
	Image     string   `json:"image"`
}

func (s *Server) grantedTunnelHosts(ctx context.Context, tx pgx.Tx, node, zone, owner string, hosts []string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT w.configuration,g.domains FROM workloads w
	 JOIN cloudflare_zone_teams g ON g.team_id=w.team_id AND g.zone_id=$2
	 WHERE w.node_id=$1 AND cove_automation_allowed($3,w.team_id,w.project_id,w.environment_id,'edit_configuration') FOR SHARE OF w,g`, node, zone, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	allowed := map[string]bool{}
	for rows.Next() {
		var encrypted []byte
		var patterns []string
		if err := rows.Scan(&encrypted, &patterns); err != nil {
			return nil, err
		}
		plain, err := unseal(s.Key, encrypted)
		var input workloadInput
		if err != nil || json.Unmarshal(plain, &input) != nil {
			return nil, errors.New("managed route grants unavailable")
		}
		for _, route := range input.Routes {
			if route.IsPublicHTTPRoute() && !strings.HasPrefix(route.Domain, "*.") && cloudflareDomainAllowed(patterns, route.Domain) {
				allowed[route.Domain] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := []string{}
	for _, host := range hosts {
		if allowed[host] {
			result = append(result, host)
		}
	}
	return result, nil
}

type nodeTunnelDNSStatus struct {
	Hostname  string `json:"hostname"`
	Phase     string `json:"phase"`
	Cleanup   bool   `json:"cleanup_requested"`
	LastError string `json:"last_error"`
}

type nodeTunnelStatus struct {
	Configuration *nodeTunnelConfiguration           `json:"configuration"`
	Phase         string                             `json:"phase"`
	Cleanup       bool                               `json:"cleanup_requested"`
	LastError     string                             `json:"last_error"`
	NextCheckAt   *time.Time                         `json:"next_check_at"`
	Worker        *worker.CloudflarePublishingStatus `json:"worker"`
	WorkerError   *string                            `json:"worker_error"`
	Zones         []map[string]string                `json:"zones"`
	Available     []string                           `json:"available_hostnames"`
	DNS           []nodeTunnelDNSStatus              `json:"dns"`
	DefaultImage  string                             `json:"default_image"`
}

func (s *Server) tunnelRoutes(ctx context.Context, node string) (map[string][]worker.Route, error) {
	rows, err := s.DB.Query(ctx, "SELECT id,configuration FROM workloads WHERE node_id=$1", node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	routes := map[string][]worker.Route{}
	for rows.Next() {
		var id string
		var encrypted []byte
		if err := rows.Scan(&id, &encrypted); err != nil {
			return nil, err
		}
		data, err := unseal(s.Key, encrypted)
		var input workloadInput
		if err != nil || json.Unmarshal(data, &input) != nil {
			return nil, errors.New("managed routes unavailable")
		}
		routes[id] = input.Routes
	}
	return routes, rows.Err()
}

func (s *Server) readNodeTunnelStatus(ctx context.Context, node, user string) (nodeTunnelStatus, error) {
	status := nodeTunnelStatus{Phase: "disabled", Zones: []map[string]string{}, Available: []string{}, DNS: []nodeTunnelDNSStatus{}, DefaultImage: worker.DefaultCloudflaredImage}
	var supported bool
	if err := s.DB.QueryRow(ctx, "SELECT COALESCE((capabilities->>'cloudflare_publishing')::boolean,false) FROM nodes WHERE id=$1", node).Scan(&supported); err != nil {
		return status, err
	}
	var configuration nodeTunnelConfiguration
	var encrypted []byte
	err := s.DB.QueryRow(ctx, "SELECT zone_id,hostnames,image,phase,cleanup_requested,last_error,next_check_at,state FROM node_cloudflare_tunnels WHERE node_id=$1", node).Scan(&configuration.ZoneID, &configuration.Hostnames, &configuration.Image, &status.Phase, &status.Cleanup, &status.LastError, &status.NextCheckAt, &encrypted)
	if err == nil {
		status.Configuration = &configuration
		data, err := unseal(s.Key, encrypted)
		var state nodeTunnelPrivate
		if err != nil || json.Unmarshal(data, &state) != nil {
			return status, errors.New("stored tunnel state unavailable")
		}
		for host, record := range state.Records {
			status.DNS = append(status.DNS, nodeTunnelDNSStatus{Hostname: host, Phase: record.Phase, Cleanup: record.Cleanup, LastError: record.LastError})
		}
		slices.SortFunc(status.DNS, func(a, b nodeTunnelDNSStatus) int { return strings.Compare(a.Hostname, b.Hostname) })
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return status, err
	}

	rows, err := s.DB.Query(ctx, "SELECT z.zone_id,z.domain,z.configuration FROM cloudflare_zones z WHERE z.owner_id=$1 ORDER BY z.domain", user)
	if err != nil {
		return status, err
	}
	type choice struct {
		id, domain    string
		configuration []byte
	}
	choices := []choice{}
	for rows.Next() {
		var item choice
		if err := rows.Scan(&item.id, &item.domain, &item.configuration); err != nil {
			rows.Close()
			return status, err
		}
		choices = append(choices, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return status, err
	}
	for _, item := range choices {
		data, err := unseal(s.Key, item.configuration)
		var zone cloudflareZone
		if err != nil || json.Unmarshal(data, &zone) != nil {
			return status, errors.New("Cloudflare zone configuration unavailable")
		}
		kind := ""
		if zone.ConnectionID != "" {
			if err := s.DB.QueryRow(ctx, "SELECT kind FROM cloudflare_accounts WHERE id=$1 AND owner_id=$2", zone.ConnectionID, user).Scan(&kind); err != nil {
				continue
			}
		}
		status.Zones = append(status.Zones, map[string]string{"id": item.id, "domain": item.domain, "connection_kind": kind})
	}
	routes, err := s.tunnelRoutes(ctx, node)
	if err != nil {
		return status, err
	}
	seen := map[string]bool{}
	for _, list := range routes {
		for _, route := range list {
			if route.IsTransportRoute() || strings.ContainsRune(route.Domain, '*') || seen[route.Domain] || route.Policy != nil && route.Policy.CertificateSource == "default" {
				continue
			}
			if worker.ValidatePublishedRoutes([]string{route.Domain}, routes) == nil {
				seen[route.Domain] = true
				status.Available = append(status.Available, route.Domain)
			}
		}
	}
	slices.Sort(status.Available)
	if supported {
		probe, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		response, err := s.nodeRPC(probe, node, worker.RPCRequest{Method: "cloudflare_publishing"})
		if err != nil || response.Publishing == nil {
			message := "Could not read the worker's current tunnel state."
			status.WorkerError = &message
		} else {
			status.Worker = response.Publishing
		}
	} else {
		message := "Upgrade and verify this worker before publishing through Cloudflare Tunnel."
		status.WorkerError = &message
	}
	return status, nil
}

func (s *Server) nodeCloudflareTunnel(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("id")
	if r.Method == "GET" {
		status, err := s.readNodeTunnelStatus(r.Context(), node, actor(r).ID)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "tunnel status unavailable", 503)
			return
		}
		respond(w, 200, status)
		return
	}
	if r.Method == "DELETE" || strings.HasSuffix(r.URL.Path, "/retry") {
		query := "UPDATE node_cloudflare_tunnels SET next_check_at=now(),last_error='',updated_at=now() WHERE node_id=$1"
		if r.Method == "DELETE" {
			query = "UPDATE node_cloudflare_tunnels SET cleanup_requested=true,phase='disabling',next_check_at=now(),last_error='',updated_at=now() WHERE node_id=$1"
		}
		_, err := s.DB.Exec(r.Context(), query, node)
		if err != nil {
			http.Error(w, "could not request tunnel reconciliation", 503)
			return
		}
		status, err := s.readNodeTunnelStatus(r.Context(), node, actor(r).ID)
		if err != nil {
			http.Error(w, "tunnel status unavailable", 503)
			return
		}
		respond(w, 202, status)
		return
	}
	s.outboundState.mu.Lock()
	defer s.outboundState.mu.Unlock()
	outbound, err := s.outboundConfig(r.Context())
	if err != nil {
		http.Error(w, "outbound proxy settings unavailable", 503)
		return
	}
	if outbound.HTTP != "" || outbound.HTTPS != "" {
		http.Error(w, "Cloudflare Tunnel edge transport cannot use Cove's HTTP outbound proxy; no tunnel was requested", 409)
		return
	}
	var input nodeTunnelConfiguration
	if !decode(w, r, &input) {
		return
	}
	if len(input.Hostnames) == 0 || len(input.Hostnames) > 128 || !cloudflareID.MatchString(input.ZoneID) {
		http.Error(w, "choose a zone and between one and 128 exact hostnames", 400)
		return
	}
	seen := map[string]bool{}
	for _, host := range input.Hostnames {
		if !validDNSName(host) || len(host) > 253 || strings.ContainsRune(host, '*') || seen[host] {
			http.Error(w, "choose unique exact lowercase DNS hostnames", 400)
			return
		}
		seen[host] = true
	}
	if err := worker.ValidateCloudflaredImage(input.Image); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if input.Image == "" {
		input.Image = worker.DefaultCloudflaredImage
	}
	var ready, supported bool
	if err := s.DB.QueryRow(r.Context(), "SELECT state='ready',COALESCE((capabilities->>'cloudflare_publishing')::boolean,false) FROM nodes WHERE id=$1", node).Scan(&ready, &supported); errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, "worker capabilities unavailable", 503)
		return
	}
	if !ready || !supported {
		http.Error(w, "a ready worker with Cloudflare Tunnel support is required", 409)
		return
	}
	routes, err := s.tunnelRoutes(r.Context(), node)
	if err != nil {
		http.Error(w, "managed routes unavailable", 503)
		return
	}
	if err := worker.ValidatePublishedRoutes(input.Hostnames, routes); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	for _, list := range routes {
		for _, route := range list {
			if seen[route.Domain] && route.Policy != nil && route.Policy.CertificateSource == "default" {
				http.Error(w, "select a trusted ACME or stored origin certificate before publishing", 400)
				return
			}
		}
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock($1)", routeClaimLock); err != nil {
		http.Error(w, "route reservations unavailable", 503)
		return
	}
	var raw []byte
	var zoneDomain string
	if err := tx.QueryRow(r.Context(), "SELECT domain,configuration FROM cloudflare_zones WHERE zone_id=$1 AND owner_id=$2 FOR SHARE", input.ZoneID, actor(r).ID).Scan(&zoneDomain, &raw); errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "choose a Cloudflare zone owned by your connection", 403)
		return
	} else if err != nil {
		http.Error(w, "Cloudflare zone unavailable", 503)
		return
	}
	data, err := unseal(s.Key, raw)
	var zone cloudflareZone
	if err != nil || json.Unmarshal(data, &zone) != nil || zone.ConnectionID == "" || !zone.Enabled {
		http.Error(w, "an enabled zone with an API-token connection is required", 400)
		return
	}
	var kind string
	if err := tx.QueryRow(r.Context(), "SELECT kind FROM cloudflare_accounts WHERE id=$1 AND owner_id=$2 FOR SHARE", zone.ConnectionID, actor(r).ID).Scan(&kind); err != nil || kind != "token" {
		http.Error(w, "use an owned API-token connection with Account Cloudflare Tunnel Edit and Zone DNS Edit/Read; OAuth grants DNS access only", 400)
		return
	}
	for _, host := range input.Hostnames {
		if host != zoneDomain && !strings.HasSuffix(host, "."+zoneDomain) {
			http.Error(w, "every hostname must belong to the selected zone", 400)
			return
		}
	}
	granted, err := s.grantedTunnelHosts(r.Context(), tx, node, input.ZoneID, actor(r).ID, input.Hostnames)
	if err != nil {
		http.Error(w, "route grants unavailable", 503)
		return
	}
	if len(granted) != len(input.Hostnames) {
		http.Error(w, "share the selected zone and every hostname with each resource's team before publishing", 403)
		return
	}
	var existingZone string
	var cleaning bool
	err = tx.QueryRow(r.Context(), "SELECT zone_id,cleanup_requested FROM node_cloudflare_tunnels WHERE node_id=$1 FOR UPDATE", node).Scan(&existingZone, &cleaning)
	if err == nil {
		if cleaning || existingZone != input.ZoneID {
			http.Error(w, "finish disabling and cleaning up the current tunnel before changing its zone", 409)
			return
		}
		_, err = tx.Exec(r.Context(), "UPDATE node_cloudflare_tunnels SET hostnames=$2,image=$3,phase='pending',last_error='',next_check_at=now(),updated_at=now() WHERE node_id=$1", node, input.Hostnames, input.Image)
	} else if errors.Is(err, pgx.ErrNoRows) {
		id := resourceID()
		secret := make([]byte, 32)
		if _, err = rand.Read(secret); err != nil {
			http.Error(w, "could not create the tunnel credential", 503)
			return
		}
		private := nodeTunnelPrivate{Identity: cloudflareTunnelIdentity{Name: "cove-" + id, Secret: base64.StdEncoding.EncodeToString(secret)}, Records: map[string]*tunnelDNSState{}}
		raw, err = s.sealJSON(private)
		if err == nil {
			_, err = tx.Exec(r.Context(), "INSERT INTO node_cloudflare_tunnels(node_id,id,zone_id,connection_id,owner_id,hostnames,image,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", node, id, input.ZoneID, zone.ConnectionID, actor(r).ID, input.Hostnames, input.Image, raw)
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save the tunnel request", 503)
		return
	}
	status, err := s.readNodeTunnelStatus(r.Context(), node, actor(r).ID)
	if err != nil {
		http.Error(w, "request saved, but tunnel status is unavailable", 503)
		return
	}
	respond(w, 202, status)
}
