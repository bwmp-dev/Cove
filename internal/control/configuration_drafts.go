package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

type configurationChange struct {
	Resource string              `json:"resource"`
	Action   string              `json:"action"`
	Fields   []string            `json:"fields"`
	Diffs    []configurationDiff `json:"diffs"`
}

func mergeManifestResource(in, old workloadInput) (workloadInput, error) {
	if old.Database != nil {
		if in.Database != nil {
			proposed, current := databaseMetadata(in.Database), databaseMetadata(old.Database)
			proposed.SettingsOperation, current.SettingsOperation = "", ""
			if !reflect.DeepEqual(proposed, current) {
				return in, errors.New("use Database settings to change managed engine configuration")
			}
		}
		if in.Compose != old.Compose || in.Source != nil || in.Kind == "job" || in.Hooks != nil || in.ComposeBundle != nil || in.Rollout != nil || in.Autoscale != nil {
			return in, errors.New("managed database Compose and lifecycle settings cannot be changed in a draft")
		}
		if in.Environment != nil {
			for name, value := range in.Environment {
				if old.Environment[name] != value {
					return in, errors.New("managed database credentials must be changed through Database settings")
				}
			}
		}
		in.Database, in.Kind, in.Environment, in.FileSecrets = old.Database, "service", old.Environment, old.FileSecrets
	}
	if in.Kind == "" {
		in.Kind = "service"
	}
	if in.Environment == nil {
		in.Environment = old.Environment
	}
	if in.ComposeBundle == nil {
		in.ComposeBundle = old.ComposeBundle
	}
	if in.Routes == nil {
		in.Routes = old.Routes
	} else {
		var err error
		in.Routes, err = retainedRouteAuth(portableRouteInput(in.Routes, old.Routes), old.Routes)
		if err != nil {
			return in, err
		}
	}
	in.Private, in.Backup = old.Private, old.Backup
	if in.Source != nil && old.Source != nil && in.Source.Repository == old.Source.Repository && in.Source.Token == "" {
		copy := *in.Source
		copy.Token = old.Source.Token
		in.Source = &copy
	}
	return in, nil
}

func projectConfigurationChanges(before, after projectConfiguration) ([]configurationChange, error) {
	changes := []configurationChange{}
	add := func(resource, action string, fields []string, diffs []configurationDiff) {
		if len(fields) > 0 {
			changes = append(changes, configurationChange{resource, action, fields, diffs})
		}
	}
	if before.Project != after.Project {
		add("project", "update", []string{"name"}, nil)
	}
	if after.Variables != nil && !reflect.DeepEqual(before.Variables, after.Variables) {
		add("project", "update", []string{"variables"}, nil)
	}
	if after.Policy != nil && !reflect.DeepEqual(before.Policy, after.Policy) {
		add("project", "update", []string{"policy"}, nil)
	}
	environments := map[string]configurationEnvironment{}
	for _, item := range before.Environments {
		environments[item.Name] = item
	}
	for _, item := range after.Environments {
		old, exists := environments[item.Name]
		if !exists {
			add("environment:"+item.Name, "create", []string{"environment"}, nil)
			continue
		}
		fields := []string{}
		if item.Variables != nil && !reflect.DeepEqual(old.Variables, item.Variables) {
			fields = append(fields, "variables")
		}
		if item.Policy != nil && !reflect.DeepEqual(old.Policy, item.Policy) {
			fields = append(fields, "policy")
		}
		add("environment:"+item.Name, "update", fields, nil)
	}
	resources := map[string]configurationResource{}
	for _, resource := range before.Resources {
		resources[resource.Key] = resource
	}
	for _, resource := range after.Resources {
		old, exists := resources[resource.Key]
		if !exists {
			add(resource.Key, "create", []string{"resource"}, nil)
			continue
		}
		config, err := mergeManifestResource(resource.Configuration, old.Configuration)
		if err != nil {
			return nil, err
		}
		a, _ := json.Marshal(old.Configuration)
		b, _ := json.Marshal(config)
		var first, second map[string]json.RawMessage
		_ = json.Unmarshal(a, &first)
		_ = json.Unmarshal(b, &second)
		fields := configurationChanges(first, second)
		if old.Environment != resource.Environment {
			fields = append(fields, "environment")
		}
		if resource.Bindings != nil && !reflect.DeepEqual(old.Bindings, resource.Bindings) {
			fields = append(fields, "bindings")
		}
		add(resource.Key, "update", fields, safeConfigurationDiffs(first, second))
	}
	return changes, nil
}

