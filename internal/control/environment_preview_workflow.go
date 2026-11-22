package control

import (
	"context"
	"cove/internal/worker"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

// previewSnapshot is the recipe a preview was created with, plus its access credentials. Later
// recipe edits apply to new previews; an existing preview keeps its data and routing decisions.
type previewSnapshot struct {
	Recipe   previewRecipe `json:"recipe"`
	Username string        `json:"username,omitempty"`
	Password string        `json:"password,omitempty"`
	Auth     string        `json:"auth,omitempty"`
}

// previewBaseline pins what production ran when the preview was created. Resources without a
// matching branch, and resources whose branch is deleted, run exactly this.
type previewBaseline struct {
	Source  *worker.GitSource `json:"source,omitempty"`
	Compose string            `json:"compose,omitempty"`
	Images  map[string]string `json:"images,omitempty"`
}

type previewRow struct {
	ID, Project, Team, Base, Environment, Suffix, Slug, State, Stage, Detail, DeleteReason, RefreshScope string
	Snapshot                                                                                             previewSnapshot
	Pinned                                                                                               bool
	LastActivity                                                                                         time.Time
}

type previewResourceRow struct {
	BaseID, ResourceID, Name, Kind, Node      string
	Position                                  int
	Repository                                int64
	Baseline                                  previewBaseline
	BaselineRevision, BaselineRelease         string
	BranchRevision, DeployedKey, DeployingKey string
	Isolation                                 worker.EnvironmentPreview
	DataState, DataOperation, DeployOperation string
	RoutesOperation, RemoveOperation          string
	Routes                                    []worker.Route
}

func (r previewResourceRow) desiredKey() string {
	if r.BranchRevision != "" {
		return "branch:" + r.BranchRevision
	}
	return "baseline:" + r.BaselineRelease
}

func (s *Server) sealJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return seal(s.Key, raw)
}

func (s *Server) unsealJSON(encrypted []byte, value any) error {
	raw, err := unseal(s.Key, encrypted)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}

