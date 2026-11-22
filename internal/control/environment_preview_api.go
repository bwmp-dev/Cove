package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type previewBranchView struct {
	RepositoryID int64      `json:"repository_id"`
	Repository   string     `json:"repository"`
	Branch       string     `json:"branch"`
	Head         string     `json:"head"`
	CheckedAt    *time.Time `json:"checked_at"`
}

type previewResourceView struct {
	BaseID           string          `json:"base_id"`
	ResourceID       string          `json:"resource_id"`
	Name             string          `json:"name"`
	Kind             string          `json:"kind"`
	Source           string          `json:"source"`
	Revision         string          `json:"revision"`
	BaselineRevision string          `json:"baseline_revision"`
	BaselineRelease  string          `json:"baseline_release"`
	BaselineAt       *time.Time      `json:"baseline_deployed_at"`
	Deployed         bool            `json:"deployed"`
	DataState        string          `json:"data_state"`
	SnapshotAt       *time.Time      `json:"snapshot_at"`
	DataReport       json.RawMessage `json:"data_report"`
	Routes           []string        `json:"routes"`
	Published        bool            `json:"published"`
	Operation        string          `json:"operation"`
}

type previewView struct {
	ID            string                `json:"id"`
	ProjectID     string                `json:"project_id"`
	EnvironmentID string                `json:"environment_id"`
	BaseID        string                `json:"base_environment_id"`
	Suffix        string                `json:"suffix"`
	Slug          string                `json:"slug"`
	State         string                `json:"state"`
	Stage         string                `json:"stage"`
	Detail        string                `json:"detail"`
	Pinned        bool                  `json:"pinned"`
	Access        string                `json:"access"`
	DeleteReason  string                `json:"delete_reason"`
	LastActivity  time.Time             `json:"last_activity_at"`
	ExpiresAt     *time.Time            `json:"expires_at"`
	CreatedAt     time.Time             `json:"created_at"`
	Branches      []previewBranchView   `json:"branches"`
	Resources     []previewResourceView `json:"resources,omitempty"`
	Permissions   []string              `json:"permissions"`
}

