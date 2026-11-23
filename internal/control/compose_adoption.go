package control

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

type composeAdoptionInput struct {
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	Projects      []struct {
		Project string `json:"project"`
		Key     string `json:"key"`
		Name    string `json:"name"`
	} `json:"projects"`
}

type composeAdoptionItem struct {
	Request       worker.AdoptionRequest `json:"request"`
	Key           string                 `json:"key"`
	Configuration workloadInput          `json:"configuration"`
}

type savedComposeAdoption struct {
	Items   []composeAdoptionItem `json:"items"`
	Changes []configurationChange `json:"changes"`
}

func (s *Server) composeAdoptions(w http.ResponseWriter, r *http.Request) {
	if !actor(r).Admin {
		http.Error(w, "instance administration is required to inspect and adopt unmanaged workloads", 403)
		return
	}
	if credential, ok := r.Context().Value(apiCredentialKey{}).(apiCredential); ok && credential.Team != "" {
		http.Error(w, "node-wide adoption requires a personal token or browser session", 403)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	ctx, node, team := r.Context(), r.PathValue("id"), actor(r).Team
	var supported bool
	if err := s.DB.QueryRow(ctx, `SELECT COALESCE((capabilities->>'adoption')::boolean,false) FROM nodes WHERE id=$1`, node).Scan(&supported); err != nil {
		http.NotFound(w, r)
		return
	}
	if !supported {
		http.Error(w, "upgrade and verify the worker before adopting Compose projects", 409)
		return
	}
	id := r.PathValue("adoption")
	if id != "" && r.Method == "POST" {
		release, err := s.requestGuard(ctx, "cove-adoption:"+id, false)
		if err != nil {
			if errors.Is(err, errRequestBusy) {
				http.Error(w, errRequestBusy.Error(), 409)
			} else if errors.Is(err, errGuardCapacity) {
				http.Error(w, errGuardCapacity.Error(), 503)
			} else {
				http.Error(w, "adoption lock unavailable", 503)
			}
			return
		}
		defer release()
	}
	if strings.HasSuffix(r.URL.Path, "/discover") {
		response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "compose_discover"})
		if err != nil {
			http.Error(w, "Compose discovery unavailable; check worker connectivity and project limits", 502)
			return
		}
		if response.ComposeProjects == nil {
			response.ComposeProjects = []worker.ComposeProject{}
		}
		respond(w, 200, map[string]any{"projects": response.ComposeProjects, "warnings": adoptionWarnings()})
		return
	}
	if id == "" && r.Method == "GET" {
		rows, err := s.DB.Query(ctx, `SELECT id,project_id,COALESCE(environment_id,''),status,created_at FROM compose_adoptions WHERE node_id=$1 AND team_id=$2 AND (status<>'planned' OR expires_at>now()) ORDER BY created_at DESC LIMIT 100`, node, team)
		if err != nil {
			http.Error(w, "adoptions unavailable", 503)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var id, project, environment, status string
			var created time.Time
			if rows.Scan(&id, &project, &environment, &status, &created) != nil {
				http.Error(w, "adoptions unavailable", 503)
				return
			}
			items = append(items, map[string]any{"id": id, "project_id": project, "environment_id": environment, "status": status, "created_at": created})
		}
		if rows.Err() != nil {
			http.Error(w, "adoptions unavailable", 503)
			return
		}
		respond(w, 200, items)
		return
	}
	if id == "" {
		s.planComposeAdoption(w, r, node, team)
		return
	}
	if r.Method == "DELETE" {
		result, err := s.DB.Exec(ctx, `DELETE FROM compose_adoptions WHERE id=$1 AND node_id=$2 AND team_id=$3 AND status='planned'`, id, node, team)
		if err != nil {
			http.Error(w, "could not delete adoption review", 503)
			return
		}
		if result.RowsAffected() != 1 {
			http.Error(w, "only an adoption that has not started can be deleted", 409)
			return
		}
		respond(w, 200, apiOK{true})
		return
	}
	var payload []byte
	var project, environment, status, base string
	var completed []string
	err := s.DB.QueryRow(ctx, `SELECT payload,project_id,COALESCE(environment_id,''),status,base_version,completed FROM compose_adoptions WHERE id=$1 AND node_id=$2 AND team_id=$3 AND (status<>'planned' OR expires_at>now())`, id, node, team).Scan(&payload, &project, &environment, &status, &base, &completed)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	var saved savedComposeAdoption
	if err != nil || s.unsealJSON(payload, &saved) != nil {
		http.Error(w, "adoption review unavailable", 503)
		return
	}
	if r.Method == "GET" {
		respond(w, 200, publicAdoption(id, status, project, environment, saved, completed))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/refresh") {
		if status != "applying" {
			http.Error(w, "only a pending adoption can refresh its source review", 409)
			return
		}
		for i := range saved.Items {
			item := &saved.Items[i]
			if slices.Contains(completed, item.Request.Workload) {
				continue
			}
			registered, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "compose_adoption_status", ID: item.Request.Workload})
			if err != nil {
				http.Error(w, "could not reconcile worker adoption state; retry when connected", 502)
				return
			}
			if registered.Adopted != nil {
				if registered.Adopted.Project != item.Request.Project || registered.Adopted.Fingerprint != item.Request.Fingerprint {
					http.Error(w, "worker ownership differs from the saved adoption; inspect the worker before continuing", 409)
					return
				}
				continue
			}
			response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "compose_plan", Adoption: &worker.AdoptionRequest{Project: item.Request.Project}})
			if err != nil || response.Adoption == nil {
				http.Error(w, "source cannot be reviewed; restore readable Compose files and reconcile their container hashes first", 409)
				return
			}
			item.Request.Fingerprint = response.Adoption.Fingerprint
			item.Configuration.Compose = response.Adoption.Configuration.Compose
			item.Configuration.Environment = response.Adoption.Configuration.Environment
			if validateWorkload(item.Configuration) != nil {
				http.Error(w, "refreshed source exceeds configuration limits", 409)
				return
			}
		}
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			http.Error(w, "adoption unavailable", 503)
			return
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `SELECT set_config('cove.adoption_write','true',true)`); err != nil {
			http.Error(w, "adoption refresh unavailable", 503)
			return
		}
		var currentCompleted []string
		if err = tx.QueryRow(ctx, `SELECT completed FROM compose_adoptions WHERE id=$1 AND status='applying' FOR UPDATE`, id).Scan(&currentCompleted); err != nil || !slices.Equal(completed, currentCompleted) {
			http.Error(w, "adoption changed; reload before refreshing", 409)
			return
		}
		for _, item := range saved.Items {
			if slices.Contains(completed, item.Request.Workload) {
				continue
			}
			sealed, err := s.sealJSON(item.Configuration)
			if err != nil {
				http.Error(w, "could not protect refreshed configuration", 503)
				return
			}
			if _, err = tx.Exec(ctx, `UPDATE workloads SET configuration=$2 WHERE id=$1`, item.Request.Workload, sealed); err != nil {
				http.Error(w, "could not update pending configuration", 503)
				return
			}
		}
		payload, err = s.sealJSON(saved)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE compose_adoptions SET payload=$2 WHERE id=$1`, id, payload)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			http.Error(w, "could not save refreshed review", 503)
			return
		}
		respond(w, 200, publicAdoption(id, status, project, environment, saved, completed))
		return
	}
	var confirm struct {
		Acknowledge   bool `json:"acknowledge"`
		ManagerPaused bool `json:"manager_paused"`
	}
	if !decode(w, r, &confirm) {
		return
	}
	if !confirm.Acknowledge || !confirm.ManagerPaused {
		http.Error(w, "acknowledge the review and confirm that source deployment, cleanup and patching automation is paused", 400)
		return
	}
	if status == "complete" {
		respond(w, 200, publicAdoption(id, status, project, environment, saved, completed))
		return
	}
	if status == "planned" {
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			http.Error(w, "adoption unavailable", 503)
			return
		}
		defer tx.Rollback(ctx)
		var locked string
		if err = tx.QueryRow(ctx, `SELECT status FROM compose_adoptions WHERE id=$1 AND status='planned' FOR UPDATE`, id).Scan(&locked); err != nil {
			http.Error(w, "adoption changed; reload its review", 409)
			return
		}
		if _, err = tx.Exec(ctx, `SELECT id FROM projects WHERE id=$1 AND team_id=$2 FOR UPDATE`, project, team); err != nil {
			http.Error(w, "destination unavailable", 503)
			return
		}
		before, err := s.readProjectConfiguration(ctx, tx, team, project)
		if err != nil || s.configurationVersion(before) != base {
			http.Error(w, "destination changed; create and review a new adoption plan", 409)
			return
		}
		for _, item := range saved.Items {
			sealed, err := s.sealJSON(item.Configuration)
			if err != nil {
				http.Error(w, "could not protect adopted configuration", 503)
				return
			}
			cpu, memory := configuredUsage(item.Configuration)
			if _, err = tx.Exec(ctx, `INSERT INTO workloads(id,node_id,name,configuration,kind,environment_id,team_id,project_id,configured_cpu,configured_memory_mb) VALUES($1,$2,$3,$4,'service',NULLIF($5,''),$6,$7,$8,$9)`, item.Request.Workload, node, item.Configuration.Name, sealed, environment, team, project, cpu, memory); err != nil {
				if !quotaError(w, err) {
					groupError(w, err)
				}
				return
			}
			if _, err = tx.Exec(ctx, `INSERT INTO configuration_resource_keys(project_id,key,workload_id) VALUES($1,$2,$3)`, project, item.Key, item.Request.Workload); err != nil {
				groupError(w, err)
				return
			}
			if _, err = tx.Exec(ctx, `INSERT INTO compose_adoption_claims(node_id,project_name,adoption_id,workload_id) VALUES($1,$2,$3,$4)`, node, item.Request.Project, id, item.Request.Workload); err != nil {
				http.Error(w, "a source project is already claimed by another adoption", 409)
				return
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE compose_adoptions SET status='applying' WHERE id=$1`, id); err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			http.Error(w, "could not reserve adopted resources; reload the review", 409)
			return
		}
	}
	for _, item := range saved.Items {
		if slices.Contains(completed, item.Request.Workload) {
			continue
		}
		if _, err = s.nodeRPC(ctx, node, worker.RPCRequest{Method: "compose_adopt", Adoption: &item.Request}); err != nil {
			http.Error(w, "adoption is pending; containers were not restarted. Check source files, unchanged configuration and worker connectivity, then retry this same adoption", 502)
			return
		}
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			http.Error(w, "adoption acknowledgement is pending; retry this adoption", 503)
			return
		}
		request := worker.Request{ID: item.Request.Release, Workload: item.Request.Workload, Action: "adopt", Compose: item.Configuration.Compose, Environment: item.Configuration.Environment, Kind: "service"}
		sealed, err := s.sealJSON(request)
		newOperation := false
		if err == nil {
			now := time.Now().UTC()
			result := worker.Operation{ID: request.ID, Workload: request.Workload, Action: "adopt", State: "succeeded", CreatedAt: now, FinishedAt: &now}
			tag, insertErr := tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES($1,$2,'adopt','succeeded',$3,$4) ON CONFLICT(id) DO NOTHING`, request.ID, request.Workload, sealed, result)
			err = insertErr
			newOperation = tag.RowsAffected() == 1
			if err == nil && !newOperation {
				var matches bool
				err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE id=$1 AND workload_id=$2 AND action='adopt' AND state='succeeded')`, request.ID, request.Workload).Scan(&matches)
				if err == nil && !matches {
					err = errors.New("adoption operation identity conflict")
				}
			}
		}
		if err == nil && newOperation {
			_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'resource.adopt',$2,200,$3)`, actor(r).ID, request.Workload, team)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE compose_adoptions SET completed=completed||jsonb_build_array($2::text) WHERE id=$1 AND NOT completed ? $2`, id, item.Request.Workload)
		}
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			tx.Rollback(ctx)
		}
		if err != nil {
			http.Error(w, "adoption acknowledgement is pending; retry this adoption", 503)
			return
		}
		completed = append(completed, item.Request.Workload)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE compose_adoptions SET status='complete' WHERE id=$1 AND jsonb_array_length(completed)=jsonb_array_length(resource_ids)`, id); err != nil {
		http.Error(w, "adoption completion is pending; retry this adoption", 503)
		return
	}
	respond(w, 200, publicAdoption(id, "complete", project, environment, saved, completed))
}

func (s *Server) planComposeAdoption(w http.ResponseWriter, r *http.Request, node, team string) {
	var in composeAdoptionInput
	if !decode(w, r, &in) {
		return
	}
	if len(in.Projects) == 0 || len(in.Projects) > 16 || in.ProjectID == "" {
		http.Error(w, "choose a destination project and 1-16 Compose projects", 400)
		return
	}
	ctx := r.Context()
	if _, err := s.DB.Exec(ctx, `DELETE FROM compose_adoptions WHERE node_id=$1 AND team_id=$2 AND status='planned' AND expires_at<=now()`, node, team); err != nil {
		http.Error(w, "could not expire old adoption reviews", 503)
		return
	}
	var count int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM compose_adoptions WHERE node_id=$1 AND team_id=$2 AND status='planned'`, node, team).Scan(&count); err != nil {
		http.Error(w, "adoption reviews unavailable", 503)
		return
	}
	if count >= 20 {
		http.Error(w, "delete an unstarted review before creating another; at most 20 per node and team", 409)
		return
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		http.Error(w, "destination unavailable", 503)
		return
	}
	defer tx.Rollback(ctx)
	before, err := s.readProjectConfiguration(ctx, tx, team, in.ProjectID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if len(before.Resources)+len(in.Projects) > 128 {
		http.Error(w, "adoption would exceed the canonical project limit of 128 resources", 409)
		return
	}
	environmentName := ""
	if in.EnvironmentID != "" {
		if err = tx.QueryRow(ctx, `SELECT name FROM environments WHERE id=$1 AND project_id=$2 AND team_id=$3 AND NOT EXISTS(SELECT 1 FROM environment_previews WHERE environment_id=$1)`, in.EnvironmentID, in.ProjectID, team).Scan(&environmentName); err != nil {
			http.Error(w, "choose a persistent destination environment", 400)
			return
		}
	}
	if err = tx.Rollback(ctx); err != nil {
		http.Error(w, "could not finish reading destination metadata", 503)
		return
	}
	manifest := projectConfiguration{APIVersion: "cove/v1", Project: before.Project, Environments: []configurationEnvironment{}, Resources: []configurationResource{}}
	if environmentName != "" {
		manifest.Environments = append(manifest.Environments, configurationEnvironment{Name: environmentName})
	}
	keys := map[string]bool{}
	for _, item := range before.Resources {
		keys[item.Key] = true
	}
	projects := map[string]bool{}
	saved := savedComposeAdoption{Items: []composeAdoptionItem{}}
	ids := []string{}
	for _, selection := range in.Projects {
		if selection.Project == "" || projects[selection.Project] || !configurationKey.MatchString(selection.Key) || keys[selection.Key] {
			http.Error(w, "choose unique source projects and unused canonical resource keys", 400)
			return
		}
		projects[selection.Project], keys[selection.Key] = true, true
		response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "compose_plan", Adoption: &worker.AdoptionRequest{Project: selection.Project}})
		if err != nil || response.Adoption == nil {
			http.Error(w, "source adoption dry-run failed; original Compose files must be readable and match container hashes, with consistent images and no one-off containers", 409)
			return
		}
		plan := response.Adoption
		config := workloadInput{Name: selection.Name, NodeID: node, Compose: plan.Configuration.Compose, Environment: plan.Configuration.Environment, Kind: "service"}
		if config.Name == "" {
			config.Name = selection.Project
		}
		request := worker.AdoptionRequest{Project: selection.Project, Fingerprint: plan.Fingerprint, Workload: resourceID(), Release: resourceID()}
		saved.Items = append(saved.Items, composeAdoptionItem{request, selection.Key, config})
		ids = append(ids, request.Workload)
		manifest.Resources = append(manifest.Resources, configurationResource{Key: selection.Key, Environment: environmentName, Configuration: config, Bindings: []configurationBinding{}})
	}
	if err = validateProjectConfiguration(manifest); err != nil {
		http.Error(w, "adoption configuration exceeds canonical project limits", 400)
		return
	}
	saved.Changes, err = projectConfigurationChanges(before, manifest)
	if err != nil {
		http.Error(w, "adoption route protections require explicit review", 409)
		return
	}
	payload, err := s.sealJSON(saved)
	if err != nil {
		http.Error(w, "could not protect adoption review", 503)
		return
	}
	id := resourceID()
	if _, err = s.DB.Exec(ctx, `INSERT INTO compose_adoptions(id,node_id,team_id,project_id,environment_id,payload,base_version,resource_ids) VALUES($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8)`, id, node, team, in.ProjectID, in.EnvironmentID, payload, s.configurationVersion(before), ids); err != nil {
		http.Error(w, "could not save adoption review", 503)
		return
	}
	view := publicAdoption(id, "planned", in.ProjectID, in.EnvironmentID, saved, nil)
	respond(w, 201, view)
}