func (s *Server) loadPreviewRow(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string, lock bool) (*previewRow, error) {
	query := `SELECT id,project_id,team_id,base_environment_id,COALESCE(environment_id,''),suffix,slug,state,stage,detail,delete_reason,refresh_scope,configuration,pinned,last_activity_at FROM environment_previews WHERE id=$1`
	if lock {
		query += " FOR UPDATE"
	}
	var p previewRow
	var encrypted []byte
	if err := q.QueryRow(ctx, query, id).Scan(&p.ID, &p.Project, &p.Team, &p.Base, &p.Environment, &p.Suffix, &p.Slug, &p.State, &p.Stage, &p.Detail, &p.DeleteReason, &p.RefreshScope, &encrypted, &p.Pinned, &p.LastActivity); err != nil {
		return nil, err
	}
	if err := s.unsealJSON(encrypted, &p.Snapshot); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Server) loadPreviewResources(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, id string) ([]previewResourceRow, error) {
	rows, err := q.Query(ctx, `SELECT r.base_id,r.resource_id,r.name,r.kind,w.node_id,r.position,r.repository_id,r.baseline,r.baseline_revision,r.baseline_release,r.branch_revision,r.deployed_key,r.deploying_key,r.isolation,r.data_state,r.data_operation,r.deploy_operation,r.routes_operation,r.remove_operation,r.routes FROM environment_preview_resources r JOIN workloads w ON w.id=r.resource_id WHERE r.preview_id=$1 ORDER BY r.position,r.name,r.base_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []previewResourceRow{}
	for rows.Next() {
		var item previewResourceRow
		var baseline, isolation, routes []byte
		if err = rows.Scan(&item.BaseID, &item.ResourceID, &item.Name, &item.Kind, &item.Node, &item.Position, &item.Repository, &baseline, &item.BaselineRevision, &item.BaselineRelease, &item.BranchRevision, &item.DeployedKey, &item.DeployingKey, &isolation, &item.DataState, &item.DataOperation, &item.DeployOperation, &item.RoutesOperation, &item.RemoveOperation, &routes); err != nil {
			return nil, err
		}
		if err = s.unsealJSON(baseline, &item.Baseline); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(isolation, &item.Isolation); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(routes, &item.Routes); err != nil {
			return nil, err
		}
		list = append(list, item)
	}
	return list, rows.Err()
}

func previewCredentials() (string, string, string, error) {
	random := make([]byte, 18)
	if _, err := rand.Read(random); err != nil {
		return "", "", "", err
	}
	password := base64.RawURLEncoding.EncodeToString(random)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		return "", "", "", err
	}
	return "preview", password, "preview:" + string(hash), nil
}

// createPreviewRecord inserts a preview and its branch rows. It is idempotent per project and
// suffix: a concurrent or repeated request returns the existing preview.
func (s *Server) createPreviewRecord(ctx context.Context, tx pgx.Tx, project string, recipe previewRecipe, version int64, suffix string, branches map[int64]previewBranchState, approvedBy string) (string, bool, error) {
	var existing string
	err := tx.QueryRow(ctx, `SELECT id FROM environment_previews WHERE project_id=$1 AND suffix=$2`, project, suffix).Scan(&existing)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	var team string
	if err = tx.QueryRow(ctx, `SELECT team_id FROM projects WHERE id=$1`, project).Scan(&team); err != nil {
		return "", false, err
	}
	taken := map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT slug FROM environment_previews WHERE project_id=$1`, project)
	if err != nil {
		return "", false, err
	}
	for rows.Next() {
		var slug string
		if err = rows.Scan(&slug); err != nil {
			rows.Close()
			return "", false, err
		}
		taken[slug] = true
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return "", false, err
	}
	slug := previewSlug(suffix, func(candidate string) bool { return taken[candidate] })
	if taken[slug] {
		return "", false, errors.New("could not derive a unique preview name")
	}
	snapshot := previewSnapshot{Recipe: recipe}
	if recipe.Access == "private" {
		snapshot.Username, snapshot.Password, snapshot.Auth, err = previewCredentials()
		if err != nil {
			return "", false, err
		}
	}
	encrypted, err := s.sealJSON(snapshot)
	if err != nil {
		return "", false, err
	}
	id := resourceID()
	state, stage := "creating", "plan"
	if recipe.RequireApproval && approvedBy == "" {
		state, stage = "awaiting_approval", "approval"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO environment_previews(id,project_id,team_id,base_environment_id,suffix,slug,state,stage,configuration,recipe_version,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id, project, team, recipe.BaseEnvironment, suffix, slug, state, stage, encrypted, version, approvedBy); err != nil {
		return "", false, err
	}
	for repository, branch := range branches {
		if _, err = tx.Exec(ctx, `INSERT INTO environment_preview_branches(preview_id,repository_id,repository,branch,head,checked_at) VALUES($1,$2,$3,$4,$5,now())`, id, repository, branch.Repository, branch.Branch, branch.Head); err != nil {
			return "", false, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM preview_branch_dismissals WHERE project_id=$1 AND repository_id=$2 AND branch=$3`, project, repository, branch.Branch); err != nil {
			return "", false, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'environment preview created',$2,201,$3)`, previewActor(approvedBy), id, team); err != nil {
		return "", false, err
	}
	return id, true, nil
}

func previewActor(user string) string {
	if user == "" {
		return "GitHub webhook"
	}
	return "user " + user
}

// reconcileEnvironmentPreviews drives every preview through its recorded stages. Each step
// re-reads its state, so a control plane restart or a repeated webhook resumes rather than repeats.
func (s *Server) reconcileEnvironmentPreviews(ctx context.Context) error {
	if err := s.scanPreviewRecipes(ctx); err != nil {
		slog.Error("environment preview branch scan failed", "error", err)
	}
	if err := s.processPreviewSignals(ctx); err != nil {
		return err
	}
	rows, err := s.DB.Query(ctx, `SELECT id FROM environment_previews WHERE next_attempt_at<=now() AND (state IN ('creating','updating','deleting','awaiting_approval') OR (state='ready' AND NOT pinned) OR (state='failed' AND NOT pinned AND stage<>'remove')) ORDER BY next_attempt_at LIMIT 20`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.advancePreview(ctx, id); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("environment preview step failed", "preview", id, "error", err)
			if _, updateErr := s.DB.Exec(ctx, `UPDATE environment_previews SET next_attempt_at=now()+interval '30 seconds' WHERE id=$1`, id); updateErr != nil {
				return updateErr
			}
		}
	}
	return nil
}

func (s *Server) setPreviewState(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, id, state, stage, detail string, delay time.Duration) error {
	_, err := q.Exec(ctx, `UPDATE environment_previews SET state=$2,stage=$3,detail=$4,updated_at=now(),next_attempt_at=now()+$5*interval '1 second' WHERE id=$1`, id, state, stage, detail, int(delay.Seconds()))
	return err
}

func (s *Server) failPreview(ctx context.Context, id, stage, detail string) error {
	_, err := s.DB.Exec(ctx, `UPDATE environment_previews SET state='failed',stage=$2,detail=$3,updated_at=now(),next_attempt_at=now()+interval '1 minute' WHERE id=$1`, id, stage, detail)
	if err == nil {
		_, err = s.DB.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status,team_id) SELECT 'Cove','environment preview failed at '||$2,id,500,team_id FROM environment_previews WHERE id=$1`, id, stage)
	}
	return err
}

func (s *Server) advancePreview(ctx context.Context, id string) error {
	p, err := s.loadPreviewRow(ctx, s.DB, id, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if p.State != "deleting" && !p.Pinned {
		expired := p.LastActivity.Add(time.Duration(p.Snapshot.Recipe.TTLHours) * time.Hour).Before(time.Now())
		if expired {
			return s.requestPreviewDeletion(ctx, s.DB, id, "inactive for "+fmt.Sprint(p.Snapshot.Recipe.TTLHours)+" hours")
		}
	}
	switch p.State {
	case "ready":
		// A branch update recorded while the preview was still publishing is deployed now.
		resources, err := s.loadPreviewResources(ctx, s.DB, id)
		if err != nil {
			return err
		}
		if previewDrifted(resources) {
			return s.markPreviewDrifted(ctx, id)
		}
		return s.setPreviewState(ctx, s.DB, id, "ready", "ready", p.Detail, 5*time.Minute)
	case "awaiting_approval":
		_, err = s.DB.Exec(ctx, `UPDATE environment_previews SET next_attempt_at=now()+interval '5 minutes' WHERE id=$1`, id)
		return err
	case "failed":
		// A failed preview waits for an explicit retry; only its expiry is checked here.
		_, err = s.DB.Exec(ctx, `UPDATE environment_previews SET next_attempt_at=now()+interval '5 minutes' WHERE id=$1`, id)
		return err
	}
	switch p.Stage {
	case "plan":
		return s.planPreviewStage(ctx, p)
	case "data":
		return s.dataPreviewStage(ctx, p)
	case "connect":
		return s.connectPreviewStage(ctx, p)
	case "deploy":
		return s.deployPreviewStage(ctx, p)
	case "publish":
		return s.publishPreviewStage(ctx, p)
	case "remove":
		return s.removePreviewStage(ctx, p)
	}
	return nil
}

func (s *Server) requestPreviewDeletion(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, id, reason string) error {
	tag, err := q.Exec(ctx, `UPDATE environment_previews SET state='deleting',stage='remove',delete_reason=$2,detail='',updated_at=now(),next_attempt_at=now() WHERE id=$1 AND state<>'deleting'`, id, reason)
	if err == nil && tag.RowsAffected() > 0 {
		_, err = q.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status,team_id) SELECT 'Cove','environment preview deletion: '||$2,id,202,team_id FROM environment_previews WHERE id=$1`, id, reason)
	}
	return err
}

