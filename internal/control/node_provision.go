package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) providerInstance(ctx context.Context, provider nodeProvider, job nodeProvision) (*providerInstance, error) {
	path, key := "/servers", "servers"
	switch provider.Kind {
	case "digitalocean":
		path, key = "/droplets", "droplets"
	case "vultr":
		path, key = "/instances", "instances"
	}
	if job.CloudID != "" {
		if !providerValue.MatchString(job.CloudID) {
			return nil, errors.New("saved cloud server identity is invalid")
		}
		response, err := s.providerJSON(ctx, provider, "GET", path+"/"+job.CloudID, nil)
		if err != nil {
			return nil, err
		}
		var row map[string]json.RawMessage
		if json.Unmarshal(response[strings.TrimSuffix(key, "s")], &row) != nil {
			return nil, errors.New("cloud server response is invalid")
		}
		instance, err := decodeProviderInstance(provider, row, job.ID)
		if err != nil {
			return nil, err
		}
		if !instance.Owned || instance.Name != job.CloudName {
			return nil, errors.New("cloud server no longer matches its provisioning ownership marker")
		}
		return &instance, nil
	}
	query := url.Values{}
	if provider.Kind == "hetzner" {
		query.Set("label_selector", "cove-provision="+job.ID)
		query.Set("name", job.CloudName)
	}
	if provider.Kind == "digitalocean" {
		query.Set("tag_name", "cove-provision-"+job.ID)
	}
	rows, limited, err := s.providerRows(ctx, provider, path, key, query)
	if err != nil {
		return nil, err
	}
	var found *providerInstance
	for _, row := range rows {
		instance, err := decodeProviderInstance(provider, row, job.ID)
		if err != nil {
			return nil, err
		}
		if instance.Owned && instance.Name == job.CloudName {
			if found != nil {
				return nil, errors.New("multiple cloud servers match this creation; reconcile them in the provider console")
			}
			copy := instance
			found = &copy
		}
	}
	if limited {
		return nil, errors.New("cloud inventory exceeds 512 servers; reconcile using the exact owned server identifier")
	}
	return found, nil
}

func provisioningUserData(origin, token string) (string, error) {
	command := strings.Replace(setupCommand(origin, token), "&& sudo sh ", "&& sh ", 1)
	check := "if ! command -v cosign >/dev/null 2>&1; then " +
		"if ! curl --fail --silent --show-error --connect-timeout 10 --max-time 30 --proto '=https' -H 'Authorization: Bearer " + token + "' -H 'Content-Type: application/json' --data-binary '{\"enrollment_error\":\"trusted_verifier_missing\"}' '" + origin + "/bootstrap/register' >/dev/null; then echo 'Could not report missing trusted Cosign verifier to Cove.' >&2; fi; " +
		"echo 'Trusted Cosign v3+ is missing. Worker enrollment refused; install a trusted verifier and request a new enrollment, or delete this paid server in its provider console.' >&2; exit 1; fi; "
	command = check + command
	data, err := json.Marshal(map[string]any{"runcmd": [][]string{{"/bin/sh", "-c", command}}})
	if err != nil || len(data) > 65500 {
		return "", errors.New("cloud enrollment user data exceeds its limit")
	}
	return "#cloud-config\n" + string(data) + "\n", nil
}