func publicAdoption(id, status, project, environment string, saved savedComposeAdoption, completed []string) map[string]any {
	redactConfigurationChanges(saved.Changes)
	items := []map[string]any{}
	for _, item := range saved.Items {
		items = append(items, map[string]any{"project": item.Request.Project, "key": item.Key, "name": item.Configuration.Name, "resource_id": item.Request.Workload, "complete": slices.Contains(completed, item.Request.Workload)})
	}
	return map[string]any{"id": id, "status": status, "project_id": project, "environment_id": environment, "resources": items, "changes": saved.Changes, "warnings": adoptionWarnings()}
}

func adoptionWarnings() []string {
	return []string{
		"Adoption records Cove ownership of existing containers without restarting, recreating, starting or stopping them. Docker project names, mounts, ports, networks and source files remain unchanged. Existing proxy routes and security middleware remain under the source configuration until explicitly migrated.",
		"You must pause the source manager's deployment, cleanup and patching automation yourself. Cove cannot verify that assertion. Keep independent backups and verify data ownership and restore procedures before cutover.",
		"Inspect manual Docker runtime overrides before adopting. A matching Compose creation hash does not prove that nobody changed the running container afterwards; adoption accepts the current runtime as its initial drift baseline.",
		"Cove retains a snapshot pinned to the existing image IDs. Its first explicit deployment can recreate containers. Configure routes, authentication, backups and Git sources separately before deploying; no DNS or ingress cutover is performed by adoption.",
		"An interrupted adoption stays pending and blocks resource changes. Retry the same adoption to reconcile the worker's durable ownership record; completed resources are retained and never adopted twice.",
	}
}

func (s *Server) requireAdoptionComplete(w http.ResponseWriter, r *http.Request, id string) bool {
	var pending bool
	if err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM compose_adoptions WHERE status='applying' AND resource_ids ? $1)`, id).Scan(&pending); err != nil {
		http.Error(w, "adoption state unavailable", 503)
		return false
	}
	if pending {
		http.Error(w, "complete the pending node adoption before changing this resource", 409)
		return false
	}
	return true
}
