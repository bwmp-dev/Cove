package control

import (
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"slices"
	"sort"
	"strings"
)

func validateWorkload(in workloadInput) error {
	if in.Source != nil && in.Source.SSHPrivateKey != "" {
		return errors.New("SSH private keys are managed through the deploy key endpoint")
	}
	if in.Kind == "job" && in.Backup != nil {
		return errors.New("disable backups before converting a service to a job")
	}
	if err := worker.ValidateBackup(in.Backup); err != nil {
		return err
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 {
		return errors.New("a name of at most 120 characters is required")
	}
	return worker.ValidateRequest(worker.Request{ID: "validation", Workload: "validation", Action: "deploy", ComposeBundle: in.ComposeBundle, Compose: in.Compose, Environment: in.Environment, Source: in.Source, Kind: in.Kind, JobService: in.JobService, Routes: in.Routes, Private: in.Private, Limits: in.Limits, Hooks: in.Hooks, FileSecrets: in.FileSecrets, GuardedDeploy: in.GuardedDeploy})
}
func (s *Server) getWorkload(w http.ResponseWriter, r *http.Request) {
	var encrypted []byte
	var environmentID, projectID string
	var stopped bool
	err := s.DB.QueryRow(r.Context(), "SELECT configuration,COALESCE(environment_id,''),project_id,manually_stopped FROM workloads WHERE id=$1", r.PathValue("id")).Scan(&encrypted, &environmentID, &projectID, &stopped)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	data, err := unseal(s.Key, encrypted)
	if err != nil {
		http.Error(w, "configuration unavailable", 500)
		return
	}
	var configuration workloadInput
	if json.Unmarshal(data, &configuration) != nil {
		http.Error(w, "configuration unavailable", 500)
		return
	}
	keys := []string{}
	for key := range configuration.Environment {
		keys = append(keys, key)
	}
	configuration.Environment = nil
	sort.Strings(keys)
	hasToken := false
	if configuration.Source != nil {
		hasToken = configuration.Source.Token != ""
		configuration.Source.Token = ""
		configuration.Source.SSHPrivateKey = ""
	}
	level, err := s.access(r, "workload", r.PathValue("id"))
	if err != nil {
		http.Error(w, "authorization unavailable", 503)
		return
	}
	deployed, err := deployedWorkload(r.Context(), s.DB, r.PathValue("id"))
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	var previewSource string
	if err = s.DB.QueryRow(r.Context(), `SELECT COALESCE((SELECT source_id FROM previews WHERE workload_id=$1),'')`, r.PathValue("id")).Scan(&previewSource); err != nil {
		http.Error(w, "preview metadata unavailable", 503)
		return
	}
	if previewSource != "" && level > 2 {
		level = 2
	}
	var environmentPreview string
	if err = s.DB.QueryRow(r.Context(), `SELECT COALESCE((SELECT preview_id FROM environment_preview_resources WHERE resource_id=$1),'')`, r.PathValue("id")).Scan(&environmentPreview); err != nil {
		http.Error(w, "preview metadata unavailable", 503)
		return
	}
	var pendingAdoption string
	if err = s.DB.QueryRow(r.Context(), `SELECT COALESCE((SELECT id FROM compose_adoptions WHERE status='applying' AND resource_ids ? $1 LIMIT 1),'')`, r.PathValue("id")).Scan(&pendingAdoption); err != nil {
		http.Error(w, "adoption metadata unavailable", 503)
		return
	}
	if pendingAdoption != "" {
		level = 1
	}
	// Protected preview routes keep their password hash on the server.
	for i := range configuration.Routes {
		configuration.Routes[i].Auth = ""
	}
	respond(w, 200, workloadView{PendingAdoptionID: pendingAdoption, ID: r.PathValue("id"), Name: configuration.Name, NodeID: configuration.NodeID, ProjectID: projectID, EnvironmentID: environmentID, Kind: configuration.Kind, JobService: configuration.JobService, Compose: configuration.Compose, Source: configuration.Source, HasSourceToken: hasToken, EnvironmentKeys: keys, Routes: configuration.Routes, GuardedDeploy: configuration.GuardedDeploy, FileSecrets: configuration.FileSecrets, Hooks: configuration.Hooks, Limits: configuration.Limits, Deployed: deployed, Permissions: permissions(level), ManuallyStopped: stopped, PreviewSourceID: previewSource, EnvironmentPreviewID: environmentPreview})
}
func (s *Server) updateWorkload(w http.ResponseWriter, r *http.Request) {
	var in workloadInput
	if !decode(w, r, &in) {
		return
	}
	if in.Backup != nil {
		http.Error(w, "use the backup policy endpoint to change backups", 400)
		return
	}
	if in.EnvironmentID != "" || in.ProjectID != "" {
		http.Error(w, "use the reviewed resource move endpoint to change project placement", 400)
		return
	}
	if in.Environment != nil && in.EnvironmentPatch != nil {
		http.Error(w, "choose environment replacement or per-key updates", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var encrypted []byte
	var node string
	err = tx.QueryRow(r.Context(), "SELECT configuration,node_id FROM workloads WHERE id=$1 FOR UPDATE", r.PathValue("id")).Scan(&encrypted, &node)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if in.NodeID != node {
		http.Error(w, "moving an existing workload requires a data migration", 409)
		return
	}
	var active bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))", r.PathValue("id")).Scan(&active); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if active {
		http.Error(w, "wait for the active operation before editing", 409)
		return
	}
	if in.ComposeBundle != nil || in.Source != nil && in.Source.InlineCompose {
		var saved []byte
		err := tx.QueryRow(r.Context(), "SELECT configuration FROM preview_policies WHERE workload_id=$1", r.PathValue("id")).Scan(&saved)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "preview settings unavailable", 503)
			return
		}
		if err == nil {
			raw, err := unseal(s.Key, saved)
			var policy previewPolicy
			if err != nil || json.Unmarshal(raw, &policy) != nil {
				http.Error(w, "preview settings unavailable", 503)
				return
			}
			if policy.Enabled {
				http.Error(w, "disable previews before selecting Cove-managed Compose, multiple files or profiles", 409)
				return
			}
		}
	}
	{
		data, err := unseal(s.Key, encrypted)
		if err != nil {
			http.Error(w, "configuration unavailable", 500)
			return
		}
		var old workloadInput
		if json.Unmarshal(data, &old) != nil {
			http.Error(w, "configuration unavailable", 500)
			return
		}
		if in.Backup == nil {
			in.Backup = old.Backup
		}
		in.Private = old.Private
		if in.Limits == nil {
			in.Limits = old.Limits
		}
		if in.Hooks == nil {
			in.Hooks = old.Hooks
		}
		if in.FileSecrets == nil {
			in.FileSecrets = old.FileSecrets
		}
		if in.GuardedDeploy == nil {
			in.GuardedDeploy = old.GuardedDeploy
		}
		if in.ComposeBundle == nil {
			in.ComposeBundle = old.ComposeBundle
		}
		if in.Environment == nil {
			in.Environment = old.Environment
		}
		if in.Routes == nil {
			in.Routes = old.Routes
		} else if !slices.Equal(in.Routes, old.Routes) {
			attempted, err := deployAttempted(r.Context(), tx, r.PathValue("id"))
			if err != nil {
				http.Error(w, "database unavailable", 503)
				return
			}
			if attempted {
				http.Error(w, "use the routes endpoint to change routes on a deployed resource", 409)
				return
			}
			if !s.checkRouteClaims(w, r.Context(), tx, r.PathValue("id"), in.Routes) {
				return
			}
		}
		if in.Source != nil && in.Source.Token == "" && old.Source != nil && in.Source.Repository == old.Source.Repository {
			in.Source.Token = old.Source.Token
		}
	}
	if in.Source != nil && strings.HasPrefix(in.Source.Repository, "ssh://") {
		var repository, host string
		err := tx.QueryRow(r.Context(), `SELECT repository,host_key FROM git_deploy_keys WHERE workload_id=$1`, r.PathValue("id")).Scan(&repository, &host)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "deploy key unavailable", 503)
			return
		}
		if err == nil && (repository != in.Source.Repository || host != in.Source.SSHHostKey) {
			http.Error(w, "use Rotate key in SSH deploy key settings to change its repository or host pin", 409)
			return
		}
	}
	if in.EnvironmentPatch != nil {
		if in.Environment == nil {
			in.Environment = map[string]string{}
		}
		for key, value := range in.EnvironmentPatch {
			if value == nil {
				delete(in.Environment, key)
			} else {
				in.Environment[key] = *value
			}
		}
		in.EnvironmentPatch = nil
	}
	if err = validateWorkload(in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if in.Kind == "" {
		in.Kind = "service"
	}
	data, err := json.Marshal(in)
	if err != nil {
		http.Error(w, "invalid configuration", 400)
		return
	}
	encrypted, err = seal(s.Key, data)
	if err != nil {
		http.Error(w, "could not protect configuration", 500)
		return
	}
	cpu, memory := configuredUsage(in)
	_, err = tx.Exec(r.Context(), "UPDATE workloads SET name=$1,configuration=$2,kind=$4,configured_cpu=$5,configured_memory_mb=$6,job_cron=CASE WHEN $4='job' THEN job_cron ELSE '' END,next_job_at=CASE WHEN $4='job' THEN next_job_at ELSE NULL END WHERE id=$3", in.Name, encrypted, r.PathValue("id"), in.Kind, cpu, memory)
	if quotaError(w, err) {
		return
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save workload", 500)
		return
	}
	respond(w, 200, map[string]string{"id": r.PathValue("id")})
}
