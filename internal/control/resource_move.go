package control

import (
	"cove/internal/worker"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

type resourceMoveInput struct {
	TeamID        string `json:"team_id"`
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	Version       string `json:"version,omitempty"`
	Acknowledge   bool   `json:"acknowledge,omitempty"`
}

type resourceMoveReview struct {
	Version   string   `json:"version"`
	Blockers  []string `json:"blockers"`
	Effects   []string `json:"effects"`
	CrossTeam bool     `json:"cross_team"`
}

func (s *Server) moveResource(w http.ResponseWriter, r *http.Request) {
	var in resourceMoveInput
	if !decode(w, r, &in) {
		return
	}
	if in.TeamID == "" || in.ProjectID == "" {
		http.Error(w, "team_id and project_id are required", 400)
		return
	}
	id, p, ctx := r.PathValue("id"), actor(r), r.Context()
	if credential, ok := ctx.Value(apiCredentialKey{}).(apiCredential); ok && credential.Team != "" && credential.Team != in.TeamID {
		http.Error(w, "a team-scoped token cannot transfer a resource to another team", 403)
		return
	}
	if !s.requireAccess(w, r, "workload", id, 3) {
		return
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		http.Error(w, "move unavailable", 503)
		return
	}
	defer tx.Rollback(ctx)
	var team, project, environment, node string
	var sealed []byte
	err = tx.QueryRow(ctx, `SELECT team_id,project_id,COALESCE(environment_id,''),node_id,configuration FROM workloads WHERE id=$1 AND team_id=$2 FOR UPDATE`, id, p.Team).Scan(&team, &project, &environment, &node, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "move unavailable", 503)
		return
	}
	var targetLevel, sourceLevel int
	err = tx.QueryRow(ctx, `SELECT CASE WHEN $1 THEN 3 ELSE cove_access($2,w.team_id,w.project_id,COALESCE(w.environment_id,'')) END FROM workloads w WHERE id=$3`, p.Admin, p.ID, id).Scan(&sourceLevel)
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT CASE WHEN $1 THEN 3 ELSE cove_access($2,p.team_id,p.id,$5) END FROM projects p WHERE p.id=$3 AND p.team_id=$4 AND ($5='' OR EXISTS(SELECT 1 FROM environments e WHERE e.id=$5 AND e.project_id=p.id AND e.team_id=p.team_id))`, p.Admin, p.ID, in.ProjectID, in.TeamID, in.EnvironmentID).Scan(&targetLevel)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "authorization unavailable", 503)
		return
	}
	if sourceLevel < 3 || targetLevel < 3 || team != in.TeamID && (!s.teamAdmin(r, team) || !s.teamAdmin(r, in.TeamID)) {
		http.Error(w, "moving resources requires edit access on both sides and team administration on both teams for a transfer", 403)
		return
	}
	var assigned bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM team_nodes WHERE team_id=$1 AND node_id=$2)`, in.TeamID, node).Scan(&assigned); err != nil {
		http.Error(w, "move unavailable", 503)
		return
	}
	if !assigned && !p.Admin {
		http.Error(w, "the node must be assigned to the destination team", 403)
		return
	}
	raw, err := unseal(s.Key, sealed)
	var config workloadInput
	if err != nil || json.Unmarshal(raw, &config) != nil {
		http.Error(w, "configuration unavailable", 503)
		return
	}
	review := resourceMoveReview{Blockers: []string{}, Effects: []string{
		"The resource ID, node, containers, saved local variables, routes and operation history are retained. No deployment is queued.",
		"Access is inherited from the destination. Source grants are not copied. Destination shared variables, built-in placement variables and deployment policies apply to future deployments.",
	}, CrossTeam: team != in.TeamID}
	if team == in.TeamID && project == in.ProjectID && environment == in.EnvironmentID {
		review.Blockers = append(review.Blockers, "Choose a different destination.")
	}
	checks := []struct{ query, message string }{
		{`SELECT EXISTS(SELECT 1 FROM environments WHERE id IN ($2,$3) AND paused) OR EXISTS(SELECT 1 FROM environment_suspensions WHERE workload_id=$1)`, "Finish resuming both environments and the resource before moving."},
		{`SELECT COALESCE((SELECT result->'rollout'->>'phase'='waiting' FROM operations WHERE workload_id=$1 AND action IN ('deploy','rollback','rollout_promote','rollout_abort') AND state='succeeded' ORDER BY updated_at DESC,id DESC LIMIT 1),false)`, "Promote or abort the staged rollout before moving."},
		{`SELECT observed_idle_state<>'awake' FROM workloads WHERE id=$1`, "Wake the resource and refresh its observed state before moving."},
		{`SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running','awaiting_approval'))`, "Finish or cancel pending operations before moving."},
		{`SELECT EXISTS(SELECT 1 FROM previews WHERE workload_id=$1 OR source_id=$1) OR EXISTS(SELECT 1 FROM environment_preview_resources WHERE resource_id=$1 OR base_id=$1)`, "Close active previews that use this resource before moving."},
		{`SELECT EXISTS(SELECT 1 FROM environment_previews WHERE environment_id=$2 OR base_environment_id=$2) OR EXISTS(SELECT 1 FROM environment_previews WHERE environment_id=$3) OR EXISTS(SELECT 1 FROM preview_recipes WHERE base_environment_id=$2 AND enabled)`, "Close dependent environment previews and disable the source environment's preview recipe before moving; preview environments cannot be selected."},
		{`SELECT EXISTS(SELECT 1 FROM configuration_gitops WHERE workload_id=$1)`, "Disconnect this resource as a GitOps source before moving."},
		{`SELECT EXISTS(SELECT 1 FROM database_access WHERE workload_id=$1 AND expires_at>now())`, "Wait for temporary database access leases to expire before moving."},
		{`SELECT EXISTS(SELECT 1 FROM resource_variable_bindings WHERE workload_id=$1 OR source_id=$1)`, "Remove incoming and outgoing resource variable bindings before moving; preserve required values as saved local variables."},
		{`SELECT EXISTS(SELECT 1 FROM resource_connections WHERE source_id=$1 OR target_id=$1) OR EXISTS(SELECT 1 FROM workload_networks WHERE workload_id=$1)`, "Detach private connections and shared networks before moving."},
		{`SELECT EXISTS(SELECT 1 FROM deployment_sequences WHERE workload_ids ? $1)`, "Remove this resource from its deployment sequence before moving."},
	}
	if review.CrossTeam {
		checks = append(checks, struct{ query, message string }{`SELECT EXISTS(SELECT 1 FROM workload_connections WHERE workload_id=$1) OR EXISTS(SELECT 1 FROM github_sources WHERE workload_id=$1) OR EXISTS(SELECT 1 FROM git_sources WHERE workload_id=$1)`, "Disconnect team-owned registry, storage and Git provider connections before transferring teams."})
		review.Effects = append(review.Effects, "Resource API tokens, deploy hooks and webhook secrets are revoked. Existing SSH deploy keys and saved credentials transfer with the resource; remove credentials that must remain with the source team first.")
	}
	for _, check := range checks {
		var blocked bool
		query := `WITH move_parameters AS (SELECT $1::text AS id,$2::text AS source_environment,$3::text AS target_environment) ` + check.query
		if err = tx.QueryRow(ctx, query, id, environment, in.EnvironmentID).Scan(&blocked); err != nil {
			http.Error(w, "move dependencies unavailable", 503)
			return
		}
		if blocked {
			review.Blockers = append(review.Blockers, check.message)
		}
	}
	var previewConfiguration []byte
	err = tx.QueryRow(ctx, `SELECT configuration FROM preview_policies WHERE workload_id=$1`, id).Scan(&previewConfiguration)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "preview policy unavailable", 503)
		return
	}
	if err == nil {
		var policy previewPolicy
		if s.unsealJSON(previewConfiguration, &policy) != nil {
			http.Error(w, "preview policy unavailable", 503)
			return
		}
		if policy.Enabled {
			review.Blockers = append(review.Blockers, "Disable the resource preview policy before moving.")
		}
	}
	for _, value := range config.Environment {
		if sharedReference.MatchString(value) {
			review.Blockers = append(review.Blockers, "Replace shared variable references with explicitly saved values before moving.")
			break
		}
	}
	if review.CrossTeam {
		for _, value := range config.Environment {
			if strings.Contains(value, "{{secret.") {
				review.Blockers = append(review.Blockers, "Replace external secret references or remove them before transferring teams; provider credentials are not transferred.")
				break
			}
		}
		if config.Private != nil || config.Backup != nil {
			review.Blockers = append(review.Blockers, "Disable private endpoints and backups before transferring teams, then configure destination credentials explicitly.")
		}
		var deployedPayload []byte
		err = tx.QueryRow(ctx, `SELECT o.payload FROM cove_committed_deployments d JOIN operations o ON o.id=d.declaration_id WHERE d.workload_id=$1 ORDER BY d.updated_at DESC,d.operation_id DESC LIMIT 1`, id).Scan(&deployedPayload)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "deployment state unavailable", 503)
			return
		}
		if err == nil {
			var deployed worker.Request
			if s.unsealJSON(deployedPayload, &deployed) != nil {
				review.Blockers = append(review.Blockers, "The deployed network state is unknown; complete a successful deployment before transferring teams.")
			} else if deployed.Private != nil || len(deployed.Networks) > 0 {
				review.Blockers = append(review.Blockers, "Deploy after disabling private endpoints and detaching shared networks so their removal is applied on the worker before transferring teams.")
			}
		}
	}
	// The reviewed destination declaration includes policies and shared values, without exposing them in the response.
	target, err := s.readProjectConfiguration(ctx, tx, in.TeamID, in.ProjectID)
	if err != nil {
		http.Error(w, "destination configuration unavailable", 503)
		return
	}
	input := in
	input.Version = ""
	input.Acknowledge = false
	digest := hmac.New(sha256.New, s.Key)
	versionData, _ := json.Marshal(struct {
		Input                              resourceMoveInput
		Source                             []byte
		Team, Project, Environment, Target string
		Blockers                           []string
	}{input, sealed, team, project, environment, s.configurationVersion(target), review.Blockers})
	digest.Write(versionData)
	review.Version = hex.EncodeToString(digest.Sum(nil))
	if strings.HasSuffix(r.URL.Path, "/preview") {
		w.Header().Set("Cache-Control", "no-store")
		respond(w, 200, review)
		return
	}
	if !in.Acknowledge || in.Version == "" {
		http.Error(w, "review the move and acknowledge its effects first", 400)
		return
	}
	if !hmac.Equal([]byte(in.Version), []byte(review.Version)) {
		http.Error(w, "the resource or destination changed; review the move again", 409)
		return
	}
	if len(review.Blockers) > 0 {
		respond(w, 409, review)
		return
	}
	config.ProjectID, config.EnvironmentID = "", ""
	raw, err = json.Marshal(config)
	if err == nil {
		sealed, err = seal(s.Key, raw)
	}
	if err != nil {
		http.Error(w, "could not protect configuration", 503)
		return
	}
	if _, err = tx.Exec(ctx, `DELETE FROM configuration_resource_keys WHERE workload_id=$1`, id); err == nil {
		_, err = tx.Exec(ctx, `UPDATE workloads SET team_id=$2,project_id=$3,environment_id=NULLIF($4,''),configuration=$5,webhook_secret=CASE WHEN team_id<>$2 THEN NULL ELSE webhook_secret END WHERE id=$1`, id, in.TeamID, in.ProjectID, in.EnvironmentID, sealed)
	}
	if err != nil {
		if !quotaError(w, err) {
			groupError(w, err)
		}
		return
	}
	if review.CrossTeam {
		for _, query := range []string{`DELETE FROM api_tokens WHERE workload_id=$1`, `DELETE FROM deploy_hooks WHERE workload_id=$1`, `DELETE FROM webhook_deliveries WHERE workload_id=$1`} {
			if _, err = tx.Exec(ctx, query, id); err != nil {
				http.Error(w, "could not revoke source access", 503)
				return
			}
		}
	}
	for _, auditTeam := range []string{team, in.TeamID} {
		if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'resource.move',$2,200,$3)`, p.ID, id, auditTeam); err != nil {
			http.Error(w, "could not audit move", 503)
			return
		}
		if team == in.TeamID {
			break
		}
	}
	if err = tx.Commit(ctx); err != nil {
		http.Error(w, "move conflicted with another change; review again", 409)
		return
	}
	respond(w, 200, map[string]any{"ok": true, "id": id, "team_id": in.TeamID, "project_id": in.ProjectID, "environment_id": in.EnvironmentID})
}