func (s *Server) previewViews(r *http.Request, project, id string) ([]previewView, error) {
	p := actor(r)
	rows, err := s.DB.Query(r.Context(), `SELECT p.id,p.project_id,COALESCE(p.environment_id,''),p.base_environment_id,p.suffix,p.slug,p.state,p.stage,p.detail,p.pinned,p.delete_reason,p.last_activity_at,p.created_at,p.configuration,
 CASE WHEN $3 THEN 3 ELSE cove_access($4,p.team_id,p.project_id,p.base_environment_id) END
 FROM environment_previews p WHERE p.team_id=$1 AND ($2='' OR p.project_id=$2) AND ($5='' OR p.id=$5) AND ($3 OR cove_access($4,p.team_id,p.project_id,p.base_environment_id)>0) ORDER BY p.created_at DESC`, p.Team, project, p.Admin, p.ID, id)
	if err != nil {
		return nil, err
	}
	views := []previewView{}
	for rows.Next() {
		var v previewView
		var encrypted []byte
		var level int
		if err = rows.Scan(&v.ID, &v.ProjectID, &v.EnvironmentID, &v.BaseID, &v.Suffix, &v.Slug, &v.State, &v.Stage, &v.Detail, &v.Pinned, &v.DeleteReason, &v.LastActivity, &v.CreatedAt, &encrypted, &level); err != nil {
			rows.Close()
			return nil, err
		}
		var snapshot previewSnapshot
		if err = s.unsealJSON(encrypted, &snapshot); err != nil {
			rows.Close()
			return nil, err
		}
		v.Access = snapshot.Recipe.Access
		if !v.Pinned && v.State != "deleting" {
			expires := v.LastActivity.Add(time.Duration(snapshot.Recipe.TTLHours) * time.Hour)
			v.ExpiresAt = &expires
		}
		v.Permissions = permissions(level)
		v.Branches = []previewBranchView{}
		views = append(views, v)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i := range views {
		branches, err := s.DB.Query(r.Context(), `SELECT repository_id,repository,branch,head,checked_at FROM environment_preview_branches WHERE preview_id=$1 ORDER BY repository`, views[i].ID)
		if err != nil {
			return nil, err
		}
		for branches.Next() {
			var b previewBranchView
			if err = branches.Scan(&b.RepositoryID, &b.Repository, &b.Branch, &b.Head, &b.CheckedAt); err != nil {
				branches.Close()
				return nil, err
			}
			views[i].Branches = append(views[i].Branches, b)
		}
		branches.Close()
		if err = branches.Err(); err != nil {
			return nil, err
		}
	}
	return views, nil
}

func (s *Server) environmentPreviews(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("id")
	var exists bool
	if err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1 AND team_id=$2)`, project, actor(r).Team).Scan(&exists); err != nil {
		http.Error(w, "project unavailable", 503)
		return
	}
	if !exists {
		http.NotFound(w, r)
		return
	}
	if r.Method == "GET" {
		views, err := s.previewViews(r, project, "")
		if err != nil {
			http.Error(w, "previews unavailable", 503)
			return
		}
		type waiting struct {
			RepositoryID int64     `json:"repository_id"`
			Branch       string    `json:"branch"`
			Reason       string    `json:"reason"`
			ReceivedAt   time.Time `json:"received_at"`
		}
		pending := []waiting{}
		if level, err := s.access(r, "project", project); err == nil && level >= 1 {
			rows, err := s.DB.Query(r.Context(), `SELECT repository_id,branch,reason,received_at FROM preview_branch_signals WHERE project_id=$1 AND reason<>'' ORDER BY received_at`, project)
			if err != nil {
				http.Error(w, "previews unavailable", 503)
				return
			}
			for rows.Next() {
				var item waiting
				if err = rows.Scan(&item.RepositoryID, &item.Branch, &item.Reason, &item.ReceivedAt); err != nil {
					rows.Close()
					http.Error(w, "previews unavailable", 503)
					return
				}
				pending = append(pending, item)
			}
			rows.Close()
		}
		respond(w, 200, map[string]any{"previews": views, "waiting": pending})
		return
	}
	if !s.requireAccess(w, r, "project", project, 3) {
		return
	}
	var in struct {
		Suffix string `json:"suffix"`
	}
	if !decode(w, r, &in) {
		return
	}
	recipe, _, saved, err := s.loadPreviewRecipe(r.Context(), s.DB, project)
	if err != nil {
		http.Error(w, "preview recipe unavailable", 503)
		return
	}
	if !saved || !recipe.Enabled || len(recipe.Repositories) == 0 {
		http.Error(w, "enable the preview recipe and select at least one repository first", 409)
		return
	}
	branch := previewPrefix(recipe.BranchPattern) + strings.TrimSpace(in.Suffix)
	if strings.TrimSpace(in.Suffix) == "" || !validPreviewBranch(branch) {
		http.Error(w, "enter the part of the branch name after "+previewPrefix(recipe.BranchPattern), 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	for _, repository := range recipe.Repositories {
		if _, err = tx.Exec(r.Context(), `INSERT INTO preview_branch_signals(project_id,repository_id,branch,approved_by) VALUES($1,$2,$3,$4) ON CONFLICT(project_id,repository_id,branch) DO UPDATE SET approved_by=EXCLUDED.approved_by,next_attempt_at=now(),attempts=0,reason=''`, project, repository, branch, actor(r).ID); err != nil {
			http.Error(w, "could not request preview", 503)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not request preview", 503)
		return
	}
	respond(w, 202, map[string]string{"branch": branch})
}

// environmentPreview serves one preview. Access follows the preview's production environment.
func (s *Server) environmentPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var base string
	err := s.DB.QueryRow(r.Context(), `SELECT base_environment_id FROM environment_previews WHERE id=$1 AND team_id=$2`, id, actor(r).Team).Scan(&base)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "preview unavailable", 503)
		return
	}
	action := r.PathValue("action")
	needed := 1
	switch {
	case r.Method == "GET" && action == "":
	case action == "retry" || action == "redeploy":
		needed = 2
	default:
		needed = 3
	}
	if !s.requireAccess(w, r, "environment", base, needed) {
		return
	}
	switch {
	case r.Method == "GET" && action == "":
		s.previewDetail(w, r, id)
	case r.Method == "GET" && action == "credentials":
		s.previewCredentialsHandler(w, r, id)
	case r.Method == "POST" && action == "approve":
		s.previewTransition(w, r, id, func(p *previewRow) (string, string, string, int) {
			if p.State != "awaiting_approval" {
				return "", "", "the preview is not waiting for approval", 409
			}
			return "creating", "plan", "", 0
		}, "approved")
	case r.Method == "POST" && action == "retry":
		s.previewTransition(w, r, id, func(p *previewRow) (string, string, string, int) {
			if p.State != "failed" {
				return "", "", "only failed previews can be retried", 409
			}
			state := "updating"
			if p.Environment == "" || p.Stage == "data" || p.Stage == "connect" {
				state = "creating"
			}
			return state, p.Stage, "", 0
		}, "retried")
	case r.Method == "POST" && action == "redeploy":
		s.previewTransition(w, r, id, func(p *previewRow) (string, string, string, int) {
			if p.State != "ready" && p.State != "failed" {
				return "", "", "wait until the preview is ready or failed", 409
			}
			if p.Environment == "" {
				return "", "", "the preview has not been created yet", 409
			}
			return "updating", "deploy", "redeploy", 0
		}, "redeploy requested")
	case r.Method == "POST" && action == "refresh":
		s.previewRefresh(w, r, id)
	case r.Method == "PUT" && action == "":
		var in struct {
			Pinned bool `json:"pinned"`
		}
		if !decode(w, r, &in) {
			return
		}
		tag, err := s.DB.Exec(r.Context(), `UPDATE environment_previews SET pinned=$2,last_activity_at=now(),next_attempt_at=now() WHERE id=$1 AND state<>'deleting'`, id, in.Pinned)
		if err != nil {
			http.Error(w, "could not update preview", 503)
			return
		}
		if tag.RowsAffected() == 0 {
			http.Error(w, "the preview is being deleted", 409)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	case r.Method == "DELETE" && action == "":
		var in struct {
			Confirm string `json:"confirm"`
		}
		if !decode(w, r, &in) {
			return
		}
		var slug string
		if err := s.DB.QueryRow(r.Context(), `SELECT slug FROM environment_previews WHERE id=$1`, id).Scan(&slug); err != nil {
			http.Error(w, "preview unavailable", 503)
			return
		}
		if in.Confirm != slug {
			http.Error(w, "type the preview name to confirm deletion", 400)
			return
		}
		if err := s.requestPreviewDeletion(r.Context(), s.DB, id, "deleted by user "+actor(r).ID); err != nil {
			http.Error(w, "could not delete preview", 503)
			return
		}
		respond(w, 202, map[string]bool{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) previewTransition(w http.ResponseWriter, r *http.Request, id string, decide func(*previewRow) (string, string, string, int), audit string) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	p, err := s.loadPreviewRow(r.Context(), tx, id, true)
	if err != nil {
		http.Error(w, "preview unavailable", 503)
		return
	}
	state, stage, message, status := decide(p)
	if status != 0 {
		http.Error(w, message, status)
		return
	}
	if message == "redeploy" {
		if _, err = tx.Exec(r.Context(), `UPDATE environment_preview_resources SET deployed_key='' WHERE preview_id=$1`, id); err != nil {
			http.Error(w, "could not redeploy", 503)
			return
		}
	}
	if audit == "retried" {
		if _, err = tx.Exec(r.Context(), `UPDATE environment_preview_resources SET data_state='pending',data_operation='' WHERE preview_id=$1 AND data_state='failed'`, id); err != nil {
			http.Error(w, "could not retry", 503)
			return
		}
		if _, err = tx.Exec(r.Context(), `UPDATE environment_preview_resources SET deploying_key='',deploy_operation='' WHERE preview_id=$1 AND deployed_key<>deploying_key`, id); err != nil {
			http.Error(w, "could not retry", 503)
			return
		}
		if _, err = tx.Exec(r.Context(), `UPDATE environment_preview_resources r SET routes_operation='' FROM operations o WHERE r.preview_id=$1 AND o.id=r.routes_operation AND o.state NOT IN ('queued','running','succeeded')`, id); err != nil {
			http.Error(w, "could not retry", 503)
			return
		}
	}
	if _, err = tx.Exec(r.Context(), `UPDATE environment_previews SET state=$2,stage=$3,detail='',updated_at=now(),next_attempt_at=now(),last_activity_at=now() WHERE id=$1`, id, state, stage); err != nil {
		http.Error(w, "could not update preview", 503)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,$2,$3,202,$4)`, "user "+actor(r).ID, "environment preview "+audit, id, actor(r).Team); err != nil {
		http.Error(w, "could not update preview", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not update preview", 503)
		return
	}
	respond(w, 202, map[string]bool{"ok": true})
}