// planPreviewStage pins production revisions and creates the preview environment and its resource
// records in one transaction, so a failure leaves either nothing or a complete set.
func (s *Server) planPreviewStage(ctx context.Context, p *previewRow) error {
	if p.Environment != "" {
		return s.setPreviewState(ctx, s.DB, p.ID, "creating", "data", "", 0)
	}
	recipe := p.Snapshot.Recipe
	sources, err := s.loadPreviewSources(ctx, p.Team, recipe.BaseEnvironment, true)
	if err != nil {
		return err
	}
	plan, err := s.planPreview(ctx, p.Team, recipe, sources)
	if err != nil {
		return err
	}
	if plan.Blocked {
		messages := []string{}
		transient := true
		for _, item := range plan.Resources {
			if item.Included {
				for _, b := range item.Blockers {
					messages = append(messages, item.Name+": "+b.Message)
					transient = transient && (b.Code == "node_unavailable" || b.Code == "inventory_unavailable")
				}
			}
		}
		if transient {
			// A disconnected node is not a recipe problem; the preview waits for it.
			return s.setPreviewState(ctx, s.DB, p.ID, "creating", "plan", "Waiting for a node. "+strings.Join(messages, " "), time.Minute)
		}
		return s.failPreview(ctx, p.ID, "plan", "The preview recipe has unresolved items. "+strings.Join(messages, " "))
	}
	plans := map[string]previewResourcePlan{}
	for _, item := range plan.Resources {
		plans[item.ID] = item
	}
	included := []*previewSource{}
	for _, src := range sources {
		if plans[src.ID].Included {
			included = append(included, src)
		}
	}
	if len(included) == 0 {
		return s.failPreview(ctx, p.ID, "plan", "The recipe includes no resources.")
	}
	// Pin the deployed production images before any record exists; export only reads the release.
	images := map[string]map[string]string{}
	for _, src := range included {
		if src.Kind == "job" || src.Release == "" {
			continue
		}
		call, cancel := context.WithTimeout(ctx, 3*time.Minute)
		response, err := s.nodeRPC(call, src.Node, worker.RPCRequest{Method: "export_release", Promotion: &worker.PromotionExport{Workload: src.ID, Release: src.Release}})
		cancel()
		if err != nil || len(response.Images) == 0 {
			var remote *RemoteError
			if err != nil && !errors.As(err, &remote) {
				return s.setPreviewState(ctx, s.DB, p.ID, "creating", "plan", fmt.Sprintf("Waiting for the node of %s to pin its deployed images.", src.Name), time.Minute)
			}
			return s.failPreview(ctx, p.ID, "plan", fmt.Sprintf("Could not pin the deployed images of %s. Wait for its active operation to finish and retry.", src.Name))
		}
		images[src.ID] = response.Images
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	current, err := s.loadPreviewRow(ctx, tx, p.ID, true)
	if err != nil {
		return err
	}
	if current.Environment != "" || current.Stage != "plan" || current.State != "creating" {
		return tx.Commit(ctx)
	}
	branches := map[int64]string{}
	rows, err := tx.Query(ctx, `SELECT repository_id,head FROM environment_preview_branches WHERE preview_id=$1`, p.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var repository int64
		var head string
		if err = rows.Scan(&repository, &head); err != nil {
			rows.Close()
			return err
		}
		branches[repository] = head
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	environment := resourceID()
	name := "Preview " + p.Suffix
	if len(name) > 120 {
		name = name[:120]
	}
	if _, err = tx.Exec(ctx, `INSERT INTO environments(id,project_id,team_id,name) VALUES($1,$2,$3,$4)`, environment, p.Project, p.Team, name); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			name = fmt.Sprintf("Preview %s (%s)", p.Suffix, p.Slug)
			_, err = tx.Exec(ctx, `INSERT INTO environments(id,project_id,team_id,name) VALUES($1,$2,$3,$4)`, environment, p.Project, p.Team, name)
		}
		if err != nil {
			return err
		}
	}
	targets := map[string]string{}
	for _, src := range included {
		targets[src.ID] = resourceID()
	}
	order := previewOrder(included, plans)
	for _, src := range included {
		item := plans[src.ID]
		policy := recipe.Resources[src.ID]
		target := targets[src.ID]
		config := workloadInput{Name: src.Name, NodeID: src.Node, Kind: src.Kind, JobService: src.Config.JobService, Limits: src.Config.Limits, Hooks: src.Config.Hooks, Environment: map[string]string{}}
		if config.Kind == "" {
			config.Kind = "service"
		}
		if policy.FileSecrets == "copy" {
			config.FileSecrets = src.Config.FileSecrets
		}
		for _, variable := range item.Variables {
			switch variable.Mode {
			case "copy":
				config.Environment[variable.Key] = src.Config.Environment[variable.Key]
			case "override":
				config.Environment[variable.Key] = policy.Variables[variable.Key].Value
			}
		}
		baseline := previewBaseline{Images: images[src.ID]}
		deployed := src.Deployed
		if deployed.Source != nil {
			source := *deployed.Source
			source.Token = ""
			source.WatchPaths = nil
			source.Revision = src.DeployedRevision
			if source.Revision == "" {
				source.Revision = deployed.Source.Revision
			}
			if source.InlineCompose {
				baseline.Compose = deployed.Compose
			}
			baseline.Source = &source
			live := source
			live.Revision = ""
			config.Source = &live
		} else {
			baseline.Compose = deployed.Compose
		}
		config.Compose = baseline.Compose
		if src.Kind == "job" && baseline.Source != nil {
			pinned := *baseline.Source
			config.Source = &pinned
		}
		if err = validateWorkload(config); err != nil {
			return s.failPreview(ctx, p.ID, "plan", fmt.Sprintf("%s cannot be copied: %v", src.Name, err))
		}
		encrypted, err := s.sealJSON(config)
		if err != nil {
			return err
		}
		cpu, memory := configuredUsage(config)
		cron := ""
		if src.Kind == "job" && policy.Schedule {
			cron = src.JobCron
		}
		if _, err = tx.Exec(ctx, `INSERT INTO workloads(id,node_id,name,configuration,kind,environment_id,team_id,project_id,configured_cpu,configured_memory_mb,job_cron,next_job_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,CASE WHEN $11='' THEN NULL ELSE now()+interval '1 minute' END)`, target, src.Node, src.Name, encrypted, config.Kind, environment, p.Team, p.Project, cpu, memory, cron); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.ConstraintName == "cove_team_quota" {
				tx.Rollback(ctx)
				return s.failPreview(ctx, p.ID, "plan", "The team quota does not allow the preview's resources. Raise the quota or delete other resources.")
			}
			return err
		}
		if src.Repository != 0 {
			if _, err = tx.Exec(ctx, `INSERT INTO github_sources(workload_id,installation_id,repository_id,repository,automatic) SELECT $1,installation_id,repository_id,repository,false FROM github_sources WHERE workload_id=$2`, target, src.ID); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO workload_connections(workload_id,kind,connection_id) SELECT $1,kind,connection_id FROM workload_connections WHERE workload_id=$2 AND kind='registry'`, target, src.ID); err != nil {
			return err
		}
		isolation := worker.EnvironmentPreview{Preview: p.ID, Source: src.ID, Images: images[src.ID]}
		index := 0
		for _, volume := range item.Volumes {
			if volume.Mode == "" {
				continue
			}
			v := worker.PreviewVolume{Key: volume.Key, Target: fmt.Sprintf("cove-pv-%s-%02d", target, index), Mode: volume.Mode}
			if volume.Mode == "copy" {
				v.Source = volume.Source
			}
			index++
			isolation.Volumes = append(isolation.Volumes, v)
		}
		if item.Database != nil && item.Database.Mode == "copy" {
			isolation.Database = &worker.PreviewDatabase{Engine: item.Database.Engine, Service: item.Database.Service, Name: item.Database.Name}
		}
		for _, service := range item.Services {
			if !service.Run && src.Kind != "job" {
				isolation.Disabled = append(isolation.Disabled, service.Name)
			}
		}
		check := worker.Request{ID: "validation", Workload: target, Action: "preview_data", EnvironmentPreview: &isolation}
		if err = worker.ValidateRequest(check); err != nil {
			return s.failPreview(ctx, p.ID, "plan", fmt.Sprintf("%s cannot be isolated: %v", src.Name, err))
		}
		isolationJSON, err := json.Marshal(isolation)
		if err != nil {
			return err
		}
		baselineSealed, err := s.sealJSON(baseline)
		if err != nil {
			return err
		}
		routes := []worker.Route{}
		for i, route := range src.Config.Routes {
			if recipe.DomainTemplate == "" || src.Kind == "job" {
				break
			}
			name := resourceSlug(src.Name)
			if i > 0 {
				name = fmt.Sprintf("%s-%d", name, i+1)
			}
			domain, err := renderPreviewDomain(recipe.DomainTemplate, name, p.Slug)
			if err != nil {
				return s.failPreview(ctx, p.ID, "plan", err.Error())
			}
			route.Domain, route.Auth = domain, p.Snapshot.Auth
			routes = append(routes, route)
		}
		routesJSON, err := json.Marshal(routes)
		if err != nil {
			return err
		}
		branchRevision := ""
		if src.Repository != 0 && plans[src.ID].FollowsRepo && src.Kind != "job" {
			branchRevision = branches[src.Repository]
		}
		dataState := "pending"
		if len(isolation.Volumes) == 0 && len(isolation.Images) == 0 {
			dataState = "none"
		}
		if _, err = tx.Exec(ctx, `INSERT INTO environment_preview_resources(preview_id,base_id,resource_id,name,kind,position,repository_id,baseline,baseline_revision,baseline_release,baseline_deployed_at,branch_revision,isolation,data_state,routes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, p.ID, src.ID, target, src.Name, config.Kind, order[src.ID], src.Repository, baselineSealed, src.DeployedRevision, src.Release, src.DeployedAt, branchRevision, isolationJSON, dataState, routesJSON); err != nil {
			return err
		}
	}
	if conflict, err := s.previewRouteConflict(ctx, tx, p.ID); err != nil {
		return err
	} else if conflict != "" {
		tx.Rollback(ctx)
		return s.failPreview(ctx, p.ID, "plan", conflict+" is already used by the dashboard or another resource.")
	}
	if _, err = tx.Exec(ctx, `UPDATE environment_previews SET environment_id=$2,state='creating',stage='data',detail='',updated_at=now(),next_attempt_at=now(),last_activity_at=now() WHERE id=$1`, p.ID, environment); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) previewRouteConflict(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	resources, err := s.loadPreviewResources(ctx, tx, id)
	if err != nil {
		return "", err
	}
	for _, resource := range resources {
		if conflict, err := s.routeConflict(ctx, tx, resource.ResourceID, resource.Routes); err != nil || conflict != "" {
			return conflict, err
		}
	}
	return "", nil
}

