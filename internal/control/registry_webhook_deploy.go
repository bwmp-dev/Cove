package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"
)

func (s *Server) registryWebhookDeployment(id string, encrypted []byte) (worker.Request, error) {
	var config workloadInput
	raw, err := unseal(s.Key, encrypted)
	if err != nil || json.Unmarshal(raw, &config) != nil {
		return worker.Request{}, errors.New("saved resource configuration is unavailable")
	}
	if config.Kind != "service" || config.Source != nil {
		return worker.Request{}, errors.New("registry redeployments require a service with saved image-only Compose, without a Git source")
	}
	var compose struct {
		Services map[string]struct {
			Image string `yaml:"image"`
			Build any    `yaml:"build"`
		} `yaml:"services"`
	}
	if yaml.Unmarshal([]byte(config.Compose), &compose) != nil || len(compose.Services) == 0 {
		return worker.Request{}, errors.New("saved image-only Compose is unavailable")
	}
	mutable := false
	for _, service := range compose.Services {
		if service.Build != nil || service.Image == "" {
			return worker.Request{}, errors.New("remove Compose build services before enabling registry redeployments")
		}
		mutable = mutable || !strings.Contains(service.Image, "@")
	}
	if !mutable {
		return worker.Request{}, errors.New("registry redeployments need at least one image tag rather than only pinned digests")
	}
	request := worker.Request{ID: resourceID(), Workload: id, Action: "deploy", Kind: "service", PullLatest: true,
		Compose: config.Compose, Environment: config.Environment, Routes: config.Routes, Private: config.Private,
		Limits: config.Limits, Hooks: config.Hooks, FileSecrets: config.FileSecrets, GuardedDeploy: config.GuardedDeploy, Rollout: config.Rollout}
	return request, worker.ValidateRequest(request)
}

func registryWebhookStopReason(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	var stopped, paused, excluded bool
	err := tx.QueryRow(ctx, `SELECT w.manually_stopped,EXISTS(SELECT 1 FROM environments e WHERE e.id=w.environment_id AND e.paused) OR EXISTS(SELECT 1 FROM environment_suspensions c WHERE c.workload_id=w.id),w.kind<>'service' OR EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id) OR EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id) FROM workloads w WHERE w.id=$1`, id).Scan(&stopped, &paused, &excluded)
	if err != nil {
		return "", err
	}
	if excluded {
		return "Registry redeployments exclude jobs and managed previews.", nil
	}
	if stopped {
		return "Resource manually stopped; push ignored.", nil
	}
	if paused {
		return "Environment paused or resuming; push ignored.", nil
	}
	return "", nil
}

func (s *Server) dispatchRegistryWebhooks(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT h.workload_id FROM registry_webhooks h JOIN workloads w ON w.id=h.workload_id JOIN nodes n ON n.id=w.node_id WHERE h.pending_at IS NOT NULL AND h.next_dispatch_at<=now() AND
 (NOT h.enabled OR w.manually_stopped OR w.kind<>'service' OR EXISTS(SELECT 1 FROM environments e WHERE e.id=w.environment_id AND e.paused) OR EXISTS(SELECT 1 FROM environment_suspensions c WHERE c.workload_id=w.id) OR
 (n.state='ready' AND COALESCE((n.capabilities->>'image_lifecycle')::boolean,false) AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.result->'rollout'->>'phase'='waiting')))
 ORDER BY h.pending_at LIMIT 4`)
	if err != nil {
		return err
	}
	var ids []string
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
		if err = s.dispatchRegistryWebhook(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) dispatchRegistryWebhook(ctx context.Context, id string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var configuration []byte
	var ready, active, held bool
	err = tx.QueryRow(ctx, `SELECT w.configuration,n.state='ready' AND COALESCE((n.capabilities->>'image_lifecycle')::boolean,false),EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')),EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.result->'rollout'->>'phase'='waiting') FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w`, id).Scan(&configuration, &ready, &active, &held)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Pause locks the environment before clearing hook intent. Take its shared admission lock before the hook lock.
	if _, err = environmentOperationGate(ctx, tx, id, "deploy"); err != nil && !errors.Is(err, errEnvironmentPaused) {
		return err
	}
	var enabled, pending bool
	var registry, server string
	err = tx.QueryRow(ctx, `SELECT enabled,pending_at IS NOT NULL,registry_id,registry_server FROM registry_webhooks WHERE workload_id=$1 FOR UPDATE`, id).Scan(&enabled, &pending, &registry, &server)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !pending {
		return nil
	}
	ignored, err := registryWebhookStopReason(ctx, tx, id)
	if err != nil {
		return err
	}
	if !enabled {
		ignored = "Webhook disabled."
	}
	connected, _, currentServer, err := s.connectedWebhookRegistry(ctx, tx, id)
	if err != nil {
		return err
	}
	if registry != connected || server != currentServer {
		ignored = "Registry selection changed; save webhook setup again."
	}
	finish := func(reason string) error {
		_, err := tx.Exec(ctx, `UPDATE registry_webhooks SET pending_at=NULL,last_ignored_reason=$2,last_error='' WHERE workload_id=$1`, id, reason)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if ignored != "" {
		return finish(ignored)
	}
	if !ready || active || held {
		return nil
	}
	if err = s.idleOperationGate(ctx, tx, id, "deploy"); err != nil {
		if errors.Is(err, errIdleAsleep) {
			return finish("Resource sleeping; push ignored without waking it.")
		}
		_, writeErr := tx.Exec(ctx, `UPDATE registry_webhooks SET next_dispatch_at=now()+interval '1 minute',last_error='Could not confirm whether the app is asleep; deployment is pending and will be checked again in about a minute.' WHERE workload_id=$1`, id)
		if writeErr != nil {
			return writeErr
		}
		return tx.Commit(ctx)
	}
	request, err := s.registryWebhookDeployment(id, configuration)
	if err != nil {
		_, writeErr := tx.Exec(ctx, `UPDATE registry_webhooks SET pending_at=NULL,last_error=$2 WHERE workload_id=$1`, id, err.Error())
		if writeErr != nil {
			return writeErr
		}
		return tx.Commit(ctx)
	}
	if err = loadNetworkRequest(ctx, tx, &request); err != nil {
		if _, writeErr := tx.Exec(ctx, `UPDATE registry_webhooks SET next_dispatch_at=now()+interval '1 minute',last_error='Saved network configuration is unavailable; deployment is pending. Verify the worker and network settings.' WHERE workload_id=$1`, id); writeErr != nil {
			return writeErr
		}
		return tx.Commit(ctx)
	}
	payload, err := s.sealJSON(request)
	if err != nil {
		return err
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'deploy',$3) ON CONFLICT DO NOTHING`, request.ID, id, payload)
	if err != nil {
		return err
	}
	if inserted.RowsAffected() == 0 {
		return nil
	}
	if _, err = tx.Exec(ctx, `UPDATE registry_webhooks SET pending_at=NULL,last_operation_id=$2,last_error='',last_ignored_reason='' WHERE workload_id=$1`, id, request.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status) VALUES('registry webhook','pull and redeploy queued',$1,202)`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
