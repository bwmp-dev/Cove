package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) dispatchImageUpdates(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT p.workload_id FROM image_update_policies p JOIN workloads w ON w.id=p.workload_id JOIN nodes n ON n.id=w.node_id WHERE p.policy->>'enabled'='true' AND p.next_dispatch_at<=now() AND n.state='ready' AND (COALESCE((n.capabilities->>'image_updates')::boolean,false) AND COALESCE((n.capabilities->>'image_lifecycle')::boolean,false)) AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) ORDER BY p.next_dispatch_at LIMIT 4`)
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
		if err = s.dispatchImageUpdate(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) dispatchImageUpdate(ctx context.Context, id string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var configuration []byte
	var active, held bool
	if err = tx.QueryRow(ctx, `SELECT configuration,EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')),EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.result->'rollout'->>'phase'='waiting') FROM workloads w WHERE id=$1 FOR UPDATE`, id).Scan(&configuration, &active, &held); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if active || held {
		return nil
	}
	if _, err = environmentOperationGate(ctx, tx, id, "deploy"); err != nil && !errors.Is(err, errEnvironmentPaused) {
		return err
	}
	var policyData []byte
	var generation, consumed string
	var retainedData []byte
	var lastCheck *string
	var updated time.Time
	if err = tx.QueryRow(ctx, `SELECT policy,generation,consumed_candidate,processed_check_id,updated_at,retained_images FROM image_update_policies WHERE workload_id=$1 FOR UPDATE`, id).Scan(&policyData, &generation, &consumed, &lastCheck, &updated, &retainedData); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	var policy worker.ImageUpdatePolicy
	if err = json.Unmarshal(policyData, &policy); err != nil {
		return err
	}
	var retained map[string]retainedImageUpdate
	if err = json.Unmarshal(retainedData, &retained); err != nil {
		return err
	}
	var ownerAllowed bool
	if err = tx.QueryRow(ctx, `SELECT cove_automation_allowed(p.creator_id,w.team_id,w.project_id,COALESCE(w.environment_id,''),'operate') AND cove_automation_allowed(p.creator_id,w.team_id,w.project_id,COALESCE(w.environment_id,''),'edit_configuration') FROM image_update_policies p JOIN workloads w ON w.id=p.workload_id WHERE w.id=$1`, id).Scan(&ownerAllowed); err != nil {
		return err
	}
	if !policy.Enabled {
		return nil
	}
	finish := func(message string) error {
		_, err := tx.Exec(ctx, `UPDATE image_update_policies SET last_error=$2,next_dispatch_at=now()+interval '1 minute' WHERE workload_id=$1`, id, message)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if !ownerAllowed {
		return finish("Image updates have no currently authorised owner; save the policy to take ownership.")
	}
	if reason, gateErr := imageAutomationStopReason(ctx, tx, id); gateErr != nil {
		return gateErr
	} else if reason != "" {
		return finish(reason)
	}
	if err = s.idleOperationGate(ctx, tx, id, "deploy"); err != nil {
		return finish("Image updates wait until the resource is confirmed awake.")
	}
	var check string
	var payload, result []byte
	var checked time.Time
	err = tx.QueryRow(ctx, `SELECT id,payload,result,updated_at FROM operations WHERE workload_id=$1 AND action='image_check' AND state='succeeded' AND updated_at>$2 ORDER BY updated_at DESC LIMIT 1 FOR KEY SHARE`, id, updated).Scan(&check, &payload, &result, &checked)
	if errors.Is(err, pgx.ErrNoRows) {
		return finish("")
	}
	if err != nil {
		return err
	}
	if lastCheck != nil && *lastCheck == check {
		return finish("")
	}
	plain, err := unseal(s.Key, payload)
	if err != nil {
		return err
	}
	var request worker.Request
	var operation worker.Operation
	if json.Unmarshal(plain, &request) != nil || json.Unmarshal(result, &operation) != nil {
		return finish("The latest image check result is unavailable.")
	}
	if request.ImageUpdates == nil || request.ImageUpdates.Generation != generation || request.ImageUpdates.Configuration != imageUpdateConfigurationHash(configuration) {
		return finish("Run a new image check for the current policy and configuration.")
	}
	var newer bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND action IN ('deploy','rollback','start','restart','stop','resume') AND updated_at>$2)`, id, checked).Scan(&newer); err != nil {
		return err
	}
	if newer {
		return finish("Run a new image check after the latest resource operation.")
	}
	if operation.ImageCheck == nil || operation.ImageCheck.Updates == nil {
		return finish("The worker returned no image update result.")
	}
	if _, err = tx.Exec(ctx, `UPDATE image_update_policies SET last_check_id=$2 WHERE workload_id=$1`, id, check); err != nil {
		return err
	}
	updates := operation.ImageCheck.Updates
	if updates.State == "unknown" {
		return finish(updates.Message)
	}
	if policy.Mode == "notify" && updates.State == "available" && updates.Key != consumed {
		summary, err := json.Marshal(updates.Candidates)
		if err != nil {
			return err
		}
		if len(summary) > 32768 {
			return finish("Image update summary exceeds its size limit.")
		}
		if _, err = tx.Exec(ctx, `INSERT INTO image_update_events(operation_id,summary) VALUES($1,$2) ON CONFLICT DO NOTHING`, check, string(summary)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `SELECT cove_queue_channel_event('image.update_available','Verified image updates are available. Review the resource image update policy.',w.team_id,w.id,'image-update:'||$2||':'||$3,'') FROM workloads w WHERE w.id=$1`, id, generation, updates.Key); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE image_update_policies SET consumed_candidate=$2 WHERE workload_id=$1`, id, updates.Key); err != nil {
			return err
		}
	}
	if policy.Mode == "auto" && updates.State == "available" && (updates.Key != consumed || missingRetainedImageTargets(updates.Pins, retained)) {
		if !withinDeploymentWindow(policy.WindowStart, policy.WindowEnd, time.Now().UTC().Format("15:04")) {
			return finish("Waiting for the image update UTC maintenance window.")
		}
		deploy, err := s.registryWebhookDeployment(id, configuration)
		if err != nil {
			return finish(err.Error())
		}
		deploy.PullLatest = false
		deploy.ImagePins = updates.Pins
		if err = worker.ValidateRequest(deploy); err != nil {
			return finish("The candidate service image map is invalid.")
		}
		if err = loadNetworkRequest(ctx, tx, &deploy); err != nil {
			return finish("Saved network configuration is unavailable.")
		}
		sealed, err := s.sealJSON(deploy)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'deploy',$3)`, deploy.ID, id, sealed); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO image_update_deployments(operation_id,window_start,window_end,creator_id,generation) SELECT $1,$2,$3,creator_id,generation FROM image_update_policies WHERE workload_id=$4`, deploy.ID, policy.WindowStart, policy.WindowEnd, id); err != nil {
			return err
		}
		if err = saveOperationReason(ctx, tx, deploy.ID, "Image update policy: verified candidate", "image update policy"); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE image_update_policies SET last_operation_id=$2,consumed_candidate=$3 WHERE workload_id=$1`, id, deploy.ID, updates.Key); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE image_update_policies SET processed_check_id=$2,last_error='',next_dispatch_at=now()+interval '1 minute' WHERE workload_id=$1`, id, check); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