// previewOrder ranks resources so connection targets and variable sources deploy before the
// resources that use them. Cycles keep their remaining members at the last rank.
func previewOrder(sources []*previewSource, plans map[string]previewResourcePlan) map[string]int {
	needs := map[string]map[string]bool{}
	present := map[string]bool{}
	for _, src := range sources {
		present[src.ID] = true
		needs[src.ID] = map[string]bool{}
	}
	for _, src := range sources {
		for _, dependency := range plans[src.ID].Depends {
			target := dependency.TargetID
			if target == "" {
				target = dependency.SourceID
			}
			if dependency.Inside && present[target] && target != src.ID {
				needs[src.ID][target] = true
			}
		}
	}
	order := map[string]int{}
	rank := 0
	for len(order) < len(sources) {
		wave := []string{}
		for _, src := range sources {
			if _, done := order[src.ID]; done {
				continue
			}
			ready := true
			for target := range needs[src.ID] {
				if _, done := order[target]; !done {
					ready = false
				}
			}
			if ready {
				wave = append(wave, src.ID)
			}
		}
		if len(wave) == 0 {
			for _, src := range sources {
				if _, done := order[src.ID]; !done {
					order[src.ID] = rank
				}
			}
			break
		}
		for _, id := range wave {
			order[id] = rank
		}
		rank++
	}
	return order
}

func (s *Server) operationState(ctx context.Context, id string) (string, []byte, error) {
	var state string
	var result []byte
	err := s.DB.QueryRow(ctx, `SELECT state,COALESCE(result,'null'::jsonb) FROM operations WHERE id=$1`, id).Scan(&state, &result)
	return state, result, err
}

