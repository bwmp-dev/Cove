package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) scheduleCloudflareTunnels(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `UPDATE node_cloudflare_tunnels SET next_check_at=now()+interval '15 seconds' WHERE node_id IN (SELECT node_id FROM node_cloudflare_tunnels WHERE next_check_at<=now() ORDER BY next_check_at,node_id LIMIT 2 FOR UPDATE SKIP LOCKED) RETURNING node_id`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.processCloudflareTunnel(ctx, id, false); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) processCloudflareTunnel(ctx context.Context, node string, freshCreate bool) error {
	ctx, cancel := context.WithTimeout(ctx, 80*time.Second)
	defer cancel()
	proxy, err := s.outboundConfig(ctx)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var id, zoneID, connectionID, image, owner string
	var hosts []string
	var encrypted []byte
	var cleanup bool
	err = tx.QueryRow(ctx, `SELECT id,zone_id,connection_id,hostnames,image,state,cleanup_requested,owner_id FROM node_cloudflare_tunnels WHERE node_id=$1 FOR UPDATE`, node).Scan(&id, &zoneID, &connectionID, &hosts, &image, &encrypted, &cleanup, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	data, err := unseal(s.Key, encrypted)
	var state nodeTunnelPrivate
	if err != nil || json.Unmarshal(data, &state) != nil {
		return errors.New("stored tunnel ownership state unavailable")
	}
	if state.Records == nil {
		state.Records = map[string]*tunnelDNSState{}
	}
	save := func(phase, message string) error {
		raw, err := s.sealJSON(state)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE node_cloudflare_tunnels SET state=$2,phase=$3,last_error=$4,next_check_at=now()+interval '15 seconds',updated_at=now() WHERE node_id=$1`, node, raw, phase, message); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if cleanup && !state.Identity.Attempted && state.Identity.ID == "" && len(state.Records) == 0 {
		response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "cloudflare_publishing_disable"})
		if err != nil || response.Publishing == nil || response.Publishing.Phase != "disabled" {
			return save("disabling", "The worker has not confirmed the connector is stopped; cleanup waits.")
		}
		if _, err := tx.Exec(ctx, "DELETE FROM node_cloudflare_tunnels WHERE node_id=$1", node); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if !cleanup && (proxy.HTTP != "" || proxy.HTTPS != "") {
		response, stopErr := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "cloudflare_publishing_disable"})
		message := "Tunnel publishing is suspended because its native edge transport cannot use the configured HTTP outbound proxy. Saved ownership is retained."
		if stopErr != nil || response.Publishing == nil || response.Publishing.Phase != "disabled" || response.Publishing.Running {
			message += " Worker shutdown is unconfirmed."
		}
		return save("blocked", message)
	}
	var zoneRaw, credentials []byte
	var kind string
	var authorized bool
	err = tx.QueryRow(ctx, `SELECT z.configuration,a.credentials,a.kind,
      cove_account_active(z.owner_id) AND cove_account_active(a.owner_id) AND
      EXISTS(SELECT 1 FROM node_cloudflare_tunnels t JOIN users u ON u.id=t.owner_id
      WHERE t.node_id=$3 AND u.is_admin AND cove_account_active(u.id))
      FROM cloudflare_zones z JOIN cloudflare_accounts a ON a.id=$2 WHERE z.zone_id=$1
      FOR SHARE OF z,a`, zoneID, connectionID, node).Scan(&zoneRaw, &credentials, &kind, &authorized)
	if err != nil {
		return save("blocked", "The saved Cloudflare zone or connection is unavailable; retain it until cleanup finishes.")
	}
	if !authorized {
		response, stopErr := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "cloudflare_publishing_disable"})
		message := "Tunnel publishing is suspended because its administrator or Cloudflare connection owner is no longer authorized. Saved provider ownership is retained; no provider request is sent."
		if stopErr != nil || response.Publishing == nil || response.Publishing.Phase != "disabled" || response.Publishing.Running {
			message += " The worker has not yet confirmed that the connector is stopped."
		}
		return save("blocked", message)
	}
	zoneData, zoneErr := unseal(s.Key, zoneRaw)
	credentialData, credentialErr := unseal(s.Key, credentials)
	var zone cloudflareZone
	var credential cloudflareCredentials
	if zoneErr != nil || credentialErr != nil || json.Unmarshal(zoneData, &zone) != nil || json.Unmarshal(credentialData, &credential) != nil || kind != "token" || !validCloudflareToken(credential.Token) {
		return save("blocked", "An API-token connection with tunnel and DNS permission is required to continue or clean up.")
	}
	if !cleanup && (!zone.Enabled || zone.ConnectionID != connectionID) {
		response, stopErr := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "cloudflare_publishing_disable"})
		message := "The saved zone is disabled or its connection changed; publishing is suspended and saved ownership is retained."
		if stopErr != nil || response.Publishing == nil || response.Publishing.Phase != "disabled" {
			message += " Worker shutdown is unconfirmed."
		}
		return save("blocked", message)
	}
	zone.Token, zone.ConnectionID = credential.Token, ""
	if !cleanup {
		hosts, err = s.grantedTunnelHosts(ctx, tx, node, zoneID, owner, hosts)
		if err != nil {
			return err
		}
		if len(hosts) == 0 {
			response, stopErr := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "cloudflare_publishing_disable"})
			if stopErr != nil || response.Publishing == nil || response.Publishing.Phase != "disabled" || response.Publishing.Running {
				return save("blocked", "No selected hostname has a current public route and team zone grant; worker shutdown is unconfirmed.")
			}
			for _, record := range state.Records {
				record.Cleanup = true
			}
			if len(state.Records) > 0 {
				return s.reconcileTunnelDNS(ctx, tx, node, zone, &state, save)
			}
			return save("blocked", "Share the zone with the resource's team and select public exact hostnames before publishing. Saved tunnel ownership is retained.")
		}
	}
	if state.AccountID == "" {
		var remote struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Account struct {
				ID string `json:"id"`
			} `json:"account"`
		}
		if err := s.cloudflareRequest(ctx, zone, "GET", "", nil, &remote); err != nil {
			return save("blocked", "Could not confirm the zone account; check Zone Read and Account Cloudflare Tunnel Edit permission.")
		}
		if remote.ID != zoneID || remote.Name != zone.Domain || !cloudflareID.MatchString(remote.Account.ID) {
			return save("blocked", "Cloudflare returned an unexpected zone account; no tunnel was created.")
		}
		state.AccountID = remote.Account.ID
		return save("pending", "")
	}
	if cleanup && freshCreate && state.Identity.ID == "" {
		state.Identity.Attempted = false
	}
	if !state.Identity.Attempted && state.Identity.ID == "" && !cleanup {
		state.Identity.Attempted = true
		if err := save("provisioning", ""); err != nil {
			return err
		}
		return s.processCloudflareTunnel(ctx, node, true)
	}
	if state.Identity.ID == "" && state.Identity.Attempted {
		if freshCreate {
			intent := state.Identity
			intent.Attempted = false
			remote, err := s.createCloudflareTunnel(ctx, credential.Token, state.AccountID, intent)
			if err != nil {
				var status cloudflareStatusError
				if errors.As(err, &status) && status >= 400 && status < 500 && status != 408 && status != 409 {
					state.Identity.Attempted = false
					return save("blocked", "Cloudflare refused tunnel creation; check Account Cloudflare Tunnel Edit permission and provider limits.")
				}
				return save("confirming", "Tunnel creation may have completed; Cove will authenticate the saved credential before adopting or cleaning up a candidate.")
			}
			state.Identity.ID, state.Identity.Owned = remote.ID, true
			return save("connecting", "")
		}
		remote, err := s.findCloudflareTunnel(ctx, credential.Token, state.AccountID, state.Identity)
		if err != nil || remote == nil {
			return save("confirming", "An earlier tunnel create outcome is unknown. Its saved ownership credential is retained; no duplicate is created.")
		}
		state.Identity.ID = remote.ID
		return save("confirming", "A candidate was found; it must authenticate with the saved secret before adoption or cleanup.")
	}
	if state.Identity.ID != "" && !state.Identity.Owned {
		response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "cloudflare_publishing_apply", Publishing: &worker.CloudflarePublishing{ID: id, TunnelID: state.Identity.ID, AccountID: state.AccountID, TunnelSecret: state.Identity.Secret, Hostnames: hosts, Image: image, ProveOnly: true}})
		if err != nil || response.Publishing == nil || !response.Publishing.Connected || response.Publishing.ID != id || response.Publishing.TunnelID != state.Identity.ID {
			return save("confirming", "The candidate has not authenticated the saved credential. It is not adopted, published or deleted.")
		}
		state.Identity.Owned = true
		if cleanup {
			return save("disabling", "")
		}
		return save("connecting", "")
	}
	if cleanup {
		response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "cloudflare_publishing_disable"})
		if err != nil || response.Publishing == nil || response.Publishing.Phase != "disabled" || response.Publishing.Running {
			return save("disabling", "The worker has not confirmed the connector is stopped; provider cleanup waits.")
		}
		for _, record := range state.Records {
			record.Cleanup = true
		}
		if len(state.Records) > 0 {
			return s.reconcileTunnelDNS(ctx, tx, node, zone, &state, save)
		}
		if state.Identity.Owned && state.Identity.ID != "" {
			if err := s.removeCloudflareTunnel(ctx, credential.Token, state.AccountID, state.Identity); err != nil {
				return save("cleanup", "The owned tunnel could not be removed; its saved ownership proof is retained.")
			}
		}
		if _, err := tx.Exec(ctx, "DELETE FROM node_cloudflare_tunnels WHERE node_id=$1", node); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "cloudflare_publishing_apply", Publishing: &worker.CloudflarePublishing{ID: id, TunnelID: state.Identity.ID, AccountID: state.AccountID, TunnelSecret: state.Identity.Secret, Hostnames: hosts, Image: image}})
	if err != nil || response.Publishing == nil || !response.Publishing.Connected || response.Publishing.ID != id || response.Publishing.TunnelID != state.Identity.ID {
		return save("connecting", "The worker has not confirmed both an authenticated edge connection and verified local origin TLS. DNS publication waits.")
	}
	wanted := map[string]bool{}
	for _, host := range hosts {
		if _, withheld := response.Publishing.Withheld[host]; !withheld {
			wanted[host] = true
		}
	}
	for host, record := range state.Records {
		record.Cleanup = !wanted[host]
	}
	for host := range wanted {
		if state.Records[host] == nil {
			if len(state.Records) >= 256 {
				return save("blocked", "Finish outstanding DNS cleanup before adding more tunnel hostnames.")
			}
			state.Records[host] = &tunnelDNSState{Proof: tunnelDNSProof{Name: host, Content: state.Identity.ID + ".cfargotunnel.com", Comment: "cove-tunnel-" + resourceID()}, Phase: "pending"}
			return save("publishing", "")
		}
	}
	if len(state.Records) > 0 {
		return s.reconcileTunnelDNS(ctx, tx, node, zone, &state, save)
	}
	return save("active", "")
}

func (s *Server) reconcileTunnelDNS(ctx context.Context, tx pgx.Tx, node string, zone cloudflareZone, state *nodeTunnelPrivate, save func(string, string) error) error {
	hosts := make([]string, 0, len(state.Records))
	for host := range state.Records {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	for _, host := range hosts {
		record := state.Records[host]
		if record.Cleanup {
			if !record.Attempted && record.Proof.ID == "" {
				delete(state.Records, host)
				return save("cleanup", "")
			}
			if record.Proof.ID == "" {
				found, err := s.findTunnelDNS(ctx, zone, record.Proof)
				if err != nil || found == nil {
					record.Phase = "cleanup"
					record.LastError = "A create outcome remains unknown; the record may or may not exist. Ownership proof is retained."
					return save("cleanup", record.LastError)
				}
				record.Proof = *found
				record.Phase = "cleanup"
				return save("cleanup", "")
			}
			if err := s.removeTunnelDNS(ctx, zone, record.Proof); err != nil {
				record.Phase = "cleanup"
				record.LastError = "The DNS record could not be safely removed; unrelated records are untouched."
				return save("cleanup", record.LastError)
			}
			delete(state.Records, host)
			return save("cleanup", "")
		}
		if record.Phase == "present" {
			continue
		}
		if !record.Attempted {
			record.Attempted = true
			record.Phase = "presenting"
			if err := save("publishing", ""); err != nil {
				return err
			}
			// This lease is the sole fresh create. A restart only reconciles the saved proof.
			return s.createFreshTunnelDNS(ctx, node, host, record.Proof, zone)
		}
		found, err := s.presentTunnelDNS(ctx, zone, record.Proof, true)
		if err != nil || found == nil {
			record.LastError = "DNS publication is unconfirmed or conflicts with an existing record; Cove will not overwrite it."
			return save("publishing", record.LastError)
		}
		record.Proof = *found
		record.Phase = "present"
		record.LastError = ""
		return save("publishing", "")
	}
	return save("active", "")
}

func (s *Server) createFreshTunnelDNS(ctx context.Context, node, host string, expected tunnelDNSProof, zone cloudflareZone) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", routeClaimLock); err != nil {
		return err
	}
	var cleanup bool
	var hosts []string
	var encrypted []byte
	if err := tx.QueryRow(ctx, "SELECT state,cleanup_requested,hostnames FROM node_cloudflare_tunnels WHERE node_id=$1 FOR UPDATE", node).Scan(&encrypted, &cleanup, &hosts); err != nil {
		return err
	}
	data, err := unseal(s.Key, encrypted)
	var state nodeTunnelPrivate
	if err != nil || json.Unmarshal(data, &state) != nil {
		return errors.New("stored tunnel ownership state unavailable")
	}
	record := state.Records[host]
	if record == nil || record.Proof != expected {
		return errors.New("DNS ownership intent changed during publication")
	}
	var found *tunnelDNSProof
	var cause error
	var zoneID, owner string
	var authorized bool
	if err := tx.QueryRow(ctx, `SELECT t.zone_id,t.owner_id,cove_account_active(t.owner_id) AND u.is_admin
	 AND cove_account_active(z.owner_id) AND cove_account_active(a.owner_id)
	 FROM node_cloudflare_tunnels t JOIN users u ON u.id=t.owner_id
	 JOIN cloudflare_zones z ON z.zone_id=t.zone_id JOIN cloudflare_accounts a ON a.id=t.connection_id
	 WHERE t.node_id=$1 FOR SHARE OF u,z,a`, node).Scan(&zoneID, &owner, &authorized); err != nil {
		return err
	}
	granted, err := s.grantedTunnelHosts(ctx, tx, node, zoneID, owner, []string{host})
	if err != nil {
		return err
	}
	routes, err := s.tunnelRoutes(ctx, node)
	if err != nil {
		return err
	}
	if cleanup || !authorized || len(granted) == 0 || !slices.Contains(hosts, host) || worker.ValidatePublishedRoutes([]string{host}, routes) != nil {
		record.Cleanup = true
		record.Attempted = false
		record.Phase = "cleanup"
	} else {
		found, cause = s.presentTunnelDNS(ctx, zone, expected, false)
	}
	message := ""
	if record.Cleanup {
		record.LastError = ""
	} else if errors.Is(cause, errTunnelDNSNotCreated) {
		record.Attempted = false
		record.Phase = "pending"
		record.LastError = "No DNS record was created. Review conflicting records and DNS permission, then retry."
		message = record.LastError
	} else if cause != nil || found == nil {
		record.LastError = "DNS create outcome is unknown; saved ownership proof is retained."
		message = record.LastError
	} else {
		record.Proof = *found
		record.Phase = "present"
		record.LastError = ""
	}
	raw, err := s.sealJSON(state)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE node_cloudflare_tunnels SET state=$2,last_error=$3,next_check_at=now(),updated_at=now() WHERE node_id=$1", node, raw, message); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) runCloudflareTunnels(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.scheduleCloudflareTunnels(ctx); err != nil && ctx.Err() == nil {
			slog.Error("Tunnel reconciliation failed; saved ownership is retained", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