func (s *Server) prepareNodeProvision(ctx context.Context, id string) (nodeProvision, nodeProvisionConfig, error) {
	job := nodeProvision{ID: id}
	config := nodeProvisionConfig{}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return job, config, err
	}
	defer tx.Rollback(ctx)
	var encrypted []byte
	var team string
	err = tx.QueryRow(ctx, `SELECT provider_id,provider_kind,provider_name,node_id,cloud_id,cloud_name,phase,configuration,team_id,created_at,updated_at,deadline FROM node_provisions WHERE id=$1 FOR UPDATE`, id).Scan(&job.ProviderID, &job.ProviderKind, &job.ProviderName, &job.NodeID, &job.CloudID, &job.CloudName, &job.Phase, &encrypted, &team, &job.CreatedAt, &job.UpdatedAt, &job.Deadline)
	if err != nil {
		return job, config, err
	}
	if job.Phase != "queued" {
		return job, config, tx.Commit(ctx)
	}
	var authorised bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_provisions p JOIN users u ON u.id=p.creator_id WHERE p.id=$1 AND u.is_admin AND cove_automation_allowed(u.id,p.team_id,'','','edit_configuration'))`, id).Scan(&authorised); err != nil {
		return job, config, err
	}
	if !authorised {
		_, err = tx.Exec(ctx, `UPDATE node_provisions SET phase='cancelled',configuration='\x'::bytea,last_error='Server creation cancelled because its creator is no longer an authorised instance administrator.',updated_at=now() WHERE id=$1`, id)
		if err != nil {
			return job, config, err
		}
		job.Phase = "cancelled"
		return job, config, tx.Commit(ctx)
	}
	data, err := unseal(s.Key, encrypted)
	if err != nil || json.Unmarshal(data, &config) != nil {
		return job, config, errors.New("provisioning configuration cannot be decrypted")
	}
	if !config.Verifier {
		if _, err = tx.Exec(ctx, `UPDATE node_provisions SET phase='cancelled',configuration='\x'::bytea,last_error='Server creation cancelled: acknowledge an image containing trusted Cosign v3+ before submitting a new paid creation.',updated_at=now() WHERE id=$1`, id); err != nil {
			return job, config, err
		}
		job.Phase = "cancelled"
		return job, config, tx.Commit(ctx)
	}
	pub, priv, err := keyPair()
	if err != nil {
		return job, config, err
	}
	secret, err := seal(s.Key, priv)
	if err != nil {
		return job, config, err
	}
	node, token := randomToken(), randomToken()
	config.Token = token
	data, err = json.Marshal(config)
	if err != nil {
		return job, config, err
	}
	payload, err := seal(s.Key, data)
	if err != nil {
		return job, config, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO nodes(id,name,address,port,public_key,private_key,cloud_provider_id,cloud_kind,cloud_server_name) VALUES($1,$2,'pending.invalid',22,$3,$4,$5,$6,$7)`, node, config.Name, pub, secret, job.ProviderID, job.ProviderKind, job.CloudName); err != nil {
		return job, config, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO team_nodes(team_id,node_id) VALUES($1,$2)`, team, node); err != nil {
		return job, config, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO enrollments(token_hash,node_id,expires_at) VALUES($1,$2,now()+interval '15 minutes')`, tokenHash(token), node); err != nil {
		return job, config, err
	}
	if config.Cost != nil {
		if _, err = tx.Exec(ctx, `INSERT INTO node_costs(node_id,monthly_minor,currency) VALUES($1,$2,$3)`, node, config.Cost.MonthlyMinor, config.Cost.Currency); err != nil {
			return job, config, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE node_provisions SET phase='creating',node_id=$2,configuration=$3,deadline=now()+interval '20 minutes',updated_at=now() WHERE id=$1`, id, node, payload); err != nil {
		return job, config, err
	}
	job.NodeID = &node
	job.Phase = "creating"
	job.Deadline = time.Now().Add(20 * time.Minute)
	return job, config, tx.Commit(ctx)
}

func (s *Server) markProvision(ctx context.Context, id, phase, message string) error {
	if len(message) > 1024 {
		message = message[:1024]
	}
	_, err := s.DB.Exec(ctx, `WITH changed AS (UPDATE node_provisions SET phase=$2,last_error=$3,configuration=CASE WHEN $2 IN ('failed','succeeded','cancelled') THEN '\x'::bytea ELSE configuration END,updated_at=now(),next_check_at=now()+interval '15 seconds' WHERE id=$1 AND phase IN ('queued','creating','server_pending','enrolling','uncertain') RETURNING node_id) UPDATE enrollments SET consumed_at=now() WHERE $2 IN ('failed','cancelled') AND node_id IN (SELECT node_id FROM changed) AND consumed_at IS NULL`, id, phase, message)
	return err
}

func (s *Server) processNodeProvision(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var before string
	if err := s.DB.QueryRow(ctx, `SELECT phase FROM node_provisions WHERE id=$1`, id).Scan(&before); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if before == "uncertain" || before == "succeeded" || before == "cancelled" || before == "failed" {
		return nil
	}
	job, config, err := s.prepareNodeProvision(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if job.Phase == "cancelled" {
		return nil
	}
	if job.ProviderID == nil || job.NodeID == nil {
		return s.markProvision(ctx, id, "uncertain", "Provider connection or node was removed; inspect the cloud console before abandoning tracking.")
	}
	provider, err := s.loadNodeProvider(ctx, *job.ProviderID)
	if err != nil {
		return s.markProvision(ctx, id, "uncertain", "Provider credentials unavailable; reconnect and reconcile the existing server.")
	}
	if before == "queued" && job.Phase == "creating" && config.Token != "" {
		userData, err := provisioningUserData(s.PublicURL, config.Token)
		if err != nil {
			return s.markProvision(ctx, id, "failed", err.Error())
		}
		path, body, err := providerCreateBody(provider, job.CloudName, config.Region, config.Plan, config.Image, userData, job.ID)
		if err != nil {
			return s.markProvision(ctx, id, "failed", err.Error())
		}
		// Persisting 'creating' before this sole POST makes a lost acknowledgement recover by lookup, never by another creation.
		response, createErr := s.providerJSON(ctx, provider, "POST", path, body)
		if createErr != nil {
			var status *providerHTTPError
			if errors.As(createErr, &status) && (status.Status == 400 || status.Status == 401 || status.Status == 403 || status.Status == 404 || status.Status == 422) {
				return s.markProvision(ctx, id, "failed", createErr.Error()+"; inspect the provider console before creating another server.")
			}
			return s.markProvision(ctx, id, "creating", "Creation acknowledgement was lost or deferred; Cove will only look up its ownership marker, never repeat creation.")
		}
		key := "server"
		if provider.Kind == "digitalocean" {
			key = "droplet"
		} else if provider.Kind == "vultr" {
			key = "instance"
		}
		var row map[string]json.RawMessage
		if json.Unmarshal(response[key], &row) != nil {
			return s.markProvision(ctx, id, "creating", "Creation returned an incomplete response; look up the owned server before any retry.")
		}
		instance, err := decodeProviderInstance(provider, row, job.ID)
		if err != nil || !instance.Owned || instance.Name != job.CloudName {
			return s.markProvision(ctx, id, "creating", "Creation identity could not be verified; reconcile the exact owned server.")
		}
		tag, err := s.DB.Exec(ctx, `UPDATE node_provisions SET cloud_id=$2,phase='server_pending',last_error='',updated_at=now(),next_check_at=now() WHERE id=$1 AND phase='creating'`, id, instance.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return nil
		}
		job.CloudID, job.Phase = instance.ID, "server_pending"
	}
	if job.Phase != "creating" && job.Phase != "server_pending" && job.Phase != "enrolling" {
		return nil
	}
	instance, lookupErr := s.providerInstance(ctx, provider, job)
	if lookupErr != nil || instance == nil {
		if !job.Deadline.IsZero() && time.Now().After(job.Deadline) {
			return s.markProvision(ctx, id, "uncertain", "Server identity or enrollment could not be confirmed in 20 minutes; inspect the provider console and reconcile, never blindly create again.")
		}
		message := "Waiting for the uniquely marked server; no creation request will be repeated."
		if lookupErr != nil {
			message = lookupErr.Error()
		}
		return s.markProvision(ctx, id, job.Phase, message)
	}
	if instance.Address != "" {
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		var phase string
		if err = tx.QueryRow(ctx, `SELECT phase FROM node_provisions WHERE id=$1 FOR UPDATE`, id).Scan(&phase); err != nil {
			return err
		}
		if phase == "cancelled" || phase == "failed" || phase == "succeeded" || phase == "uncertain" {
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE nodes SET address=$2,cloud_server_id=$3,state=CASE WHEN address=$2 THEN state WHEN host_key IS NULL THEN 'pending' ELSE 'enrolled' END,last_checked_at=CASE WHEN address=$2 THEN last_checked_at ELSE NULL END WHERE id=$1 AND (address='pending.invalid' OR address=$2)`, *job.NodeID, instance.Address, instance.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			_ = tx.Rollback(ctx)
			return s.markProvision(ctx, id, "uncertain", "Node address changed outside provisioning; reconcile through the provider and node settings.")
		}
		var ready bool
		if err = tx.QueryRow(ctx, `SELECT state='ready' AND host_key IS NOT NULL FROM nodes WHERE id=$1`, *job.NodeID).Scan(&ready); err != nil {
			return err
		}
		phase = "enrolling"
		message := "Waiting for cloud-init, one-time enrollment and pinned SSH readiness."
		if ready {
			phase, message = "succeeded", ""
		} else if !job.Deadline.IsZero() && time.Now().After(job.Deadline) {
			phase, message = "uncertain", "The paid server exists but enrollment is not ready. Inspect cloud-init and confirm the image contains trusted Cosign v3+; a missing verifier refuses enrollment. Repair it and renew enrollment, or delete the server in its provider console. Creation will not be repeated."
		}
		_, err = tx.Exec(ctx, `UPDATE node_provisions SET cloud_id=$2,phase=$3,last_error=$4,configuration=CASE WHEN $3='succeeded' THEN '\x'::bytea ELSE configuration END,updated_at=now(),next_check_at=now()+interval '15 seconds' WHERE id=$1`, id, instance.ID, phase, message)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if time.Now().After(job.Deadline) {
		return s.markProvision(ctx, id, "uncertain", "The owned server has no usable public IPv4 address; inspect the provider console before proceeding.")
	}
	return s.markProvision(ctx, id, job.Phase, "Waiting for a public IPv4 address; the cloud server may already be billable.")
}

func (s *Server) runNodeProvisions(ctx context.Context) error {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		rows, err := s.DB.Query(ctx, `SELECT id FROM node_provisions WHERE phase IN ('queued','creating','server_pending','enrolling') AND next_check_at<=now() ORDER BY next_check_at LIMIT 4`)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err = s.processNodeProvision(ctx, id); err != nil && ctx.Err() == nil {
				return err
			}
		}
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			return err
		}
		err = pruneNodeProvisions(ctx, tx)
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
