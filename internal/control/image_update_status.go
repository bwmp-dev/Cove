package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"strings"
)

func markRetainedImageUpdatesApplied(status map[string]any, images map[string]string) bool {
	policy, ok := status["configuration"].(worker.ImageUpdatePolicy)
	if !ok || !policy.Enabled || policy.Mode != "auto" {
		return false
	}
	retained, ok := status["retained_images"].(map[string]retainedImageUpdate)
	if !ok || len(retained) == 0 {
		return false
	}
	raw, ok := status["updates"].(json.RawMessage)
	if !ok {
		return false
	}
	var updates worker.ImageUpdateResult
	if json.Unmarshal(raw, &updates) != nil || updates.State != "available" || len(updates.Candidates) == 0 || len(updates.Pins) != len(images) {
		return false
	}
	for service, pin := range updates.Pins {
		if images[service] == "" || pin == "" {
			return false
		}
	}
	seen := map[string]bool{}
	for _, candidate := range updates.Candidates {
		image := retained[candidate.Service]
		if seen[candidate.Service] || images[candidate.Service] == "" || image.Configured != images[candidate.Service] || image.Image != candidate.Target || updates.Pins[candidate.Service] != candidate.Target || !strings.Contains(candidate.Target, "@sha256:") {
			return false
		}
		seen[candidate.Service] = true
	}
	status["updates"] = worker.ImageUpdateResult{State: "current", Message: "The retained automatic image versions are applied.", Candidates: []worker.ImageUpdateCandidate{}}
	return true
}

func (s *Server) commitImagePolicyCheck(ctx context.Context, request worker.Request, result []byte) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE operations SET state='succeeded',result=$2,updated_at=now() WHERE id=$1`, request.ID, result); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE image_update_policies SET next_dispatch_at=now() WHERE workload_id=$1 AND generation=$2 AND policy->>'enabled'='true'`, request.Workload, request.ImageUpdates.Generation); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