func (s *Server) projectConfigurationHandler(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccess(w, r, "project", r.PathValue("id"), 1) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	tx, err := s.DB.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		http.Error(w, "project configuration unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	manifest, err := s.readProjectConfiguration(r.Context(), tx, actor(r).Team, r.PathValue("id"))
	if err != nil {
		http.Error(w, "project configuration unavailable", 503)
		return
	}
	version := s.configurationVersion(manifest)
	ids := map[string]string{}
	for _, resource := range manifest.Resources {
		ids[resource.Key] = resource.ID
	}
	respond(w, 200, map[string]any{"version": version, "resource_ids": ids, "manifest": publicProjectConfiguration(manifest, false)})
}

func (s *Server) configurationDrafts(w http.ResponseWriter, r *http.Request) {
	project, id := r.PathValue("id"), r.PathValue("draft")
	if !s.requirePermission(w, r, "project", project, "edit_configuration") {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == "DELETE" {
		result, err := s.DB.Exec(r.Context(), `DELETE FROM configuration_drafts WHERE id=$1 AND project_id=$2 AND team_id=$3`, id, project, actor(r).Team)
		if err != nil {
			http.Error(w, "could not delete draft", 503)
			return
		}
		if result.RowsAffected() != 1 {
			http.NotFound(w, r)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), `SELECT id,base_version,changes,created_at,applied_at,operation_ids FROM configuration_drafts WHERE project_id=$1 AND team_id=$2 ORDER BY created_at DESC LIMIT 100`, project, actor(r).Team)
		if err != nil {
			http.Error(w, "drafts unavailable", 503)
			return
		}
		defer rows.Close()
		type draft struct {
			ID           string                `json:"id"`
			BaseVersion  string                `json:"base_version"`
			Changes      []configurationChange `json:"changes"`
			CreatedAt    time.Time             `json:"created_at"`
			AppliedAt    *time.Time            `json:"applied_at"`
			OperationIDs []string              `json:"operation_ids"`
		}
		items := []draft{}
		for rows.Next() {
			var item draft
			if err = rows.Scan(&item.ID, &item.BaseVersion, &item.Changes, &item.CreatedAt, &item.AppliedAt, &item.OperationIDs); err != nil {
				http.Error(w, "drafts unavailable", 503)
				return
			}
			redactConfigurationChanges(item.Changes)
			items = append(items, item)
		}
		if rows.Err() != nil {
			http.Error(w, "drafts unavailable", 503)
			return
		}
		respond(w, 200, items)
		return
	}
	var in struct {
		Content string `json:"content"`
	}
	if !decode(w, r, &in) {
		return
	}
	manifest, err := parseProjectConfiguration(in.Content)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		http.Error(w, "drafts unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var projectName string
	if err = tx.QueryRow(r.Context(), `SELECT name FROM projects WHERE id=$1 AND team_id=$2 FOR UPDATE`, project, actor(r).Team).Scan(&projectName); err != nil {
		http.Error(w, "project unavailable", 404)
		return
	}
	var count int
	if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM configuration_drafts WHERE project_id=$1`, project).Scan(&count); err != nil {
		http.Error(w, "drafts unavailable", 503)
		return
	}
	if count >= 100 {
		http.Error(w, "delete an old draft before saving another; at most 100 drafts per project", 409)
		return
	}
	before, err := s.readProjectConfiguration(r.Context(), tx, actor(r).Team, project)
	if err != nil {
		http.Error(w, "project configuration unavailable", 503)
		return
	}
	changes, err := projectConfigurationChanges(before, manifest)
	if err != nil {
		http.Error(w, "route protections are ambiguous; set them explicitly through the route settings before importing", 409)
		return
	}
	base := s.configurationVersion(before)
	encrypted, err := s.sealJSON(manifest)
	if err != nil {
		http.Error(w, "could not protect draft", 500)
		return
	}
	id = resourceID()
	if _, err = tx.Exec(r.Context(), `INSERT INTO configuration_drafts(id,project_id,team_id,creator_id,payload,base_version,changes) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, project, actor(r).Team, actor(r).ID, encrypted, base, changes); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save draft", 503)
		return
	}
	respond(w, 201, map[string]any{"id": id, "base_version": base, "changes": changes})
}