// previewRefresh replaces the preview's copied data with a fresh production copy. It requires the
// preview name as confirmation because it discards everything written to the preview since.
func (s *Server) previewRefresh(w http.ResponseWriter, r *http.Request, id string) {
	var in struct {
		Confirm string `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	p, err := s.loadPreviewRow(r.Context(), tx, id, true)
	if err != nil {
		http.Error(w, "preview unavailable", 503)
		return
	}
	if in.Confirm != p.Slug {
		http.Error(w, "type the preview name to confirm replacing its data", 400)
		return
	}
	if p.State != "ready" && p.State != "failed" {
		http.Error(w, "wait until the preview is ready or failed", 409)
		return
	}
	if p.Environment == "" {
		http.Error(w, "the preview has no data yet", 409)
		return
	}
	var active bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations o JOIN environment_preview_resources r ON r.resource_id=o.workload_id WHERE r.preview_id=$1 AND o.state IN ('queued','running'))`, id).Scan(&active); err != nil {
		http.Error(w, "operations unavailable", 503)
		return
	}
	if active {
		http.Error(w, "wait for the preview's operations to finish", 409)
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE environment_preview_resources SET data_state='pending',data_operation='' WHERE preview_id=$1 AND data_state<>'none'`, id); err != nil {
		http.Error(w, "could not refresh", 503)
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE environment_previews SET state='updating',stage='data',refresh_scope='data',detail='',updated_at=now(),next_attempt_at=now(),last_activity_at=now() WHERE id=$1`, id); err != nil {
		http.Error(w, "could not refresh", 503)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'environment preview data refresh from production',$2,202,$3)`, "user "+actor(r).ID, id, actor(r).Team); err != nil {
		http.Error(w, "could not refresh", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not refresh", 503)
		return
	}
	respond(w, 202, map[string]bool{"ok": true})
}

func (s *Server) previewCredentialsHandler(w http.ResponseWriter, r *http.Request, id string) {
	p, err := s.loadPreviewRow(r.Context(), s.DB, id, false)
	if err != nil {
		http.Error(w, "preview unavailable", 503)
		return
	}
	if p.Snapshot.Username == "" {
		http.Error(w, "this preview is public", 404)
		return
	}
	if _, err = s.DB.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'environment preview credentials viewed',$2,200,$3)`, "user "+actor(r).ID, id, actor(r).Team); err != nil {
		http.Error(w, "preview unavailable", 503)
		return
	}
	respond(w, 200, map[string]string{"username": p.Snapshot.Username, "password": p.Snapshot.Password})
}

