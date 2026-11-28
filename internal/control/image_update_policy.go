package control

import (
	"context"
	"cove/internal/worker"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) validateImageUpdateConfiguration(id string, encrypted []byte, policy worker.ImageUpdatePolicy) ([]string, error) {
	if _, err := s.registryWebhookDeployment(id, encrypted); err != nil {
		return nil, err
	}
	data, err := unseal(s.Key, encrypted)
	if err != nil {
		return nil, errors.New("saved configuration unavailable")
	}
	var config workloadInput
	if json.Unmarshal(data, &config) != nil {
		return nil, errors.New("saved configuration unavailable")
	}
	images, err := imageOnlyServiceImages(config)
	if err != nil {
		return nil, err
	}
	if err := worker.ValidateImageUpdateImages(images); err != nil {
		return nil, err
	}
	services := []string{}
	for service := range images {
		services = append(services, service)
	}
	for _, rule := range policy.Rules {
		image, exists := images[rule.Service]
		if !exists || strings.ContainsAny(image, "@$") {
			return nil, errors.New("each selected service needs an explicit mutable image tag")
		}
	}
	return services, nil
}

func (s *Server) imageUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "PUT" {
		var input struct {
			Enabled     *bool                    `json:"enabled"`
			Mode        string                   `json:"mode"`
			Rules       []worker.ImageUpdateRule `json:"rules"`
			WindowStart string                   `json:"window_start"`
			WindowEnd   string                   `json:"window_end"`
		}
		if !decode(w, r, &input) {
			return
		}
		if input.Enabled == nil {
			http.Error(w, "enabled is required", 400)
			return
		}
		policy := worker.ImageUpdatePolicy{Enabled: *input.Enabled, Mode: input.Mode, Rules: input.Rules, WindowStart: input.WindowStart, WindowEnd: input.WindowEnd}
		if err := policy.Validate(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if !validDeploymentWindow(policy.WindowStart, policy.WindowEnd) {
			http.Error(w, "set two different UTC times in HH:MM format, or clear both", 400)
			return
		}
		if policy.Enabled && !s.requirePermission(w, r, "workload", id, "operate") {
			return
		}
		tx, err := s.DB.Begin(r.Context())
		if err != nil {
			http.Error(w, "image update policy unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		var encrypted []byte
		var supported, eligible, active bool
		err = tx.QueryRow(r.Context(), `SELECT w.configuration,n.state='ready' AND (COALESCE((n.capabilities->>'image_updates')::boolean,false) AND COALESCE((n.capabilities->>'image_lifecycle')::boolean,false)),w.kind='service' AND NOT EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id) AND NOT EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id),EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w`, id).Scan(&encrypted, &supported, &eligible, &active)
		if err != nil {
			http.Error(w, "image update policy unavailable", 503)
			return
		}
		if policy.Enabled && !eligible {
			http.Error(w, "image update policies require a service outside managed previews", 400)
			return
		}
		if policy.Enabled && !supported {
			http.Error(w, "upgrade and verify the worker before enabling image updates", 409)
			return
		}
		if active && policy.Enabled {
			http.Error(w, "wait for the active operation before changing image updates", 409)
			return
		}
		if _, err = s.validateImageUpdateConfiguration(id, encrypted, policy); policy.Enabled && err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if _, err = tx.Exec(r.Context(), `UPDATE operations o SET cancel_requested=true,updated_at=now() WHERE o.workload_id=$1 AND o.state IN ('queued','running') AND EXISTS(SELECT 1 FROM image_update_deployments d WHERE d.operation_id=o.id)`, id); err != nil {
			http.Error(w, "could not revoke old image update work", 503)
			return
		}
		data, err := json.Marshal(policy)
		if err != nil {
			http.Error(w, "could not save policy", 503)
			return
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO image_update_policies(workload_id,policy,generation,creator_id) VALUES($1,$2,$3,NULLIF($4,'')) ON CONFLICT(workload_id) DO UPDATE SET policy=$2,generation=$3,creator_id=NULLIF($4,''),last_check_id=NULL,processed_check_id=NULL,last_operation_id=NULL,consumed_candidate='',last_error='',retained_images=CASE WHEN $5 THEN image_update_policies.retained_images ELSE '{}'::jsonb END,updated_at=now()`, id, data, resourceID(), actor(r).ID, policy.Enabled && policy.Mode == "auto")
		if err == nil && policy.Enabled && policy.Mode == "auto" {
			var config workloadInput
			if err = s.unsealJSON(encrypted, &config); err == nil {
				err = s.pruneImageUpdateRetention(r.Context(), tx, id, config)
			}
		}
		if err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE workloads SET next_image_check_at=now() WHERE id=$1`, id)
		}
		if err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			http.Error(w, "could not save policy", 503)
			return
		}
	}
	status, err := s.readImageUpdatePolicy(r.Context(), id)
	if err != nil {
		http.Error(w, "image update policy unavailable", 503)
		return
	}
	if err = s.imageUpdatePolicyMetadata(r.Context(), id, status); err != nil {
		http.Error(w, "image update policy unavailable", 503)
		return
	}
	respond(w, 200, status)
}

