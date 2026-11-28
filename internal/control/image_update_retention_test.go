package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func imageRetentionFixture(t *testing.T) (*registryHookFixture, worker.ImageUpdatePolicy) {
	t.Helper()
	f, policy := mergedImagePolicyFixture(t, "auto")
	f.sql(`UPDATE nodes SET capabilities=capabilities || '{"exact_promotion":true,"rollouts":true,"workload_moves":true}'::jsonb WHERE id='node'`)
	return f, policy
}

func imageRetentionCandidate(t *testing.T, f *registryHookFixture, pins map[string]string) string {
	t.Helper()
	tx, err := f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	request := worker.Request{ID: resourceID(), Workload: f.id, Kind: "service", Action: "image_check"}
	if err := f.s.prepareImageUpdateCheck(t.Context(), tx, &request); err != nil || request.ImageUpdates == nil {
		t.Fatal("image policy check", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	check, err := f.s.sealJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	candidates := []worker.ImageUpdateCandidate{}
	for service, image := range pins {
		if strings.Contains(image, "@") {
			candidates = append(candidates, worker.ImageUpdateCandidate{Service: service, Current: request.ImageUpdates.Images[service], Target: image})
		}
	}
	result, _ := json.Marshal(worker.Operation{ImageCheck: &worker.ImageCheck{Updates: &worker.ImageUpdateResult{State: "available", Key: strings.Repeat("b", 64), Candidates: candidates, Pins: pins}}})
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES($1,$2,'image_check','succeeded',$3,$4)`, request.ID, f.id, check, result)
	f.sql(`UPDATE image_update_policies SET next_dispatch_at=now() WHERE workload_id=$1`, f.id)
	if err := f.s.dispatchImageUpdates(t.Context()); err != nil {
		t.Fatal(err)
	}
	var operation string
	if err := f.s.DB.QueryRow(t.Context(), `SELECT last_operation_id FROM image_update_policies WHERE workload_id=$1`, f.id).Scan(&operation); err != nil {
		t.Fatal("automatic deployment was not queued", err)
	}
	return operation
}

func imageRetentionProcess(t *testing.T, f *registryHookFixture, id string, report worker.Operation) (worker.Request, string) {
	t.Helper()
	var submitted worker.Request
	var destination string
	f.s.workerRPC = func(_ context.Context, node string, rpc worker.RPCRequest) (worker.RPCResponse, error) {
		if rpc.Request == nil || !strings.HasPrefix(rpc.Method, "submit") {
			t.Fatal("unexpected worker call", rpc.Method)
		}
		submitted, destination = *rpc.Request, node
		report.ID, report.Workload, report.Action = rpc.Request.ID, rpc.Request.Workload, rpc.Request.Action
		return worker.RPCResponse{Operation: &report}, nil
	}
	var payload []byte
	var node string
	if err := f.s.DB.QueryRow(t.Context(), `SELECT o.payload,w.node_id FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE o.id=$1`, id).Scan(&payload, &node); err != nil {
		t.Fatal(err)
	}
	if err := f.s.processOperation(t.Context(), id, node, payload); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.s.DB.QueryRow(t.Context(), `SELECT state FROM operations WHERE id=$1`, id).Scan(&state); err != nil || state != report.State || submitted.ID != id {
		t.Fatal("operation not submitted/completed", state, submitted.ID, err)
	}
	return submitted, destination
}

func imageRetentionTarget() string {
	return "registry.example.test/team/app:v1.12.0@sha256:" + strings.Repeat("a", 64)
}

func TestImageUpdateAppliedVersionSurvivesOrdinaryDeploy(t *testing.T) {
	f, _ := imageRetentionFixture(t)
	target := imageRetentionTarget()
	automatic := imageRetentionCandidate(t, f, map[string]string{"app": target})
	imageRetentionProcess(t, f, automatic, worker.Operation{State: "succeeded"})
	id := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "deploy", "pull_latest": true}, 202), "id")
	var encrypted []byte
	if err := f.s.DB.QueryRow(t.Context(), `SELECT payload FROM operations WHERE id=$1`, id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	var queued worker.Request
	if err := f.s.unsealJSON(encrypted, &queued); err != nil {
		t.Fatal(err)
	}
	request, _ := imageRetentionProcess(t, f, id, worker.Operation{State: "succeeded"})
	if request.ImagePins["app"] != target {
		t.Fatal("ordinary deployment discarded the successfully applied automatic version", request.ImagePins)
	}
	if request.PullLatest {
		t.Fatal("retained full-map deployment still resolved mutable configured tags")
	}
	if err := f.s.DB.QueryRow(t.Context(), `SELECT payload FROM operations WHERE id=$1`, id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if err := f.s.unsealJSON(encrypted, &queued); err != nil || queued.ImagePins["app"] != target {
		t.Fatal("selected images were not frozen before worker submission", queued.ImagePins, err)
	}
	var configuration struct{ Compose string }
	if err := json.Unmarshal(f.call("root", "default", "GET", f.path, nil, 200).Body.Bytes(), &configuration); err != nil || !strings.Contains(configuration.Compose, "app:stable") || strings.Contains(configuration.Compose, "v1.12.0") {
		t.Fatal("automatic retention rewrote the configured image", configuration, err)
	}
}

func TestImageUpdateRetentionWaitsForConfirmedSuccess(t *testing.T) {
	for _, state := range []string{"queued", "running", "failed", "cancelled", "interrupted"} {
		t.Run(state, func(t *testing.T) {
			f, _ := imageRetentionFixture(t)
			automatic := imageRetentionCandidate(t, f, map[string]string{"app": imageRetentionTarget()})
			imageRetentionProcess(t, f, automatic, worker.Operation{State: state})
			var status struct {
				Retained map[string]json.RawMessage `json:"retained_images"`
			}
			if err := json.Unmarshal(f.call("root", "default", "GET", f.path+"/image-update-policy", nil, 200).Body.Bytes(), &status); err != nil || len(status.Retained) != 0 {
				t.Fatal("unconfirmed candidate became retained", status, err)
			}
		})
	}
}

func TestImageUpdateRetentionWaitsForRolloutPromotion(t *testing.T) {
	f, _ := imageRetentionFixture(t)
	automatic := imageRetentionCandidate(t, f, map[string]string{"app": imageRetentionTarget()})
	imageRetentionProcess(t, f, automatic, worker.Operation{State: "succeeded", Rollout: &worker.RolloutStatus{Phase: "waiting", Release: automatic}})
	var status struct {
		Retained map[string]json.RawMessage `json:"retained_images"`
	}
	if err := json.Unmarshal(f.call("root", "default", "GET", f.path+"/image-update-policy", nil, 200).Body.Bytes(), &status); err != nil || len(status.Retained) != 0 {
		t.Fatal("held rollout became retained", status, err)
	}
	id := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "rollout_promote"}, 202), "id")
	imageRetentionProcess(t, f, id, worker.Operation{State: "succeeded", Rollout: &worker.RolloutStatus{Phase: "completed", Release: automatic}})
	if err := json.Unmarshal(f.call("root", "default", "GET", f.path+"/image-update-policy", nil, 200).Body.Bytes(), &status); err != nil || len(status.Retained) != 1 {
		t.Fatal("promoted automatic release was not retained", status, err)
	}
}

func TestImageUpdateExplicitTagChangeWinsOnlyForThatService(t *testing.T) {
	f, policy := imageRetentionFixture(t)
	config := workloadInput{Name: "App", NodeID: "node", Kind: "service", Compose: "services:\n  app:\n    image: registry.example.test/team/app:stable\n  other:\n    image: registry.example.test/team/other:stable\n"}
	f.call("root", "default", "PUT", f.path, config, 200)
	policy.Rules = append(policy.Rules, worker.ImageUpdateRule{Service: "other"})
	f.call("root", "default", "PUT", f.path+"/image-update-policy", policy, 200)
	other := "registry.example.test/team/other:v1.12.0@sha256:" + strings.Repeat("c", 64)
	automatic := imageRetentionCandidate(t, f, map[string]string{"app": imageRetentionTarget(), "other": other})
	imageRetentionProcess(t, f, automatic, worker.Operation{State: "succeeded"})
	policy.Rules = policy.Rules[1:]
	f.call("root", "default", "PUT", f.path+"/image-update-policy", policy, 200)
	var retained struct {
		Images map[string]json.RawMessage `json:"retained_images"`
	}
	if err := json.Unmarshal(f.call("root", "default", "GET", f.path+"/image-update-policy", nil, 200).Body.Bytes(), &retained); err != nil || len(retained.Images) != 2 {
		t.Fatal("stopping future discovery reverted an already applied service", retained, err)
	}
	config.Compose = strings.Replace(config.Compose, "app:stable", "app:v1.11.0", 1)
	f.call("root", "default", "PUT", f.path, config, 200)
	// Changing away and back is still an explicit choice, not permission to resurrect the old pin.
	config.Compose = strings.Replace(config.Compose, "app:v1.11.0", "app:stable", 1)
	f.call("root", "default", "PUT", f.path, config, 200)
	id := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "deploy", "pull_latest": true}, 202), "id")
	request, _ := imageRetentionProcess(t, f, id, worker.Operation{State: "succeeded"})
	if len(request.ImagePins) != 0 || !request.PullLatest || request.Compose != config.Compose || request.ComposeBundle == nil || len(request.ComposeBundle.Files) != 1 {
		t.Fatal("partial retention changed the explicit configuration", request)
	}
	var overlay struct {
		Services map[string]struct{ Image string }
	}
	if err := yaml.Unmarshal([]byte(request.ComposeBundle.Files[0].Content), &overlay); err != nil || len(overlay.Services) != 1 || overlay.Services["other"].Image != other {
		t.Fatal("retention lost the other service or repinned the changed service", overlay, err)
	}
	var declared []byte
	if err := f.s.DB.QueryRow(t.Context(), `SELECT declared_payload FROM operations WHERE id=$1`, id).Scan(&declared); err != nil {
		t.Fatal(err)
	}
	var declaration worker.Request
	if err := f.s.unsealJSON(declared, &declaration); err != nil || declaration.ComposeBundle != nil {
		t.Fatal("internal retention override leaked into the user's declared configuration", declaration.ComposeBundle, err)
	}
	config.ComposeBundle = &worker.ComposeBundle{}
	for i := 0; i < 7; i++ {
		config.ComposeBundle.Files = append(config.ComposeBundle.Files, worker.ComposeFile{Path: string(rune('a'+i)) + ".yaml", Content: "services: {}\n"})
	}
	f.call("root", "default", "PUT", f.path, config, 200)
	response := f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "deploy"}, 409)
	if !strings.Contains(response.Body.String(), "Reduce the selected Compose files to seven or fewer") {
		t.Fatal("full bundle refusal is not actionable", response.Body.String())
	}
}

func TestImageUpdateRollbackPausesPolicyAtomically(t *testing.T) {
	f, policy := imageRetentionFixture(t)
	automatic := imageRetentionCandidate(t, f, map[string]string{"app": imageRetentionTarget()})
	imageRetentionProcess(t, f, automatic, worker.Operation{State: "succeeded"})
	f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "rollback", "release": "missing"}, 400)
	var status struct {
		Configuration worker.ImageUpdatePolicy   `json:"configuration"`
		Retained      map[string]json.RawMessage `json:"retained_images"`
		Message       string                     `json:"last_error"`
	}
	read := func() {
		t.Helper()
		status.Retained = nil
		if err := json.Unmarshal(f.call("root", "default", "GET", f.path+"/image-update-policy", nil, 200).Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if !status.Configuration.Enabled || len(status.Retained) != 1 {
		t.Fatal("rejected rollback changed the policy", status)
	}
	f.call("root", "default", "PUT", "/api/projects/project/deployment-policy", deploymentPolicy{Approval: true}, 200)
	id := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "rollback", "release": automatic, "reason": "Choose this release"}, 202), "id")
	read()
	if status.Configuration.Enabled || len(status.Retained) != 0 || !strings.Contains(status.Message, "Turn the policy on and save") {
		t.Fatal("queued rollback did not explicitly pause automatic updates", status)
	}
	var reason string
	var audited bool
	if err := f.s.DB.QueryRow(t.Context(), `SELECT reason,EXISTS(SELECT 1 FROM audit_events WHERE subject=o.id AND details->>'reason' LIKE '%Rollback paused automatic image updates.%') FROM operations o WHERE id=$1`, id).Scan(&reason, &audited); err != nil || !strings.Contains(reason, "Choose this release") || !strings.Contains(reason, "Rollback paused automatic image updates.") || !audited {
		t.Fatal("rollback pause is not recorded in its reason and audit", reason, audited, err)
	}
	if admitted, err := f.s.admitDeployment(t.Context(), id); err != nil || admitted {
		t.Fatal("rollback did not wait for approval", admitted, err)
	}
	if err := f.s.failOperation(t.Context(), id, "Fixture rollback failure while awaiting approval"); err != nil {
		t.Fatal(err)
	}
	read()
	if status.Configuration.Enabled {
		t.Fatal("failed rollback silently resumed automatic updates")
	}
	f.call("root", "default", "PUT", f.path+"/image-update-policy", policy, 200)
	read()
	if !status.Configuration.Enabled || status.Message != "" {
		t.Fatal("one explicit policy save did not resume", status)
	}
}

func TestImageUpdateMissingRetentionReevaluatesConsumedCandidate(t *testing.T) {
	f, _ := imageRetentionFixture(t)
	pins := map[string]string{"app": imageRetentionTarget()}
	first := imageRetentionCandidate(t, f, pins)
	imageRetentionProcess(t, f, first, worker.Operation{State: "failed"})
	second := imageRetentionCandidate(t, f, pins)
	if second == first {
		t.Fatal("missing retained target left the consumed candidate permanently ineligible")
	}
	imageRetentionProcess(t, f, second, worker.Operation{State: "succeeded"})
	if third := imageRetentionCandidate(t, f, pins); third != second {
		t.Fatal("a retained consumed candidate was repeatedly deployed", third, second)
	}
}

func TestImageUpdateRetentionSurvivesHistoryPruningAndNodeMove(t *testing.T) {
	f, _ := imageRetentionFixture(t)
	target := imageRetentionTarget()
	automatic := imageRetentionCandidate(t, f, map[string]string{"app": target})
	imageRetentionProcess(t, f, automatic, worker.Operation{State: "succeeded"})
	f.sql(`DELETE FROM operations WHERE id=$1`, automatic)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) SELECT 'destination','Destination','destination.invalid',22,'',private_key,'ready',capabilities FROM nodes WHERE id='node'`)
	f.sql(`INSERT INTO team_nodes(team_id,node_id) VALUES('default','destination')`)
	var configuration []byte
	if err := f.s.DB.QueryRow(t.Context(), `SELECT configuration FROM workloads WHERE id=$1`, f.id).Scan(&configuration); err != nil {
		t.Fatal(err)
	}
	intent := workloadMoveIntent{Action: "move", Configuration: configuration, SourceDaemon: "node-daemon", TargetDaemon: "destination-daemon"}
	payload, err := f.s.sealJSON(intent)
	if err != nil {
		t.Fatal(err)
	}
	move := resourceID()
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload) VALUES($1,$2,'move','running',$3)`, move, f.id, payload)
	f.sql(`INSERT INTO workload_moves(operation_id,source_node,target_node,phase) VALUES($1,'node','destination','committing')`, move)
	f.s.workerRPC = func(_ context.Context, node string, rpc worker.RPCRequest) (worker.RPCResponse, error) {
		if rpc.Move == nil || rpc.Move.Daemon != node+"-daemon" {
			t.Fatal("move lost its Docker daemon identity", node, rpc.Method)
		}
		return worker.RPCResponse{Move: &worker.MoveState{ID: rpc.Move.ID, Workload: rpc.Move.Workload, Phase: "started"}}, nil
	}
	for i := 0; i < 3; i++ {
		if err := f.s.processWorkloadMove(t.Context(), move, intent); err != nil {
			t.Fatal(err)
		}
	}
	id := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "deploy"}, 202), "id")
	request, destination := imageRetentionProcess(t, f, id, worker.Operation{State: "succeeded"})
	if destination != "destination" || request.ImagePins["app"] != target {
		t.Fatal("move or history pruning discarded automatic image intent", destination, request.ImagePins)
	}
}

func TestImageUpdateWebhookKeepsRetainedTarget(t *testing.T) {
	f, _ := imageRetentionFixture(t)
	target := imageRetentionTarget()
	automatic := imageRetentionCandidate(t, f, map[string]string{"app": target})
	imageRetentionProcess(t, f, automatic, worker.Operation{State: "succeeded"})
	input := map[string]any{"provider": "distribution", "enabled": true, "filters": []registryWebhookFilter{{Repository: "team/app", Tag: "stable"}}}
	var hook registryWebhookStatus
	if err := json.Unmarshal(f.call("root", "default", "PUT", f.path+"/registry-webhook", input, 201).Body.Bytes(), &hook); err != nil {
		t.Fatal(err)
	}
	f.token = hook.Token
	endpoint, err := url.Parse(*hook.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	f.endpoint = endpoint.Path
	f.send(distributionPush("retained"), "distribution", "", 202)
	if err := f.s.dispatchRegistryWebhooks(t.Context()); err != nil {
		t.Fatal(err)
	}
	var deploys int
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM operations WHERE workload_id=$1 AND action='deploy'`, f.id).Scan(&deploys); err != nil || deploys != 1 {
		t.Fatal("webhook bypassed policy image selection", deploys, err)
	}
	if !strings.Contains(f.status().Ignored, "fresh check") {
		t.Fatal("webhook did not defer to the enabled policy", f.status())
	}
	id := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "deploy"}, 202), "id")
	request, _ := imageRetentionProcess(t, f, id, worker.Operation{State: "succeeded"})
	if request.ImagePins["app"] != target {
		t.Fatal("webhook erased the retained automatic version", request.ImagePins)
	}
}

