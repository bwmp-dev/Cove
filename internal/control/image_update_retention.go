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

type retainedImageUpdate struct {
	Configured string `json:"configured"`
	Image      string `json:"image"`
}

type imageUpdateRetentionError struct{ message string }

func (e *imageUpdateRetentionError) Error() string { return e.message }

func readRetainedImageUpdates(ctx context.Context, tx pgx.Tx, id string) (worker.ImageUpdatePolicy, map[string]retainedImageUpdate, error) {
	var policy worker.ImageUpdatePolicy
	retained := map[string]retainedImageUpdate{}
	var raw, images []byte
	err := tx.QueryRow(ctx, `SELECT policy,retained_images FROM image_update_policies WHERE workload_id=$1 FOR UPDATE`, id).Scan(&raw, &images)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy, retained, nil
	}
	if err != nil {
		return policy, retained, err
	}
	if json.Unmarshal(raw, &policy) != nil || json.Unmarshal(images, &retained) != nil || retained == nil || len(retained) > 64 {
		return policy, nil, errors.New("retained automatic image versions are unavailable")
	}
	configured, pins := map[string]string{}, map[string]string{}
	for service, image := range retained {
		configured[service], pins[service] = image.Configured, image.Image
		if !strings.Contains(image.Image, "@sha256:") {
			return policy, nil, errors.New("retained automatic image version is not an immutable registry reference")
		}
	}
	if len(retained) > 0 {
		if err := worker.ValidateImageUpdateImages(configured); err != nil {
			return policy, nil, err
		}
		if err := worker.ValidateRequest(worker.Request{ID: "retention", Workload: "retention", Action: "deploy", Kind: "service", Compose: "services: {}", ImagePins: pins}); err != nil {
			return policy, nil, err
		}
	}
	return policy, retained, nil
}

func imageUpdateConfiguredImages(config workloadInput) map[string]string {
	if resourceKind(config) != "service" || config.Source != nil || config.Database != nil {
		return nil
	}
	images, err := imageOnlyServiceImages(config)
	if err != nil {
		return nil
	}
	return images
}

func pruneRetainedImages(policy worker.ImageUpdatePolicy, retained map[string]retainedImageUpdate, config workloadInput) {
	images := imageUpdateConfiguredImages(config)
	for service, target := range retained {
		if !policy.Enabled || policy.Mode != "auto" || images[service] != target.Configured {
			delete(retained, service)
		}
	}
}

func (s *Server) pruneImageUpdateRetention(ctx context.Context, tx pgx.Tx, id string, config workloadInput) error {
	policy, retained, err := readRetainedImageUpdates(ctx, tx, id)
	if err != nil || len(retained) == 0 {
		return err
	}
	pruneRetainedImages(policy, retained, config)
	data, err := json.Marshal(retained)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE image_update_policies SET retained_images=$2 WHERE workload_id=$1`, id, data)
	return err
}

func applyRetainedImageUpdates(request *worker.Request, policy worker.ImageUpdatePolicy, retained map[string]retainedImageUpdate) (bool, error) {
	if request.Action != "deploy" || request.ManagedDatabase || request.Scale != nil || request.Preview || request.EnvironmentPreview != nil || !policy.Enabled || policy.Mode != "auto" {
		return false, nil
	}
	config := workloadInput{Kind: request.Kind, Source: request.Source, Compose: request.Compose, ComposeBundle: request.ComposeBundle}
	pruneRetainedImages(policy, retained, config)
	if len(retained) == 0 {
		return false, nil
	}
	pins := map[string]string{}
	for service, image := range retained {
		pins[service] = image.Image
	}
	if len(request.ImagePins) != 0 {
		for service, image := range pins {
			if request.ImagePins[service] != image {
				return false, &imageUpdateRetentionError{"Turn off the automatic image update policy before promoting a different retained release. Its applied image versions are still retained."}
			}
		}
		return false, nil
	}
	if len(pins) == len(imageUpdateConfiguredImages(config)) {
		request.ImagePins, request.PullLatest = pins, false
		return true, nil
	}
	// Partial policy retention must not relax promotion's exact full-service-map contract.
	bundle := &worker.ComposeBundle{}
	if request.ComposeBundle != nil {
		*bundle = *request.ComposeBundle
		bundle.Files = append([]worker.ComposeFile{}, bundle.Files...)
	}
	if len(bundle.Files) >= 7 {
		return false, &imageUpdateRetentionError{"Retaining automatic image versions needs one Compose override slot. Reduce the selected Compose files to seven or fewer, or turn off the automatic policy to deliberately deploy the configured tags."}
	}
	services := map[string]map[string]string{}
	for service, image := range pins {
		services[service] = map[string]string{"image": image, "pull_policy": "missing"}
	}
	content, err := yaml.Marshal(map[string]any{"services": services})
	if err != nil {
		return false, err
	}
	name := "cove-retained-images-" + request.ID + ".yaml"
	bundle.Files = append(bundle.Files, worker.ComposeFile{Path: name, Content: string(content)})
	if err := worker.ValidateComposeBundle(bundle, request.Source, request.Preview); err != nil {
		return false, &imageUpdateRetentionError{"Retaining automatic image versions needs space for a Compose override. Reduce the selected Compose files/assets or turn off the automatic policy to deliberately deploy the configured tags."}
	}
	request.ComposeBundle = bundle
	return true, nil
}

func (s *Server) retainImageUpdatesForRequest(ctx context.Context, tx pgx.Tx, request *worker.Request) (bool, error) {
	if request.Action != "deploy" {
		return false, nil
	}
	policy, retained, err := readRetainedImageUpdates(ctx, tx, request.Workload)
	if err != nil {
		return false, err
	}
	return applyRetainedImageUpdates(request, policy, retained)
}

func (s *Server) prepareRetainedImageUpdates(ctx context.Context, request *worker.Request) error {
	if request.Action != "deploy" {
		return nil
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id string
	if err = tx.QueryRow(ctx, `SELECT id FROM workloads WHERE id=$1 FOR UPDATE`, request.Workload).Scan(&id); err != nil {
		return err
	}
	var frozen, automatic bool
	if err = tx.QueryRow(ctx, `SELECT drift_prepared,EXISTS(SELECT 1 FROM image_update_deployments WHERE operation_id=o.id) FROM operations o WHERE id=$1`, request.ID).Scan(&frozen, &automatic); err != nil {
		return err
	}
	if frozen || automatic {
		return nil
	}
	changed, err := s.retainImageUpdatesForRequest(ctx, tx, request)
	if err != nil || !changed {
		return err
	}
	data, err := s.sealJSON(request)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE operations SET payload=$2 WHERE id=$1`, request.ID, data)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	return err
}