func (s *Server) readImageUpdatePolicy(ctx context.Context, id string) (map[string]any, error) {
	var data, retainedData []byte
	var checked, operation *string
	var message string
	var ownerAllowed bool
	err := s.DB.QueryRow(ctx, `SELECT p.policy,p.last_check_id,p.last_operation_id,p.last_error,p.retained_images,cove_automation_allowed(p.creator_id,w.team_id,w.project_id,COALESCE(w.environment_id,''),'operate') AND cove_automation_allowed(p.creator_id,w.team_id,w.project_id,COALESCE(w.environment_id,''),'edit_configuration') FROM image_update_policies p JOIN workloads w ON w.id=p.workload_id WHERE p.workload_id=$1`, id).Scan(&data, &checked, &operation, &message, &retainedData, &ownerAllowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]any{"retained_images": map[string]retainedImageUpdate{}, "ownership_required": false, "configuration": nil, "last_check_id": nil, "last_operation_id": nil, "last_error": "", "updates": nil, "checked_at": nil}, nil
	}
	if err != nil {
		return nil, err
	}
	var policy worker.ImageUpdatePolicy
	if err = json.Unmarshal(data, &policy); err != nil {
		return nil, err
	}
	retained := map[string]retainedImageUpdate{}
	if err = json.Unmarshal(retainedData, &retained); err != nil {
		return nil, err
	}
	var updates json.RawMessage
	var checkedAt *string
	var checkedTime *time.Time
	if checked != nil {
		err = s.DB.QueryRow(ctx, `SELECT result->'image_check'->'updates',updated_at FROM operations WHERE id=$1`, *checked).Scan(&updates, &checkedTime)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if checkedTime != nil {
			value := checkedTime.UTC().Format(time.RFC3339)
			checkedAt = &value
		}
	}
	return map[string]any{"retained_images": retained, "ownership_required": !ownerAllowed, "configuration": policy, "last_check_id": checked, "last_operation_id": operation, "last_error": message, "updates": updates, "checked_at": checkedAt}, nil
}

func imageUpdateConfigurationHash(configuration []byte) string {
	sum := sha256.Sum256(configuration)
	return hex.EncodeToString(sum[:])
}