func TestImageUpdateRetentionIsFrozenAcrossLostAcknowledgement(t *testing.T) {
	f, policy := imageRetentionFixture(t)
	target := imageRetentionTarget()
	automatic := imageRetentionCandidate(t, f, map[string]string{"app": target})
	imageRetentionProcess(t, f, automatic, worker.Operation{State: "succeeded"})
	id := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "deploy"}, 202), "id")
	submitted := false
	f.s.workerRPC = func(_ context.Context, _ string, rpc worker.RPCRequest) (worker.RPCResponse, error) {
		if rpc.Request == nil || rpc.Request.ImagePins["app"] != target {
			t.Fatal("first submission did not freeze the applied version")
		}
		submitted = true
		return worker.RPCResponse{}, errors.New("injected lost submission acknowledgement")
	}
	var payload []byte
	if err := f.s.DB.QueryRow(t.Context(), `SELECT payload FROM operations WHERE id=$1`, id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if err := f.s.processOperation(t.Context(), id, "node", payload); err != nil || !submitted {
		t.Fatal("lost acknowledgement boundary was not reached", err)
	}
	policy.Enabled = false
	f.call("root", "default", "PUT", f.path+"/image-update-policy", policy, 200)
	request, _ := imageRetentionProcess(t, f, id, worker.Operation{State: "succeeded"})
	if request.ImagePins["app"] != target {
		t.Fatal("ambiguous submission selected different images after retention was cleared", request.ImagePins)
	}
}