func (s *Server) recordAppliedImageUpdates(ctx context.Context, tx pgx.Tx, operation string) error {
	var id string
	var payload, configuration []byte
	err := tx.QueryRow(ctx, `SELECT o.workload_id,o.payload,w.configuration FROM operations o JOIN image_update_deployments d ON d.operation_id=o.id JOIN workloads w ON w.id=o.workload_id WHERE o.id=$1 AND o.state='succeeded' AND COALESCE(o.result->'rollout'->>'phase','') NOT IN ('waiting','aborted')`, operation).Scan(&id, &payload, &configuration)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	policy, retained, err := readRetainedImageUpdates(ctx, tx, id)
	if err != nil || !policy.Enabled || policy.Mode != "auto" {
		return err
	}
	var request worker.Request
	var config workloadInput
	if err = s.unsealJSON(payload, &request); err != nil {
		return err
	}
	if err = s.unsealJSON(configuration, &config); err != nil {
		return err
	}
	pruneRetainedImages(policy, retained, config)
	current := imageUpdateConfiguredImages(config)
	baseline := imageUpdateConfiguredImages(workloadInput{Kind: request.Kind, Compose: request.Compose, ComposeBundle: request.ComposeBundle, Source: request.Source})
	for _, rule := range policy.Rules {
		target := request.ImagePins[rule.Service]
		if strings.Contains(target, "@sha256:") && baseline[rule.Service] != "" && baseline[rule.Service] == current[rule.Service] {
			retained[rule.Service] = retainedImageUpdate{Configured: baseline[rule.Service], Image: target}
		}
	}
	data, err := json.Marshal(retained)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE image_update_policies SET retained_images=$2 WHERE workload_id=$1`, id, data)
	}
	return err
}

func (s *Server) commitImageUpdateOutcome(ctx context.Context, request worker.Request, result []byte) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id string
	if err = tx.QueryRow(ctx, `SELECT id FROM workloads WHERE id=$1 FOR UPDATE`, request.Workload).Scan(&id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE operations SET state='succeeded',result=$2,updated_at=now() WHERE id=$1`, request.ID, result); err != nil {
		return err
	}
	if err = s.recordAppliedImageUpdates(ctx, tx, request.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func pauseImageUpdatesForRollback(ctx context.Context, tx pgx.Tx, workload string) (bool, error) {
	result, err := tx.Exec(ctx, `UPDATE image_update_policies SET policy=jsonb_set(policy,'{enabled}','false'),retained_images='{}',last_error='Automatic image updates were turned off by a rollback request. Turn the policy on and save to resume automatic updates.',updated_at=now() WHERE workload_id=$1 AND policy->>'enabled'='true' AND policy->>'mode'='auto'`, workload)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() != 0, nil
}

func missingRetainedImageTargets(pins map[string]string, retained map[string]retainedImageUpdate) bool {
	for service, target := range pins {
		if strings.Contains(target, "@sha256:") && retained[service].Image != target {
			return true
		}
	}
	return false
}