func (s *Server) previewDetail(w http.ResponseWriter, r *http.Request, id string) {
	views, err := s.previewViews(r, "", id)
	if err != nil {
		http.Error(w, "preview unavailable", 503)
		return
	}
	if len(views) != 1 {
		http.NotFound(w, r)
		return
	}
	view := views[0]
	resources, err := s.loadPreviewResources(r.Context(), s.DB, id)
	if err != nil {
		http.Error(w, "preview unavailable", 503)
		return
	}
	view.Resources = []previewResourceView{}
	for _, resource := range resources {
		var report []byte
		var snapshot, baselineAt *time.Time
		if err = s.DB.QueryRow(r.Context(), `SELECT COALESCE(data_report,'null'::jsonb),snapshot_at,baseline_deployed_at FROM environment_preview_resources WHERE preview_id=$1 AND base_id=$2`, id, resource.BaseID).Scan(&report, &snapshot, &baselineAt); err != nil {
			http.Error(w, "preview unavailable", 503)
			return
		}
		item := previewResourceView{BaseID: resource.BaseID, ResourceID: resource.ResourceID, Name: resource.Name, Kind: resource.Kind, BaselineRevision: resource.BaselineRevision, BaselineRelease: resource.BaselineRelease, BaselineAt: baselineAt, DataState: resource.DataState, SnapshotAt: snapshot, DataReport: report, Routes: []string{}, Deployed: resource.Kind != "job" && resource.DeployedKey == resource.desiredKey(), Operation: resource.DeployOperation}
		item.Source, item.Revision = "baseline", resource.BaselineRevision
		if resource.BranchRevision != "" {
			item.Source, item.Revision = "branch", resource.BranchRevision
		}
		if resource.Kind == "job" {
			item.Source = "disabled"
		}
		for _, route := range resource.Routes {
			item.Routes = append(item.Routes, route.Domain)
		}
		if resource.RoutesOperation != "" {
			state, _, err := s.operationState(r.Context(), resource.RoutesOperation)
			item.Published = err == nil && state == "succeeded"
		}
		view.Resources = append(view.Resources, item)
	}
	respond(w, 200, view)
}