func TestImageUpdateRetentionInvalidationAtComposeWriteBoundaries(t *testing.T) {
	for _, boundary := range []string{"bundle", "draft"} {
		t.Run(boundary, func(t *testing.T) {
			f, _ := imageRetentionFixture(t)
			automatic := imageRetentionCandidate(t, f, map[string]string{"app": imageRetentionTarget()})
			imageRetentionProcess(t, f, automatic, worker.Operation{State: "succeeded"})
			if boundary == "bundle" {
				f.call("root", "default", "PUT", f.path+"/compose-bundle", map[string]any{"bundle": &worker.ComposeBundle{Files: []worker.ComposeFile{{Path: "override.yaml", Content: "services:\n  app:\n    image: registry.example.test/team/app:v1.11.0\n"}}}}, 200)
				f.call("root", "default", "PUT", f.path+"/compose-bundle", map[string]any{"bundle": nil}, 200)
			} else {
				for _, tag := range []string{"v1.11.0", "stable"} {
					var current struct {
						Manifest projectConfiguration `json:"manifest"`
					}
					path := "/api/projects/project/configuration"
					if err := json.Unmarshal(f.call("root", "default", "GET", path, nil, 200).Body.Bytes(), &current); err != nil || len(current.Manifest.Resources) != 1 {
						t.Fatal("project configuration", current, err)
					}
					current.Manifest.Resources[0].Configuration.Compose = "services:\n  app:\n    image: registry.example.test/team/app:" + tag + "\n"
					content, err := json.Marshal(current.Manifest)
					if err != nil {
						t.Fatal(err)
					}
					draft := jsonString(t, f.call("root", "default", "POST", path+"/drafts", map[string]string{"content": string(content)}, 201), "id")
					f.call("root", "default", "POST", path+"/drafts/"+draft+"/apply", nil, 200)
				}
			}
			id := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "deploy"}, 202), "id")
			request, _ := imageRetentionProcess(t, f, id, worker.Operation{State: "succeeded"})
			if len(request.ImagePins) != 0 || request.ComposeBundle != nil {
				t.Fatal("explicit tag change resurrected an old automatic version", boundary, request.ImagePins, request.ComposeBundle)
			}
		})
	}
}