func (s *Server) applyConfigurationDraft(w http.ResponseWriter, r *http.Request) {
	project, id := r.PathValue("id"), r.PathValue("draft")
	if !s.requirePermission(w, r, "project", project, "edit_configuration") {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "draft unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var projectName string
	if err = tx.QueryRow(r.Context(), `SELECT name FROM projects WHERE id=$1 AND team_id=$2 FOR UPDATE`, project, actor(r).Team).Scan(&projectName); err != nil {
		http.NotFound(w, r)
		return
	}
	// Lock existing declarations before checking the reviewed version and applying any changes.
	for _, query := range []string{
		`SELECT id FROM workloads WHERE project_id=$1 ORDER BY id FOR UPDATE`,
		`SELECT name FROM shared_variables WHERE project_id=$1 ORDER BY environment_id NULLS FIRST,name FOR UPDATE`,
		`SELECT id FROM environments WHERE project_id=$1 ORDER BY id FOR UPDATE`,
	} {
		rows, e := tx.Query(r.Context(), query, project)
		if e != nil {
			http.Error(w, "project configuration unavailable", 503)
			return
		}
		for rows.Next() {
		}
		rows.Close()
		if rows.Err() != nil {
			http.Error(w, "project configuration unavailable", 503)
			return
		}
	}
	var encrypted []byte
	var base string
	var applied *time.Time
	if err = tx.QueryRow(r.Context(), `SELECT payload,base_version,applied_at FROM configuration_drafts WHERE id=$1 AND project_id=$2 AND team_id=$3 FOR UPDATE`, id, project, actor(r).Team).Scan(&encrypted, &base, &applied); err != nil {
		http.NotFound(w, r)
		return
	}
	if applied != nil {
		http.Error(w, "draft was already applied", 409)
		return
	}
	var manifest projectConfiguration
	if s.unsealJSON(encrypted, &manifest) != nil || validateProjectConfiguration(manifest) != nil {
		http.Error(w, "saved draft unavailable", 503)
		return
	}
	before, err := s.readProjectConfiguration(r.Context(), tx, actor(r).Team, project)
	if err != nil {
		http.Error(w, "project configuration unavailable", 503)
		return
	}
	if s.configurationVersion(before) != base {
		http.Error(w, "project changed since this diff was saved; save and review a new draft", 409)
		return
	}
	operations, ok := s.applyProjectConfiguration(w, r, tx, project, before, manifest)
	if !ok {
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE configuration_drafts SET applied_at=now(),operation_ids=$2 WHERE id=$1`, id, operations); err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id,details) VALUES($1,'configuration applied',$2,200,$3,jsonb_build_object('draft_id',$4::text))`, actor(r).ID, project, actor(r).Team, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not apply draft", 409)
		return
	}
	respond(w, 200, map[string]any{"id": id, "operation_ids": operations, "state": "saved"})
}

func (s *Server) applyProjectConfiguration(w http.ResponseWriter, r *http.Request, tx pgx.Tx, project string, before, manifest projectConfiguration) ([]string, bool) {
	ctx, team := r.Context(), actor(r).Team
	fail := func(message string, status int) ([]string, bool) { http.Error(w, message, status); return nil, false }
	environments := map[string]string{"": ""}
	rows, err := tx.Query(ctx, `SELECT id,name FROM environments WHERE project_id=$1 AND team_id=$2 AND NOT EXISTS(SELECT 1 FROM environment_previews p WHERE p.environment_id=environments.id)`, project, team)
	if err != nil {
		return fail("environments unavailable", 503)
	}
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			break
		}
		environments[name] = id
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		return fail("environments unavailable", 503)
	}
	for _, environment := range manifest.Environments {
		if environments[environment.Name] == "" {
			id := resourceID()
			if _, err = tx.Exec(ctx, `INSERT INTO environments(id,project_id,team_id,name) VALUES($1,$2,$3,$4)`, id, project, team, environment.Name); err != nil {
				return fail("environment name is unavailable", 409)
			}
			environments[environment.Name] = id
		}
	}
	oldResources := map[string]configurationResource{}
	for _, resource := range before.Resources {
		oldResources[resource.Key] = resource
	}
	ids := map[string]string{}
	configs := map[string]workloadInput{}
	operations := []string{}
	resources := append([]configurationResource(nil), manifest.Resources...)
	sort.Slice(resources, func(i, j int) bool { return resources[i].Key < resources[j].Key })
	for _, resource := range resources {
		old, exists := oldResources[resource.Key]
		config, mergeErr := mergeManifestResource(resource.Configuration, old.Configuration)
		if mergeErr != nil {
			return fail(mergeErr.Error(), 409)
		}
		id := resourceID()
		if config.Database != nil && !exists {
			original := config
			generated := workloadInput{Name: config.Name, NodeID: config.NodeID, Kind: "database", Database: &databaseResource{Engine: config.Database.Engine, Version: config.Database.Version, TemplateRevision: config.Database.TemplateRevision}, Limits: config.Limits}
			if err = prepareDatabase(&generated, id); err != nil {
				return fail("database declaration requires its current pinned template", 409)
			}
			expected, actual := *generated.Database, *original.Database
			expected.Host, actual.Host = "", ""
			actual.SettingsOperation = ""
			if !reflect.DeepEqual(expected, actual) || original.Compose != generated.Compose {
				return fail("create this database with its dedicated settings before importing pooler, extension or public-TLS configuration", 409)
			}
			config = generated
		}
		if err = validateWorkload(config); err != nil {
			return fail("resource configuration is invalid: "+err.Error(), 400)
		}
		if exists {
			err = tx.QueryRow(ctx, `SELECT w.id FROM workloads w LEFT JOIN configuration_resource_keys k ON k.workload_id=w.id WHERE w.project_id=$1 AND w.team_id=$2 AND COALESCE(k.key,w.id)=$3`, project, team, resource.Key).Scan(&id)
			if err != nil {
				return fail("resource changed; review a new draft", 409)
			}
			if old.Configuration.NodeID != config.NodeID || old.Environment != resource.Environment {
				return fail("use the resource migration flow to change existing node or environment placement", 409)
			}
			if !s.requirePersistentResource(w, r, id) {
				return nil, false
			}
		}
		if exists {
			var connected bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM github_sources WHERE workload_id=$1) OR EXISTS(SELECT 1 FROM git_sources WHERE workload_id=$1)`, id).Scan(&connected); err != nil {
				return fail("Git connection unavailable", 503)
			}
			if connected && (config.Source == nil || old.Configuration.Source == nil || config.Source.Repository != old.Configuration.Source.Repository) {
				return fail("disconnect the managed Git provider before changing its repository in a draft", 409)
			}
			if old.Configuration.Source != nil && config.Source != nil && old.Configuration.Source.SSHHostKey != "" && (old.Configuration.Source.SSHHostKey != config.Source.SSHHostKey || old.Configuration.Source.Repository != config.Source.Repository) {
				return fail("rotate the SSH deploy key to change its repository or host pin", 409)
			}
			if config.ComposeBundle != nil || config.Source != nil && config.Source.InlineCompose {
				var saved []byte
				err = tx.QueryRow(ctx, `SELECT configuration FROM preview_policies WHERE workload_id=$1`, id).Scan(&saved)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return fail("preview policy unavailable", 503)
				}
				if err == nil {
					var policy previewPolicy
					if s.unsealJSON(saved, &policy) != nil {
						return fail("preview policy unavailable", 503)
					}
					if policy.Enabled {
						return fail("disable pull-request previews before selecting managed Compose, files or profiles", 409)
					}
				}
			}
		}
		var ready, assigned, bundleSupported, routesSupported bool
		if err = tx.QueryRow(ctx, `SELECT state='ready',EXISTS(SELECT 1 FROM team_nodes WHERE team_id=$2 AND node_id=n.id),COALESCE((capabilities->>'compose_bundles')::boolean,false),COALESCE((capabilities->>'live_routes')::boolean,false) FROM nodes n WHERE n.id=$1`, config.NodeID, team).Scan(&ready, &assigned, &bundleSupported, &routesSupported); err != nil || !exists && !ready {
			return fail("new resources require ready nodes", 409)
		}
		if !actor(r).Admin && !assigned {
			return fail("a resource node is not assigned to this team", 403)
		}
		if config.ComposeBundle != nil && !bundleSupported && !reflect.DeepEqual(config.ComposeBundle, old.Configuration.ComposeBundle) {
			return fail("upgrade and verify the worker before selecting Compose files or profiles", 409)
		}
		var active bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))`, id).Scan(&active); err != nil {
			return fail("operations unavailable", 503)
		}
		if active {
			return fail("wait for active operations before applying a draft", 409)
		}
		if !s.checkRouteListenerSelection(w, r.WithContext(ctx), config.NodeID, config.Routes) {
			return nil, false
		}
		if !s.checkRouteClaims(w, ctx, tx, id, config.NodeID, config.Routes) {
			return nil, false
		}
		if exists && !reflect.DeepEqual(config.Routes, old.Configuration.Routes) {
			attempted, err := deployAttempted(ctx, tx, id)
			if err != nil {
				return fail("route history unavailable", 503)
			}
			if attempted {
				if !routesSupported {
					return fail("upgrade and verify the worker before applying live route changes", 409)
				}
				request := worker.Request{ID: resourceID(), Workload: id, Action: "routes", Kind: config.Kind, Routes: config.Routes}
				payload, err := s.sealJSON(request)
				if err != nil {
					return fail("could not protect route change", 500)
				}
				if _, err = tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'routes',$3)`, request.ID, id, payload); err != nil {
					return fail("could not queue route change", 409)
				}
				operations = append(operations, request.ID)
				config.Routes = old.Configuration.Routes
			}
		}
		sealed, err := s.sealJSON(config)
		if err != nil {
			return fail("could not protect resource", 500)
		}
		cpu, memory := configuredUsage(config)
		if exists {
			_, err = tx.Exec(ctx, `UPDATE workloads SET name=$2,configuration=$3,kind=$4,configured_cpu=$5,configured_memory_mb=$6,job_cron=CASE WHEN $4='job' THEN job_cron ELSE '' END,next_job_at=CASE WHEN $4='job' THEN next_job_at ELSE NULL END WHERE id=$1`, id, config.Name, sealed, resourceKind(config), cpu, memory)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO workloads(id,node_id,name,configuration,kind,project_id,environment_id,team_id,configured_cpu,configured_memory_mb) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10)`, id, config.NodeID, config.Name, sealed, resourceKind(config), project, environments[resource.Environment], team, cpu, memory)
		}
		if quotaError(w, err) {
			return nil, false
		}
		if err != nil {
			return fail("could not save resource", 409)
		}
		if err = s.pruneImageUpdateRetention(ctx, tx, id, config); err != nil {
			return fail("retained image versions unavailable", 503)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO configuration_resource_keys(project_id,key,workload_id) VALUES($1,$2,$3) ON CONFLICT(workload_id) DO NOTHING`, project, resource.Key, id); err != nil {
			return fail("resource key is already in use", 409)
		}
		ids[resource.Key], configs[resource.Key] = id, config
	}
	for _, resource := range resources {
		if resource.Bindings == nil {
			continue
		}
		id := ids[resource.Key]
		if _, err = tx.Exec(ctx, `DELETE FROM resource_variable_bindings WHERE workload_id=$1`, id); err != nil {
			return fail("could not update variable bindings", 503)
		}
		for _, binding := range resource.Bindings {
			source, config := ids[binding.Resource], configs[binding.Resource]
			if binding.SourceID != "" {
				source = binding.SourceID
				if source == id {
					return fail("a variable binding cannot reference itself", 400)
				}
				if !s.requireAccess(w, r, "workload", source, 3) || !s.requirePersistentResource(w, r, source) {
					return nil, false
				}
				var encrypted []byte
				if err = tx.QueryRow(ctx, `SELECT configuration FROM workloads WHERE id=$1 AND team_id=$2 FOR UPDATE`, source, team).Scan(&encrypted); err != nil || s.unsealJSON(encrypted, &config) != nil {
					return fail("binding source is unavailable in this team", 409)
				}
			}
			if _, ok := config.Environment[binding.Key]; !ok {
				return fail("a binding source variable does not exist", 400)
			}
			if _, err = tx.Exec(ctx, `INSERT INTO resource_variable_bindings(workload_id,name,source_id,source_key) VALUES($1,$2,$3,$4)`, id, binding.Name, source, binding.Key); err != nil {
				return fail("could not save variable binding", 503)
			}
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE projects SET name=$2 WHERE id=$1`, project, manifest.Project); err != nil {
		return fail("project name is already in use", 409)
	}
	policy := manifest.Policy
	if reflect.DeepEqual(policy, before.Policy) {
		policy = nil
	}
	if err = s.applyManifestScope(ctx, tx, team, project, "", manifest.Variables, policy, s.teamAdmin(r, team)); err != nil {
		return fail(err.Error(), 409)
	}
	for _, environment := range manifest.Environments {
		policy := environment.Policy
		for _, old := range before.Environments {
			if old.Name == environment.Name && reflect.DeepEqual(old.Policy, policy) {
				policy = nil
			}
		}
		if err = s.applyManifestScope(ctx, tx, team, project, environments[environment.Name], environment.Variables, policy, s.teamAdmin(r, team)); err != nil {
			return fail(err.Error(), 409)
		}
	}
	return operations, true
}