func (s *Server) queuePreviewOperation(ctx context.Context, tx pgx.Tx, request worker.Request) error {
	if err := worker.ValidateRequest(request); err != nil {
		return err
	}
	payload, err := s.sealJSON(request)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,$3,$4)`, request.ID, request.Workload, request.Action, payload)
	return err
}

func activeOperation(ctx context.Context, tx pgx.Tx, workload string) (bool, error) {
	var active bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))`, workload).Scan(&active)
	return active, err
}

// dataPreviewStage copies storage and snapshots databases, once per resource. Completed copies are
// never repeated on retry, so retrying a failed preview cannot reset data it already holds.
func (s *Server) dataPreviewStage(ctx context.Context, p *previewRow) error {
	resources, err := s.loadPreviewResources(ctx, s.DB, p.ID)
	if err != nil {
		return err
	}
	pending := false
	for _, resource := range resources {
		switch resource.DataState {
		case "none", "ready":
			continue
		case "failed":
			return s.failPreview(ctx, p.ID, "data", fmt.Sprintf("Copying data for %s failed. Production data was only read. Check the operation log and retry.", resource.Name))
		}
		pending = true
		if resource.DataOperation != "" {
			state, result, err := s.operationState(ctx, resource.DataOperation)
			if err != nil {
				return err
			}
			switch state {
			case "queued", "running":
				continue
			case "succeeded":
				var operation struct {
					Preview *worker.PreviewReport `json:"environment_preview"`
				}
				_ = json.Unmarshal(result, &operation)
				var snapshot *time.Time
				report := []byte("null")
				if operation.Preview != nil {
					report, _ = json.Marshal(operation.Preview)
					if operation.Preview.Database != nil {
						at := operation.Preview.Database.SnapshotAt
						snapshot = &at
					}
				}
				if _, err = s.DB.Exec(ctx, `UPDATE environment_preview_resources SET data_state='ready',data_report=$3,snapshot_at=$4 WHERE preview_id=$1 AND base_id=$2`, p.ID, resource.BaseID, report, snapshot); err != nil {
					return err
				}
				continue
			default:
				if _, err = s.DB.Exec(ctx, `UPDATE environment_preview_resources SET data_state='failed' WHERE preview_id=$1 AND base_id=$2`, p.ID, resource.BaseID); err != nil {
					return err
				}
				return s.failPreview(ctx, p.ID, "data", fmt.Sprintf("Copying data for %s failed. Production data was only read. Check the operation log and retry.", resource.Name))
			}
		}
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			return err
		}
		active, err := activeOperation(ctx, tx, resource.ResourceID)
		if err != nil || active {
			tx.Rollback(ctx)
			if err != nil {
				return err
			}
			continue
		}
		isolation := resource.Isolation
		isolation.Refresh = p.RefreshScope != ""
		request := worker.Request{ID: resourceID(), Workload: resource.ResourceID, Action: "preview_data", EnvironmentPreview: &isolation}
		if err = s.queuePreviewOperation(ctx, tx, request); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE environment_preview_resources SET data_state='copying',data_operation=$3 WHERE preview_id=$1 AND base_id=$2`, p.ID, resource.BaseID, request.ID); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
	}
	if pending {
		_, err = s.DB.Exec(ctx, `UPDATE environment_previews SET next_attempt_at=now()+interval '5 seconds' WHERE id=$1`, p.ID)
		return err
	}
	next := "connect"
	if p.RefreshScope != "" {
		next = "deploy"
		if _, err = s.DB.Exec(ctx, `UPDATE environment_preview_resources SET deployed_key='' WHERE preview_id=$1`, p.ID); err != nil {
			return err
		}
		if _, err = s.DB.Exec(ctx, `UPDATE environment_previews SET refresh_scope='' WHERE id=$1`, p.ID); err != nil {
			return err
		}
	}
	return s.setPreviewState(ctx, s.DB, p.ID, p.State, next, "", 0)
}

// connectPreviewStage recreates the production environment's private connections, shared networks
// and variable bindings between the preview's own resources. Nothing is connected to production.
func (s *Server) connectPreviewStage(ctx context.Context, p *previewRow) error {
	resources, err := s.loadPreviewResources(ctx, s.DB, p.ID)
	if err != nil {
		return err
	}
	copies := map[string]string{}
	ids := []string{}
	for _, resource := range resources {
		copies[resource.BaseID] = resource.ResourceID
		ids = append(ids, resource.BaseID)
	}
	topology, err := s.loadPreviewTopology(ctx, p.Team, ids)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	current, err := s.loadPreviewRow(ctx, tx, p.ID, true)
	if err != nil || current.Stage != "connect" {
		return err
	}
	for _, c := range topology.connections {
		source, target := copies[c.source], copies[c.target]
		if source == "" || target == "" {
			continue
		}
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM resource_connections WHERE source_id=$1 AND source_service=$2 AND target_id=$3 AND target_service=$4 AND port=$5 AND NOT removing)`, source, c.sourceService, target, c.targetService, c.port).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err = tx.Exec(ctx, `INSERT INTO resource_connections(id,team_id,source_id,source_service,target_id,target_service,port,alias,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, resourceID(), p.Team, source, c.sourceService, target, c.targetService, c.port, c.alias, "preview:"+p.ID); err != nil {
			return err
		}
	}
	networks := map[string]string{}
	rows, err := tx.Query(ctx, `SELECT base_network_id,network_id FROM environment_preview_networks WHERE preview_id=$1`, p.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var base, network string
		if err = rows.Scan(&base, &network); err != nil {
			rows.Close()
			return err
		}
		networks[base] = network
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, n := range topology.networks {
		member := copies[n.workload]
		if member == "" {
			continue
		}
		network := networks[n.network]
		if network == "" {
			network = resourceID()
			var node string
			if err = tx.QueryRow(ctx, `SELECT node_id FROM shared_networks WHERE id=$1`, n.network).Scan(&node); err != nil {
				return err
			}
			name := fmt.Sprintf("%s (preview %s)", n.name, p.Slug)
			if len(name) > 120 {
				name = name[:120]
			}
			if _, err = tx.Exec(ctx, `INSERT INTO shared_networks(id,team_id,node_id,name) VALUES($1,$2,$3,$4)`, network, p.Team, node, name); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO environment_preview_networks(preview_id,base_network_id,network_id) VALUES($1,$2,$3)`, p.ID, n.network, network); err != nil {
				return err
			}
			networks[n.network] = network
		}
		if _, err = tx.Exec(ctx, `INSERT INTO workload_networks(workload_id,network_id,service,alias) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, member, network, n.service, n.alias); err != nil {
			return err
		}
	}
	for _, b := range topology.bindings {
		consumer := copies[b.workload]
		if consumer == "" {
			continue
		}
		var raw []byte
		if err = tx.QueryRow(ctx, `SELECT configuration FROM workloads WHERE id=$1`, consumer).Scan(&raw); err != nil {
			return err
		}
		var config workloadInput
		if err = s.unsealJSON(raw, &config); err != nil {
			return err
		}
		referenced := false
		for _, value := range config.Environment {
			if strings.Contains(value, "{{resource."+b.name+"}}") {
				referenced = true
			}
		}
		if !referenced {
			continue
		}
		source := copies[b.source]
		if source == "" {
			// The recipe explicitly chose to copy the production value for this binding.
			source = b.source
		}
		if _, err = tx.Exec(ctx, `INSERT INTO resource_variable_bindings(workload_id,name,source_id,source_key) VALUES($1,$2,$3,$4) ON CONFLICT(workload_id,name) DO NOTHING`, consumer, b.name, source, b.sourceKey); err != nil {
			return err
		}
	}
	if err = s.setPreviewState(ctx, tx, p.ID, p.State, "deploy", "", 0); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// previewDeployRequest builds the deployment of one preview resource: the matching branch head when
// one exists, otherwise the production baseline pinned at creation.
func (s *Server) previewDeployRequest(ctx context.Context, tx pgx.Tx, p *previewRow, resource previewResourceRow, published bool) (worker.Request, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT configuration FROM workloads WHERE id=$1 FOR UPDATE`, resource.ResourceID).Scan(&raw); err != nil {
		return worker.Request{}, err
	}
	var config workloadInput
	if err := s.unsealJSON(raw, &config); err != nil {
		return worker.Request{}, err
	}
	request := worker.Request{ID: resourceID(), Workload: resource.ResourceID, Action: "deploy", Compose: config.Compose, Environment: config.Environment, Kind: config.Kind, JobService: config.JobService, Limits: config.Limits, Hooks: config.Hooks, FileSecrets: config.FileSecrets}
	isolation := resource.Isolation
	isolation.Images = nil
	isolation.Refresh = false
	request.EnvironmentPreview = &isolation
	if resource.BranchRevision != "" && config.Source != nil {
		source := *config.Source
		source.Revision = resource.BranchRevision
		source.Token = ""
		branch, err := s.previewBranchName(ctx, tx, p.ID, resource.Repository)
		if err != nil {
			return worker.Request{}, err
		}
		source.Branch = branch
		request.Source = &source
	} else {
		if resource.Baseline.Source != nil {
			source := *resource.Baseline.Source
			request.Source = &source
		}
		request.Compose = resource.Baseline.Compose
		if len(resource.Baseline.Images) > 0 {
			request.ImagePins = resource.Baseline.Images
		}
	}
	if published {
		request.Routes = resource.Routes
	}
	if err := loadNetworkRequest(ctx, tx, &request); err != nil {
		return worker.Request{}, err
	}
	return request, nil
}