func (s *Server) prepareImageUpdateCheck(ctx context.Context, tx pgx.Tx, request *worker.Request) error {
	if request.Kind == "job" {
		return nil
	}
	var policyData, configuration []byte
	var generation string
	var supported, ownerAllowed bool
	err := tx.QueryRow(ctx, `SELECT p.policy,p.generation,w.configuration,(COALESCE((n.capabilities->>'image_updates')::boolean,false) AND COALESCE((n.capabilities->>'image_lifecycle')::boolean,false)),cove_automation_allowed(p.creator_id,w.team_id,w.project_id,COALESCE(w.environment_id,''),'operate') AND cove_automation_allowed(p.creator_id,w.team_id,w.project_id,COALESCE(w.environment_id,''),'edit_configuration') FROM image_update_policies p JOIN workloads w ON w.id=p.workload_id JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, request.Workload).Scan(&policyData, &generation, &configuration, &supported, &ownerAllowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var policy worker.ImageUpdatePolicy
	if err = json.Unmarshal(policyData, &policy); err != nil {
		return err
	}
	if !policy.Enabled || !supported || !ownerAllowed {
		return nil
	}
	if _, err = s.validateImageUpdateConfiguration(request.Workload, configuration, policy); err != nil {
		_, writeErr := tx.Exec(ctx, `UPDATE image_update_policies SET last_error=$2 WHERE workload_id=$1`, request.Workload, err.Error())
		return writeErr
	}
	raw, err := unseal(s.Key, configuration)
	if err != nil {
		return err
	}
	var config workloadInput
	if err = json.Unmarshal(raw, &config); err != nil {
		return err
	}
	images, err := imageOnlyServiceImages(config)
	if err != nil {
		return err
	}
	request.ImageUpdates = &worker.ImageUpdateOptions{Policy: policy, Images: images, Generation: generation, Configuration: imageUpdateConfigurationHash(configuration)}
	return request.ImageUpdates.Validate()
}

func (s *Server) imageUpdatePolicyMetadata(ctx context.Context, id string, status map[string]any) error {
	var encrypted []byte
	var supported, ready, eligible bool
	var active *string
	err := s.DB.QueryRow(ctx, `SELECT w.configuration,(COALESCE((n.capabilities->>'image_updates')::boolean,false) AND COALESCE((n.capabilities->>'image_lifecycle')::boolean,false)),n.state='ready',w.kind='service' AND NOT EXISTS(SELECT 1 FROM previews WHERE workload_id=w.id) AND NOT EXISTS(SELECT 1 FROM environment_preview_resources WHERE resource_id=w.id),(SELECT id FROM operations WHERE workload_id=w.id AND state IN ('queued','running') LIMIT 1) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, id).Scan(&encrypted, &supported, &ready, &eligible, &active)
	if err != nil {
		return err
	}
	reason := ""
	if _, err = s.registryWebhookDeployment(id, encrypted); err != nil {
		eligible = false
		reason = err.Error()
	}
	if !eligible && reason == "" {
		reason = "Image updates require an image-only service outside managed previews."
	}
	raw, err := unseal(s.Key, encrypted)
	if err != nil {
		return err
	}
	var config workloadInput
	if err = json.Unmarshal(raw, &config); err != nil {
		return err
	}
	images, err := imageOnlyServiceImages(config)
	if err != nil {
		images = map[string]string{}
	}
	if imageErr := worker.ValidateImageUpdateImages(images); imageErr != nil && eligible {
		eligible = false
		reason = imageErr.Error()
	}
	type service struct {
		Service string `json:"service"`
		Image   string `json:"image"`
		Mutable bool   `json:"mutable"`
	}
	services := []service{}
	for name, image := range images {
		services = append(services, service{name, image, image != "" && !strings.ContainsAny(image, "@$")})
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Service < services[j].Service })
	stale := true
	freshness := ""
	if policy, ok := status["configuration"].(worker.ImageUpdatePolicy); ok && policy.Enabled && eligible && supported && status["ownership_required"] != true {
		freshness = "Run an image check for the current policy and configuration."
	}
	if check, ok := status["last_check_id"].(*string); ok && check != nil {
		var payload []byte
		var newer, unconfirmed bool
		err = s.DB.QueryRow(ctx, `SELECT payload,EXISTS(SELECT 1 FROM operations newer WHERE newer.workload_id=o.workload_id AND newer.action IN ('deploy','rollback','start','restart','stop','resume','image_check') AND newer.updated_at>o.updated_at),EXISTS(SELECT 1 FROM operations newer WHERE newer.workload_id=o.workload_id AND newer.action IN ('deploy','rollback','start','restart','stop','resume','image_check') AND newer.updated_at>o.updated_at AND (newer.action='image_check' OR newer.state<>'succeeded' OR COALESCE(newer.result->'rollout'->>'phase','') IN ('waiting','aborted'))) FROM operations o WHERE id=$1`, *check).Scan(&payload, &newer, &unconfirmed)
		if err != nil {
			return err
		}
		plain, err := unseal(s.Key, payload)
		if err != nil {
			return err
		}
		var request worker.Request
		if err = json.Unmarshal(plain, &request); err != nil {
			return err
		}
		configurationCurrent := request.ImageUpdates != nil && request.ImageUpdates.Configuration == imageUpdateConfigurationHash(encrypted)
		if configurationCurrent {
			freshness = ""
		}
		stale = newer || !configurationCurrent
		if configurationCurrent && !unconfirmed && markRetainedImageUpdatesApplied(status, request.ImageUpdates.Images) {
			stale = false
		}
	}
	status["stale"] = stale
	status["freshness_message"] = freshness
	status["services"] = services
	status["eligible"] = eligible
	status["unavailable_reason"] = reason
	status["worker_supported"] = supported
	status["node_ready"] = ready
	status["active_operation_id"] = active
	return nil
}