func (s *Server) applyManifestScope(ctx context.Context, tx pgx.Tx, team, project, environment string, variables map[string]*string, policy *deploymentPolicy, admin bool) error {
	for name, value := range variables {
		if value == nil {
			if _, err := tx.Exec(ctx, `DELETE FROM shared_variables WHERE team_id=$1 AND project_id=$2 AND environment_id IS NOT DISTINCT FROM NULLIF($3,'') AND name=$4`, team, project, environment, name); err != nil {
				return errors.New("could not remove shared variable")
			}
		} else {
			encrypted, err := seal(s.Key, []byte(*value))
			if err != nil {
				return errors.New("could not protect shared variable")
			}
			if _, err = tx.Exec(ctx, `INSERT INTO shared_variables(team_id,project_id,environment_id,name,value) VALUES($1,$2,NULLIF($3,''),$4,$5) ON CONFLICT(team_id,project_id,environment_id,name) DO UPDATE SET value=EXCLUDED.value`, team, project, environment, name, encrypted); err != nil {
				return errors.New("could not save shared variable")
			}
		}
	}
	if policy == nil {
		return nil
	}
	if !admin {
		return errors.New("team administrator required to apply deployment policies")
	}
	policyProject := project
	if environment != "" {
		policyProject = ""
	}
	if _, err := tx.Exec(ctx, `INSERT INTO deployment_policies(team_id,project_id,environment_id,approval,freeze_from,freeze_until,window_start,window_end,timezone,recurring_windows,freezes,outside_window) VALUES($1,NULLIF($2,''),NULLIF($3,''),$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(team_id,project_id,environment_id) DO UPDATE SET approval=$4,freeze_from=$5,freeze_until=$6,window_start=$7,window_end=$8,timezone=$9,recurring_windows=$10,freezes=$11,outside_window=$12`, team, policyProject, environment, policy.Approval, policy.FreezeFrom, policy.FreezeUntil, policy.WindowStart, policy.WindowEnd, policy.Timezone, policy.Windows, policy.Freezes, policy.OutsideWindow); err != nil {
		return errors.New("could not save deployment policy")
	}
	return nil
}