func (s *Server) previewBranchName(ctx context.Context, tx pgx.Tx, preview string, repository int64) (string, error) {
	var branch string
	err := tx.QueryRow(ctx, `SELECT branch FROM environment_preview_branches WHERE preview_id=$1 AND repository_id=$2`, preview, repository).Scan(&branch)
	return branch, err
}

// deployPreviewStage deploys resources whose desired revision changed, in dependency order. A push
// to one repository redeploys only that repository's resources; databases are never reset here.
func (s *Server) deployPreviewStage(ctx context.Context, p *previewRow) error {
	resources, err := s.loadPreviewResources(ctx, s.DB, p.ID)
	if err != nil {
		return err
	}
	published := false
	for _, resource := range resources {
		if resource.RoutesOperation != "" {
			published = true
		}
	}
	waiting := false
	blockedRank := -1
	for _, resource := range resources {
		if resource.Kind == "job" {
			continue
		}
		if blockedRank >= 0 && resource.Position > blockedRank {
			break
		}
		desired := resource.desiredKey()
		if resource.DeployedKey == desired {
			continue
		}
		if resource.DeployOperation != "" && resource.DeployingKey != "" {
			state, result, err := s.operationState(ctx, resource.DeployOperation)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			switch state {
			case "queued", "running":
				waiting = true
				blockedRank = resource.Position
				continue
			case "succeeded":
				if _, err = s.DB.Exec(ctx, `UPDATE environment_preview_resources SET deployed_key=deploying_key,deploying_key='' WHERE preview_id=$1 AND base_id=$2`, p.ID, resource.BaseID); err != nil {
					return err
				}
				// The deployment that restored the database snapshot records the verified row counts.
				var operation struct {
					Preview *struct {
						Seeded json.RawMessage `json:"seeded"`
					} `json:"environment_preview"`
				}
				if json.Unmarshal(result, &operation) == nil && operation.Preview != nil && len(operation.Preview.Seeded) > 0 {
					if _, err = s.DB.Exec(ctx, `UPDATE environment_preview_resources SET data_report=jsonb_set(CASE WHEN jsonb_typeof(data_report)='object' THEN data_report ELSE '{}'::jsonb END,'{seeded}',$3::jsonb) WHERE preview_id=$1 AND base_id=$2`, p.ID, resource.BaseID, string(operation.Preview.Seeded)); err != nil {
						return err
					}
				}
				if resource.DeployingKey == desired {
					continue
				}
			default:
				if resource.DeployingKey == desired {
					return s.failPreview(ctx, p.ID, "deploy", fmt.Sprintf("Deploying %s failed. Its data was kept. Check the operation log, push a fix or retry.", resource.Name))
				}
			}
		}
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			return err
		}
		active, err := activeOperation(ctx, tx, resource.ResourceID)
		if err != nil || active {
			tx.Rollback(ctx)
			if err != nil {
				return err
			}
			waiting = true
			blockedRank = resource.Position
			continue
		}
		request, err := s.previewDeployRequest(ctx, tx, p, resource, published)
		if err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err = s.queuePreviewOperation(ctx, tx, request); err != nil {
			tx.Rollback(ctx)
			return s.failPreview(ctx, p.ID, "deploy", fmt.Sprintf("Could not queue the deployment of %s: %v", resource.Name, err))
		}
		if _, err = tx.Exec(ctx, `UPDATE workloads SET manually_stopped=false WHERE id=$1`, resource.ResourceID); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE environment_preview_resources SET deploy_operation=$3,deploying_key=$4 WHERE preview_id=$1 AND base_id=$2`, p.ID, resource.BaseID, request.ID, desired); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		waiting = true
		blockedRank = resource.Position
	}
	if waiting {
		_, err = s.DB.Exec(ctx, `UPDATE environment_previews SET next_attempt_at=now()+interval '5 seconds' WHERE id=$1`, p.ID)
		return err
	}
	return s.setPreviewState(ctx, s.DB, p.ID, p.State, "publish", "", 0)
}

// publishPreviewStage exposes protected routes only after every resource deployed successfully.
func (s *Server) publishPreviewStage(ctx context.Context, p *previewRow) error {
	resources, err := s.loadPreviewResources(ctx, s.DB, p.ID)
	if err != nil {
		return err
	}
	waiting := false
	for _, resource := range resources {
		if len(resource.Routes) == 0 {
			continue
		}
		if resource.RoutesOperation != "" {
			state, _, err := s.operationState(ctx, resource.RoutesOperation)
			if err != nil {
				return err
			}
			switch state {
			case "succeeded":
				continue
			case "queued", "running":
				waiting = true
				continue
			default:
				return s.failPreview(ctx, p.ID, "publish", fmt.Sprintf("Publishing the protected routes of %s failed; nothing was exposed. Retry once the node's proxy is healthy.", resource.Name))
			}
		}
		for _, route := range resource.Routes {
			if err = s.ensurePreviewDNS(ctx, p.Team, resource.Node, route.Domain); err != nil {
				return s.failPreview(ctx, p.ID, "publish", fmt.Sprintf("Could not create the DNS record for %s: %v", route.Domain, err))
			}
		}
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			return err
		}
		active, err := activeOperation(ctx, tx, resource.ResourceID)
		if err != nil || active {
			tx.Rollback(ctx)
			if err != nil {
				return err
			}
			waiting = true
			continue
		}
		if conflict, err := s.routeConflict(ctx, tx, resource.ResourceID, resource.Routes); err != nil || conflict != "" {
			tx.Rollback(ctx)
			if err != nil {
				return err
			}
			return s.failPreview(ctx, p.ID, "publish", conflict+" is already used by the dashboard or another resource.")
		}
		isolation := resource.Isolation
		isolation.Images = nil
		request := worker.Request{ID: resourceID(), Workload: resource.ResourceID, Action: "routes", Routes: resource.Routes, EnvironmentPreview: &isolation}
		if err = s.queuePreviewOperation(ctx, tx, request); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE environment_preview_resources SET routes_operation=$3 WHERE preview_id=$1 AND base_id=$2`, p.ID, resource.BaseID, request.ID); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		waiting = true
	}
	if waiting {
		_, err = s.DB.Exec(ctx, `UPDATE environment_previews SET next_attempt_at=now()+interval '5 seconds' WHERE id=$1`, p.ID)
		return err
	}
	current, err := s.loadPreviewResources(ctx, s.DB, p.ID)
	if err != nil {
		return err
	}
	if previewDrifted(current) {
		return s.markPreviewDrifted(ctx, p.ID)
	}
	detail := ""
	if p.State == "creating" {
		_, err = s.DB.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES('Cove','environment preview ready',$1,200,$2)`, p.ID, p.Team)
		if err != nil {
			return err
		}
	}
	return s.setPreviewState(ctx, s.DB, p.ID, "ready", "ready", detail, 5*time.Minute)
}

// previewDrifted reports resources whose branch or baseline changed after their last deployment.
func previewDrifted(resources []previewResourceRow) bool {
	for _, resource := range resources {
		if resource.Kind != "job" && resource.DeployedKey != resource.desiredKey() {
			return true
		}
	}
	return false
}

func (s *Server) markPreviewDrifted(ctx context.Context, id string) error {
	_, err := s.DB.Exec(ctx, `UPDATE environment_previews SET state=CASE WHEN state='creating' THEN 'creating' ELSE 'updating' END,stage='deploy',detail='',updated_at=now(),next_attempt_at=now() WHERE id=$1 AND state IN ('creating','updating','ready')`, id)
	return err
}

func (s *Server) ensurePreviewDNS(ctx context.Context, team, node, domain string) error {
	zone, address, err := s.previewDNSZone(ctx, team, node, domain)
	if err != nil || zone == nil {
		return err
	}
	return s.reconcileDNSRecord(ctx, *zone, domain, address)
}

func (s *Server) previewDNSZone(ctx context.Context, team, node, domain string) (*cloudflareZone, string, error) {
	rows, err := s.DB.Query(ctx, `SELECT z.configuration,t.domains FROM cloudflare_zones z JOIN cloudflare_zone_teams t ON t.zone_id=z.zone_id WHERE t.team_id=$1 AND ($2=z.domain OR $2 LIKE '%.'||z.domain)`, team, domain)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var domains []string
		if err = rows.Scan(&raw, &domains); err != nil {
			return nil, "", err
		}
		var zone cloudflareZone
		if err = s.unsealJSON(raw, &zone); err != nil {
			return nil, "", errors.New("DNS configuration unavailable")
		}
		if zone.Enabled && zone.Nodes[node] != "" && cloudflareDomainAllowed(domains, domain) {
			return &zone, zone.Nodes[node], nil
		}
	}
	return nil, "", rows.Err()
}

// removePreviewStage removes every preview resource from its node, then the records. Production
// resources are never touched: the worker refuses resources without the preview ownership marker.
func (s *Server) removePreviewStage(ctx context.Context, p *previewRow) error {
	resources, err := s.loadPreviewResources(ctx, s.DB, p.ID)
	if err != nil {
		return err
	}
	ids := []string{}
	for _, resource := range resources {
		ids = append(ids, resource.ResourceID)
	}
	if len(ids) > 0 {
		if _, err = s.DB.Exec(ctx, `UPDATE operations SET cancel_requested=true WHERE workload_id=ANY($1) AND action NOT IN ('environment_preview_remove','routes') AND state IN ('queued','running')`, ids); err != nil {
			return err
		}
		if _, err = s.DB.Exec(ctx, `DELETE FROM resource_connections WHERE source_id=ANY($1) OR target_id=ANY($1)`, ids); err != nil {
			return err
		}
		if _, err = s.DB.Exec(ctx, `UPDATE workloads SET job_cron='',next_job_at=NULL WHERE id=ANY($1)`, ids); err != nil {
			return err
		}
	}
	networks := []string{}
	rows, err := s.DB.Query(ctx, `SELECT network_id FROM environment_preview_networks WHERE preview_id=$1 ORDER BY network_id`, p.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		networks = append(networks, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	// Remove one resource at a time so the last removal can delete the preview's shared networks.
	for _, resource := range resources {
		if resource.RemoveOperation != "" {
			state, _, err := s.operationState(ctx, resource.RemoveOperation)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if state == "succeeded" {
				continue
			}
			if state == "queued" || state == "running" {
				_, err = s.DB.Exec(ctx, `UPDATE environment_previews SET next_attempt_at=now()+interval '5 seconds' WHERE id=$1`, p.ID)
				return err
			}
			if state != "" {
				if _, err = s.DB.Exec(ctx, `UPDATE environment_previews SET detail=$2 WHERE id=$1`, p.ID, fmt.Sprintf("Removing %s failed; retrying. Check the operation log if this repeats.", resource.Name)); err != nil {
					return err
				}
			}
		}
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			return err
		}
		active, err := activeOperation(ctx, tx, resource.ResourceID)
		if err != nil || active {
			tx.Rollback(ctx)
			if err == nil {
				_, err = s.DB.Exec(ctx, `UPDATE environment_previews SET next_attempt_at=now()+interval '5 seconds' WHERE id=$1`, p.ID)
			}
			return err
		}
		isolation := resource.Isolation
		isolation.Images = nil
		isolation.Networks = networks
		request := worker.Request{ID: resourceID(), Workload: resource.ResourceID, Action: "environment_preview_remove", EnvironmentPreview: &isolation}
		if err = s.queuePreviewOperation(ctx, tx, request); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE environment_preview_resources SET remove_operation=$3 WHERE preview_id=$1 AND base_id=$2`, p.ID, resource.BaseID, request.ID); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		_, err = s.DB.Exec(ctx, `UPDATE environment_previews SET next_attempt_at=now()+interval '5 seconds' WHERE id=$1`, p.ID)
		return err
	}
	for _, resource := range resources {
		for _, route := range resource.Routes {
			if resource.RoutesOperation == "" {
				continue
			}
			zone, address, err := s.previewDNSZone(ctx, p.Team, resource.Node, route.Domain)
			if err != nil {
				return err
			}
			if zone != nil {
				if err = s.removeDNSRecord(ctx, *zone, route.Domain, address); err != nil {
					_, err = s.DB.Exec(ctx, `UPDATE environment_previews SET detail=$2,next_attempt_at=now()+interval '1 minute' WHERE id=$1`, p.ID, "Could not remove the DNS record for "+route.Domain+"; retrying.")
					return err
				}
			}
		}
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = s.loadPreviewRow(ctx, tx, p.ID, true); err != nil {
		return err
	}
	queries := []string{
		`DELETE FROM resource_variable_bindings WHERE workload_id=ANY($1) OR source_id=ANY($1)`,
		`DELETE FROM workload_networks WHERE workload_id=ANY($1)`,
		`DELETE FROM api_tokens WHERE workload_id=ANY($1)`,
		`DELETE FROM webhook_deliveries WHERE workload_id=ANY($1)`,
		`DELETE FROM operation_dependencies WHERE operation_id IN (SELECT id FROM operations WHERE workload_id=ANY($1)) OR prerequisite_id IN (SELECT id FROM operations WHERE workload_id=ANY($1))`,
		`DELETE FROM operations WHERE workload_id=ANY($1)`,
		`DELETE FROM environment_preview_resources WHERE resource_id=ANY($1)`,
		`DELETE FROM workloads WHERE id=ANY($1)`,
	}
	for _, query := range queries {
		if _, err = tx.Exec(ctx, query, ids); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM environment_preview_networks WHERE preview_id=$1`, p.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO preview_branch_dismissals(project_id,repository_id,branch,head) SELECT $2,repository_id,branch,head FROM environment_preview_branches WHERE preview_id=$1 AND head<>'' ON CONFLICT(project_id,repository_id,branch) DO UPDATE SET head=EXCLUDED.head,dismissed_at=now()`, p.ID, p.Project); err != nil {
		return err
	}
	if len(networks) > 0 {
		if _, err = tx.Exec(ctx, `DELETE FROM shared_networks WHERE id=ANY($1) AND NOT EXISTS(SELECT 1 FROM workload_networks m WHERE m.network_id=shared_networks.id)`, networks); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM environment_previews WHERE id=$1`, p.ID); err != nil {
		return err
	}
	if p.Environment != "" {
		if _, err = tx.Exec(ctx, `DELETE FROM shared_variables WHERE environment_id=$1`, p.Environment); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM deployment_policies WHERE environment_id=$1`, p.Environment); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM environments e WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM workloads WHERE environment_id=e.id)`, p.Environment); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES('Cove','environment preview deleted: '||$2,$1,200,$3)`, p.ID, p.DeleteReason, p.Team); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
