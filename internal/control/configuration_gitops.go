package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

type gitOpsInput struct {
	WorkloadID string `json:"workload_id"`
	Path       string `json:"path"`
	Automatic  bool   `json:"automatic"`
}

func (s *Server) projectGitOps(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("id")
	level := 1
	if r.Method != "GET" {
		level = 3
	}
	if !s.requireAccess(w, r, "project", project, level) {
		return
	}
	if r.Method == "DELETE" {
		if _, err := s.DB.Exec(r.Context(), `DELETE FROM configuration_gitops WHERE project_id=$1 AND team_id=$2`, project, actor(r).Team); err != nil {
			http.Error(w, "could not disconnect GitOps source", 503)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	if r.Method == "PUT" {
		var in gitOpsInput
		if !decode(w, r, &in) {
			return
		}
		if in.Path == "" {
			in.Path = "cove.yaml"
		}
		if !s.requireAccess(w, r, "workload", in.WorkloadID, 3) || !s.requirePersistentResource(w, r, in.WorkloadID) {
			return
		}
		var encrypted []byte
		var capable bool
		if err := s.DB.QueryRow(r.Context(), `SELECT w.configuration,COALESCE((n.capabilities->>'source_files')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 AND w.project_id=$2 AND w.team_id=$3`, in.WorkloadID, project, actor(r).Team).Scan(&encrypted, &capable); err != nil {
			http.Error(w, "select a persistent Git resource in this project", 400)
			return
		}
		var config workloadInput
		if s.unsealJSON(encrypted, &config) != nil || config.Source == nil {
			http.Error(w, "select a resource with a Git source", 400)
			return
		}
		source := *config.Source
		source.InlineCompose, source.ComposePath = false, in.Path
		if err := worker.ValidateRequest(worker.Request{ID: "validation", Workload: "validation", Action: "deploy", Source: &source}); err != nil {
			http.Error(w, "select a repository-relative cove.yaml path", 400)
			return
		}
		if !capable {
			http.Error(w, "upgrade and verify the worker before connecting a GitOps source", 409)
			return
		}
		if in.Automatic && actor(r).Email == "" {
			http.Error(w, "automatic GitOps requires an individual verified account", 409)
			return
		}
		if _, err := s.DB.Exec(r.Context(), `INSERT INTO configuration_gitops(project_id,team_id,workload_id,path,automatic,creator_id) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(project_id) DO UPDATE SET workload_id=$3,path=$4,automatic=$5,creator_id=$6,updated_at=now(),next_scan_at=now(),last_revision='',desired_payload=NULL,last_draft_id=NULL,last_error=''`, project, actor(r).Team, in.WorkloadID, in.Path, in.Automatic, actor(r).ID); err != nil {
			http.Error(w, "could not save GitOps source", 503)
			return
		}
	}
	var input gitOpsInput
	var revision, lastError, draft string
	var synced *time.Time
	var desired []byte
	err := s.DB.QueryRow(r.Context(), `SELECT workload_id,path,automatic,last_revision,last_error,COALESCE((SELECT id FROM configuration_drafts WHERE id=configuration_gitops.last_draft_id AND applied_at IS NULL),''),synced_at,desired_payload FROM configuration_gitops WHERE project_id=$1 AND team_id=$2`, project, actor(r).Team).Scan(&input.WorkloadID, &input.Path, &input.Automatic, &revision, &lastError, &draft, &synced, &desired)
	if errors.Is(err, pgx.ErrNoRows) {
		respond(w, 200, map[string]any{"connected": false})
		return
	}
	if err != nil {
		http.Error(w, "GitOps source unavailable", 503)
		return
	}
	changes := []configurationChange{}
	if len(desired) > 0 {
		var manifest projectConfiguration
		if s.unsealJSON(desired, &manifest) != nil {
			http.Error(w, "GitOps declaration unavailable", 503)
			return
		}
		tx, err := s.DB.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			http.Error(w, "GitOps drift unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		current, err := s.readProjectConfiguration(r.Context(), tx, actor(r).Team, project)
		if err != nil {
			http.Error(w, "GitOps drift unavailable", 503)
			return
		}
		changes, err = projectConfigurationChanges(current, manifest)
		if err != nil {
			http.Error(w, "GitOps route protections are ambiguous; review route settings", 409)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, map[string]any{"connected": true, "source": input, "last_revision": revision, "last_error": lastError, "draft_id": draft, "synced_at": synced, "drift": changes})
}

func (s *Server) syncProjectGitOps(w http.ResponseWriter, r *http.Request) {
	project, team := r.PathValue("id"), actor(r).Team
	if !s.requireAccess(w, r, "project", project, 3) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "GitOps source unavailable", 503)
		return
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('cove-gitops:'||$1,0))`, project).Scan(&locked); err != nil || !locked {
		http.Error(w, "another GitOps sync is running", 409)
		return
	}
	var workload, selectedPath, oldDraft, oldRevision string
	var automatic bool
	if err = tx.QueryRow(ctx, `SELECT workload_id,path,automatic,COALESCE(last_draft_id,''),last_revision FROM configuration_gitops WHERE project_id=$1 AND team_id=$2 FOR UPDATE`, project, team).Scan(&workload, &selectedPath, &automatic, &oldDraft, &oldRevision); err != nil {
		http.Error(w, "connect a GitOps source first", 404)
		return
	}
	if !s.requireAccess(w, r, "workload", workload, 3) || !s.requirePersistentResource(w, r, workload) {
		return
	}
	var encrypted []byte
	var node string
	var capable bool
	if err = tx.QueryRow(ctx, `SELECT w.configuration,w.node_id,COALESCE((n.capabilities->>'source_files')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 AND w.project_id=$2 AND w.team_id=$3`, workload, project, team).Scan(&encrypted, &node, &capable); err != nil || !capable {
		http.Error(w, "GitOps source moved or its worker requires an upgrade and verify", 409)
		return
	}
	var config workloadInput
	if s.unsealJSON(encrypted, &config) != nil || config.Source == nil {
		http.Error(w, "GitOps resource no longer has a repository", 409)
		return
	}
	source := *config.Source
	source.InlineCompose, source.ComposePath = false, selectedPath
	request := worker.Request{ID: resourceID(), Workload: workload, Action: "deploy", Source: &source}
	if err = s.prepareAppDeployment(ctx, &request); err != nil {
		http.Error(w, "GitOps repository access unavailable", 409)
		return
	}
	response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "source_files", Request: &request})
	if err != nil || response.SourceFiles == nil {
		http.Error(w, "could not fetch cove.yaml; check source access and worker connectivity", 502)
		return
	}
	if !gitCommit.MatchString(response.SourceFiles.Revision) {
		http.Error(w, "worker returned an invalid GitOps revision", 502)
		return
	}
	manifest, err := parseProjectConfiguration(response.SourceFiles.Files[selectedPath])
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	current, err := s.readProjectConfiguration(ctx, tx, team, project)
	if err != nil {
		http.Error(w, "project configuration unavailable", 503)
		return
	}
	changes, err := projectConfigurationChanges(current, manifest)
	if err != nil {
		http.Error(w, "GitOps route protections are ambiguous; review route settings", 409)
		return
	}
	draft := ""
	if len(changes) > 0 {
		input, err := json.Marshal(map[string]string{"content": response.SourceFiles.Files[selectedPath]})
		if err != nil {
			http.Error(w, "could not encode GitOps declaration", 500)
			return
		}
		staging := r.Clone(ctx)
		staging.Method = "POST"
		staging.Body = io.NopCloser(bytes.NewReader(input))
		capture := &apiCapture{headers: http.Header{}}
		s.configurationDrafts(capture, staging)
		if capture.status != 201 || capture.failed {
			http.Error(w, "could not stage GitOps diff; check manifest limits and current project access", 409)
			return
		}
		var created apiID
		if json.Unmarshal([]byte(capture.body.String()), &created) != nil || created.ID == "" {
			http.Error(w, "could not read staged GitOps diff", 503)
			return
		}
		draft = created.ID
	}
	sealed, err := s.sealJSON(manifest)
	if err != nil {
		http.Error(w, "could not protect GitOps declaration", 500)
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE configuration_gitops SET last_revision=$2,desired_payload=$3,last_draft_id=NULLIF($4,''),last_error='',synced_at=now(),next_scan_at=now()+interval '1 minute' WHERE project_id=$1`, project, response.SourceFiles.Revision, sealed, draft); err == nil && oldDraft != "" && oldDraft != draft {
		_, err = tx.Exec(ctx, `DELETE FROM configuration_drafts WHERE id=$1 AND project_id=$2 AND applied_at IS NULL`, oldDraft, project)
	}
	if err == nil && draft != "" && (oldDraft == "" || oldRevision != response.SourceFiles.Revision) {
		_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'GitOps diff staged',$2,200,$3)`, "user "+actor(r).ID, project, team)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		http.Error(w, "could not save GitOps sync", 503)
		return
	}
	state := "unchanged"
	if draft != "" {
		state = "pending"
		if automatic {
			applying := r.Clone(ctx)
			applying.SetPathValue("draft", draft)
			capture := &apiCapture{headers: http.Header{}}
			s.applyConfigurationDraft(capture, applying)
			applying.Pattern = "POST /api/projects/{id}/configuration/drafts/{draft}/apply"
			s.recordAudit(applying, "GitOps user "+actor(r).ID, capture.status)
			if capture.status != 200 {
				http.Error(w, "GitOps diff is pending; automatic apply was rejected by current permissions or configuration checks", 409)
				return
			}
			state = "saved"
		}
	}
	respond(w, 200, map[string]any{"state": state, "draft_id": draft, "changes": changes, "revision": response.SourceFiles.Revision})
}

func (s *Server) reconcileProjectGitOps(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `UPDATE configuration_gitops SET next_scan_at=now()+interval '1 minute' WHERE project_id IN (SELECT project_id FROM configuration_gitops WHERE automatic AND next_scan_at<=now() ORDER BY next_scan_at LIMIT 2 FOR UPDATE SKIP LOCKED) RETURNING project_id,team_id,creator_id`)
	if err != nil {
		return err
	}
	type source struct{ project, team, owner string }
	items := []source{}
	for rows.Next() {
		var item source
		if err = rows.Scan(&item.project, &item.team, &item.owner); err != nil {
			break
		}
		items = append(items, item)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, item := range items {
		request, err := http.NewRequestWithContext(ctx, "POST", "/api/projects/"+item.project+"/configuration/gitops/sync", http.NoBody)
		if err != nil {
			return err
		}
		request.SetPathValue("id", item.project)
		request.Header.Set("X-Cove-Team", item.team)
		person, authErr := s.identity(request, item.owner)
		capture := &apiCapture{headers: http.Header{}}
		if authErr == nil && person.Email != "" {
			request = request.WithContext(context.WithValue(request.Context(), identityKey{}, person))
			if s.authorize(capture, request) {
				s.syncProjectGitOps(capture, request)
			}
		}
		lastError := ""
		if authErr != nil || person == nil || person.Email == "" || capture.status != 200 || capture.failed {
			lastError = "GitOps sync failed; check the owner's current grants, repository access, manifest and worker readiness."
		}
		if _, err = s.DB.Exec(ctx, `UPDATE configuration_gitops SET last_error=$2 WHERE project_id=$1`, item.project, strings.TrimSpace(lastError)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) runProjectGitOps(ctx context.Context) error {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		if err := s.reconcileProjectGitOps(ctx); err != nil && ctx.Err() == nil {
			logPeriodicError(ctx, "reconcileProjectGitOps", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
	}
}