func TestImageUpdateCPUScaleKeepsPolicyAndRetention(t *testing.T) {
	f, _ := imageRetentionFixture(t)
	automatic := imageRetentionCandidate(t, f, map[string]string{"app": imageRetentionTarget()})
	imageRetentionProcess(t, f, automatic, worker.Operation{State: "succeeded"})
	var encrypted []byte
	if err := f.s.DB.QueryRow(t.Context(), `SELECT configuration FROM workloads WHERE id=$1`, f.id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	var config workloadInput
	if err := f.s.unsealJSON(encrypted, &config); err != nil {
		t.Fatal(err)
	}
	config.Rollout = &worker.RolloutOptions{Strategy: "rolling", Replicas: map[string]int{"app": 1}, TimeoutSeconds: 30, DrainSeconds: 1}
	encrypted, err := f.s.sealJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE workloads SET configuration=$2 WHERE id=$1`, f.id, encrypted)
	request := worker.Request{ID: resourceID(), Workload: f.id, Kind: "service", Action: "deploy", Rollout: config.Rollout, Scale: &worker.CPUScaleOptions{Generation: automatic, Before: map[string]int{"app": 1}, Replicas: map[string]int{"app": 2}}}
	payload, err := f.s.sealJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'deploy',$3)`, request.ID, f.id, payload)
	imageRetentionProcess(t, f, request.ID, worker.Operation{State: "succeeded", Scale: request.Scale, Rollout: &worker.RolloutStatus{Phase: "completed", Replicas: request.Scale.Replicas}})
	var status struct {
		Policy worker.ImageUpdatePolicy   `json:"configuration"`
		Images map[string]json.RawMessage `json:"retained_images"`
	}
	if err := json.Unmarshal(f.call("root", "default", "GET", f.path+"/image-update-policy", nil, 200).Body.Bytes(), &status); err != nil || !status.Policy.Enabled || len(status.Images) != 1 {
		t.Fatal("CPU scaling was treated as a user rollback or discarded retention", status, err)
	}
}

func TestImageUpdateDisableAndDeleteClearRetention(t *testing.T) {
	for _, action := range []string{"disable", "notify", "delete"} {
		t.Run(action, func(t *testing.T) {
			f, policy := imageRetentionFixture(t)
			automatic := imageRetentionCandidate(t, f, map[string]string{"app": imageRetentionTarget()})
			imageRetentionProcess(t, f, automatic, worker.Operation{State: "succeeded"})
			if action == "delete" {
				f.sql(`DELETE FROM image_update_policies WHERE workload_id=$1`, f.id)
			} else {
				if action == "disable" {
					policy.Enabled = false
				} else {
					policy.Mode = "notify"
				}
				f.call("root", "default", "PUT", f.path+"/image-update-policy", policy, 200)
			}
			id := jsonString(t, f.call("root", "default", "POST", f.path+"/operations", map[string]any{"action": "deploy"}, 202), "id")
			request, _ := imageRetentionProcess(t, f, id, worker.Operation{State: "succeeded"})
			if len(request.ImagePins) != 0 || request.ComposeBundle != nil {
				t.Fatal("policy no longer controls the images but retention remained", request)
			}
		})
	}
}
