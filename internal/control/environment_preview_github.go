package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type previewBranchState struct {
	Repository string
	Branch     string
	Head       string
}

var gitCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

func previewPrefix(pattern string) string { return strings.TrimSuffix(pattern, "*") }

func validPreviewBranch(branch string) bool {
	return branch != "" && len(branch) <= 200 && !strings.HasPrefix(branch, "-") && !strings.ContainsAny(branch, " \r\n\t~^:?*[\\") && !strings.Contains(branch, "..") && !strings.HasSuffix(branch, "/") && !strings.HasSuffix(branch, ".lock")
}

// previewBranchWebhook records that a branch may have changed. The event is only a hint: the
// branch's actual head is read from GitHub before anything changes, so replayed, stale or
// out-of-order deliveries cannot move a preview to an older commit or resurrect a deleted branch.
func (s *Server) previewBranchWebhook(ctx context.Context, body []byte, delivery string) error {
	var event struct {
		Ref          string `json:"ref"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Repository struct {
			ID   int64 `json:"id"`
			Fork bool  `json:"fork"`
		} `json:"repository"`
	}
	if json.Unmarshal(body, &event) != nil || !strings.HasPrefix(event.Ref, "refs/heads/") || event.Repository.ID == 0 || event.Installation.ID == 0 {
		return nil
	}
	branch := strings.TrimPrefix(event.Ref, "refs/heads/")
	if !validPreviewBranch(branch) {
		return nil
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	receipt, err := tx.Exec(ctx, `INSERT INTO preview_branch_deliveries(delivery) VALUES($1) ON CONFLICT DO NOTHING`, delivery)
	if err != nil {
		return err
	}
	if receipt.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	// Only recipes whose team was given this installation can react to its repositories.
	rows, err := tx.Query(ctx, `SELECT r.project_id,r.configuration FROM preview_recipes r JOIN github_installation_teams i ON i.team_id=r.team_id AND i.installation_id=$1 WHERE r.enabled AND EXISTS(SELECT 1 FROM github_sources g JOIN workloads w ON w.id=g.workload_id WHERE g.repository_id=$2 AND g.installation_id=$1 AND w.environment_id=r.base_environment_id)`, event.Installation.ID, event.Repository.ID)
	if err != nil {
		return err
	}
	projects := []string{}
	for rows.Next() {
		var project string
		var encrypted []byte
		if err = rows.Scan(&project, &encrypted); err != nil {
			rows.Close()
			return err
		}
		var recipe previewRecipe
		if err = s.unsealJSON(encrypted, &recipe); err != nil {
			rows.Close()
			return err
		}
		selected := false
		for _, id := range recipe.Repositories {
			selected = selected || id == event.Repository.ID
		}
		if selected && strings.HasPrefix(branch, previewPrefix(recipe.BranchPattern)) && len(branch) > len(previewPrefix(recipe.BranchPattern)) {
			projects = append(projects, project)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, project := range projects {
		if _, err = tx.Exec(ctx, `INSERT INTO preview_branch_signals(project_id,repository_id,branch) VALUES($1,$2,$3) ON CONFLICT(project_id,repository_id,branch) DO UPDATE SET next_attempt_at=now(),attempts=0,received_at=now()`, project, event.Repository.ID, branch); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// previewRepositoryAccess finds the installation through which this project's production
// environment uses a repository, provided the installation is shared with the project's team.
func (s *Server) previewRepositoryAccess(ctx context.Context, project string, repository int64) (int64, string, error) {
	var installation int64
	var clone string
	err := s.DB.QueryRow(ctx, `SELECT g.installation_id,g.repository FROM preview_recipes r JOIN workloads w ON w.environment_id=r.base_environment_id AND w.team_id=r.team_id JOIN github_sources g ON g.workload_id=w.id JOIN github_installation_teams i ON i.installation_id=g.installation_id AND i.team_id=r.team_id WHERE r.project_id=$1 AND g.repository_id=$2 ORDER BY w.id LIMIT 1`, project, repository).Scan(&installation, &clone)
	return installation, clone, err
}

// scanPreviewRecipes periodically lists matching branches, so previews converge even when webhooks
// are missed, and re-checks the branches of existing previews so deletions are noticed.
func (s *Server) scanPreviewRecipes(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT project_id,configuration FROM preview_recipes WHERE enabled AND next_scan_at<=now() ORDER BY next_scan_at LIMIT 4`)
	if err != nil {
		return err
	}
	type entry struct {
		project string
		recipe  previewRecipe
	}
	entries := []entry{}
	for rows.Next() {
		var e entry
		var encrypted []byte
		if err = rows.Scan(&e.project, &encrypted); err != nil {
			rows.Close()
			return err
		}
		if err = s.unsealJSON(encrypted, &e.recipe); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, e)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, e := range entries {
		if _, err = s.DB.Exec(ctx, `UPDATE preview_recipes SET next_scan_at=now()+interval '5 minutes' WHERE project_id=$1`, e.project); err != nil {
			return err
		}
		prefix := previewPrefix(e.recipe.BranchPattern)
		for _, repository := range e.recipe.Repositories {
			installation, _, err := s.previewRepositoryAccess(ctx, e.project, repository)
			if err != nil {
				slog.Warn("environment preview repository unavailable", "project", e.project, "repository", repository)
				continue
			}
			refs, err := s.githubProvider().Branches(ctx, installation, repository, prefix)
			if err != nil {
				slog.Warn("environment preview branch listing failed", "project", e.project, "repository", repository, "error", err)
				continue
			}
			for _, ref := range refs {
				branch := strings.TrimPrefix(ref.Ref, "refs/heads/")
				if !strings.HasPrefix(branch, prefix) || len(branch) == len(prefix) || !validPreviewBranch(branch) {
					continue
				}
				if _, err = s.DB.Exec(ctx, `INSERT INTO preview_branch_signals(project_id,repository_id,branch) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, e.project, repository, branch); err != nil {
					return err
				}
			}
		}
		if _, err = s.DB.Exec(ctx, `INSERT INTO preview_branch_signals(project_id,repository_id,branch) SELECT p.project_id,b.repository_id,b.branch FROM environment_preview_branches b JOIN environment_previews p ON p.id=b.preview_id WHERE p.project_id=$1 AND p.state<>'deleting' ON CONFLICT DO NOTHING`, e.project); err != nil {
			return err
		}
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM preview_branch_deliveries WHERE received_at<now()-interval '30 days'`)
	return err
}

func (s *Server) processPreviewSignals(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT project_id,repository_id,branch,approved_by,attempts FROM preview_branch_signals WHERE next_attempt_at<=now() ORDER BY next_attempt_at LIMIT 20`)
	if err != nil {
		return err
	}
	type signal struct {
		project, branch, approved string
		repository                int64
		attempts                  int
	}
	signals := []signal{}
	for rows.Next() {
		var item signal
		if err = rows.Scan(&item.project, &item.repository, &item.branch, &item.approved, &item.attempts); err != nil {
			rows.Close()
			return err
		}
		signals = append(signals, item)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, item := range signals {
		reason, retry, err := s.applyPreviewSignal(ctx, item.project, item.repository, item.branch, item.approved)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			slog.Warn("environment preview branch check failed", "project", item.project, "repository", item.repository, "error", err)
			reason, retry = "Waiting for GitHub: "+err.Error(), true
		}
		if retry {
			delay := time.Duration(1<<min(item.attempts, 5)) * time.Minute
			if _, err = s.DB.Exec(ctx, `UPDATE preview_branch_signals SET reason=$4,attempts=attempts+1,next_attempt_at=now()+$5*interval '1 second' WHERE project_id=$1 AND repository_id=$2 AND branch=$3`, item.project, item.repository, item.branch, reason, int(delay.Seconds())); err != nil {
				return err
			}
			continue
		}
		if _, err = s.DB.Exec(ctx, `DELETE FROM preview_branch_signals WHERE project_id=$1 AND repository_id=$2 AND branch=$3`, item.project, item.repository, item.branch); err != nil {
			return err
		}
	}
	return nil
}

// applyPreviewSignal reconciles one branch with GitHub. It returns a reason and whether the signal
// should be retried later, for example while the preview limit is reached.
func (s *Server) applyPreviewSignal(ctx context.Context, project string, repository int64, branch, approved string) (string, bool, error) {
	recipe, version, saved, err := s.loadPreviewRecipe(ctx, s.DB, project)
	if err != nil {
		return "", true, err
	}
	prefix := previewPrefix(recipe.BranchPattern)
	selected := false
	for _, id := range recipe.Repositories {
		selected = selected || id == repository
	}
	var previewID string
	err = s.DB.QueryRow(ctx, `SELECT p.id FROM environment_previews p JOIN environment_preview_branches b ON b.preview_id=p.id WHERE p.project_id=$1 AND b.repository_id=$2 AND b.branch=$3 AND p.state<>'deleting'`, project, repository, branch).Scan(&previewID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", true, err
	}
	if previewID == "" && (!saved || !recipe.Enabled || !selected || !strings.HasPrefix(branch, prefix) || len(branch) == len(prefix)) {
		return "", false, nil
	}
	installation, clone, err := s.previewRepositoryAccess(ctx, project, repository)
	if errors.Is(err, pgx.ErrNoRows) {
		return "The repository is no longer connected through an installation shared with this team.", false, nil
	}
	if err != nil {
		return "", true, err
	}
	head, err := s.githubProvider().BranchHead(ctx, installation, repository, branch)
	if err != nil {
		return "", true, err
	}
	if previewID != "" {
		return "", false, s.updatePreviewBranch(ctx, previewID, repository, head)
	}
	if head == "" {
		_, err = s.DB.Exec(ctx, `DELETE FROM preview_branch_dismissals WHERE project_id=$1 AND repository_id=$2 AND branch=$3`, project, repository, branch)
		return "", err != nil, err
	}
	suffix := strings.TrimPrefix(branch, prefix)
	var ciKey bool
	if err = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM preview_ci_slots WHERE project_id=$1 AND suffix=$2)`, project, suffix).Scan(&ciKey); err != nil {
		return "", true, err
	}
	if ciKey {
		return "This preview key is reserved for CI image submissions.", false, nil
	}
	// A branch that names an existing preview through another repository joins it.
	err = s.DB.QueryRow(ctx, `SELECT id FROM environment_previews WHERE project_id=$1 AND suffix=$2 AND state<>'deleting'`, project, suffix).Scan(&previewID)
	if err == nil {
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			return "", true, err
		}
		defer tx.Rollback(ctx)
		p, problem := s.loadPreviewRow(ctx, tx, previewID, true)
		if problem != nil {
			return "", true, problem
		}
		if p.Snapshot.CI != nil {
			return "This preview key is reserved for CI image submissions.", false, nil
		}
		if _, err = tx.Exec(ctx, `INSERT INTO environment_preview_branches(preview_id,repository_id,repository,branch,head,checked_at) VALUES($1,$2,$3,$4,'',now()) ON CONFLICT DO NOTHING`, previewID, repository, repositoryName(clone), branch); err != nil {
			return "", true, err
		}
		if err = tx.Commit(ctx); err != nil {
			return "", true, err
		}
		return "", false, s.updatePreviewBranch(ctx, previewID, repository, head)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", true, err
	}
	// A preview removed by its editors or by inactivity stays removed until the branch moves on or
	// someone asks for it again; the periodic scan alone never brings it back.
	var dismissed string
	err = s.DB.QueryRow(ctx, `SELECT head FROM preview_branch_dismissals WHERE project_id=$1 AND repository_id=$2 AND branch=$3`, project, repository, branch).Scan(&dismissed)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", true, err
	}
	if dismissed == head && approved == "" {
		return "", false, nil
	}
	branches := map[int64]previewBranchState{repository: {Repository: repositoryName(clone), Branch: branch, Head: head}}
	for _, other := range recipe.Repositories {
		if other == repository {
			continue
		}
		otherInstallation, otherClone, err := s.previewRepositoryAccess(ctx, project, other)
		if err != nil {
			continue
		}
		otherHead, err := s.githubProvider().BranchHead(ctx, otherInstallation, other, branch)
		if err != nil {
			return "", true, err
		}
		if otherHead != "" {
			branches[other] = previewBranchState{Repository: repositoryName(otherClone), Branch: branch, Head: otherHead}
		}
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", true, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cove-preview-recipe:'||$1,0))`, project); err != nil {
		return "", true, err
	}
	var active int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM environment_previews WHERE project_id=$1 AND state<>'deleting'`, project).Scan(&active); err != nil {
		return "", true, err
	}
	if active >= recipe.MaxPreviews {
		return fmt.Sprintf("Waiting: %d of %d previews are in use.", active, recipe.MaxPreviews), true, nil
	}
	id, created, err := s.createPreviewRecord(ctx, tx, project, recipe, version, suffix, branches, approved)
	if err != nil {
		return "", true, err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", true, err
	}
	if !created {
		var state string
		if err = s.DB.QueryRow(ctx, `SELECT state FROM environment_previews WHERE id=$1`, id).Scan(&state); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", true, err
		}
		if state == "deleting" || errors.Is(err, pgx.ErrNoRows) {
			// The branch name is reused while its previous preview is still being removed.
			return "Waiting for the previous preview with this name to be removed.", true, nil
		}
		return "", false, s.updatePreviewBranch(ctx, id, repository, head)
	}
	return "", false, nil
}

// updatePreviewBranch applies a branch head to the preview's resources from that repository. A new
// head redeploys only those resources; a deleted branch returns them to the pinned production
// baseline. The preview is removed only when every participating branch is gone.
func (s *Server) updatePreviewBranch(ctx context.Context, id string, repository int64, head string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	p, err := s.loadPreviewRow(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if p.State == "deleting" || p.Snapshot.CI != nil {
		return tx.Commit(ctx)
	}
	var previous string
	if err = tx.QueryRow(ctx, `SELECT head FROM environment_preview_branches WHERE preview_id=$1 AND repository_id=$2`, id, repository).Scan(&previous); err != nil {
		return err
	}
	if previous == head {
		if _, err = tx.Exec(ctx, `UPDATE environment_preview_branches SET checked_at=now() WHERE preview_id=$1 AND repository_id=$2`, id, repository); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `UPDATE environment_preview_branches SET head=$3,checked_at=now() WHERE preview_id=$1 AND repository_id=$2`, id, repository, head); err != nil {
		return err
	}
	follows := false
	for _, selected := range p.Snapshot.Recipe.Repositories {
		follows = follows || selected == repository
	}
	if follows {
		if _, err = tx.Exec(ctx, `UPDATE environment_preview_resources SET branch_revision=$3 WHERE preview_id=$1 AND repository_id=$2 AND kind<>'job'`, id, repository, head); err != nil {
			return err
		}
	}
	action := "branch updated"
	if head == "" {
		action = "branch deleted; resources return to the pinned production baseline"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES('GitHub webhook',$2,$1,202,$3)`, id, "environment preview "+action, p.Team); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE environment_previews SET last_activity_at=now() WHERE id=$1`, id); err != nil {
		return err
	}
	var present bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environment_preview_branches WHERE preview_id=$1 AND head<>'')`, id).Scan(&present); err != nil {
		return err
	}
	if !present && !p.Pinned {
		if err = s.requestPreviewDeletion(ctx, tx, id, "all preview branches were deleted"); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	// A new head after a failed deployment is treated as the fix and retried automatically.
	if p.State == "ready" || (p.State == "failed" && p.Stage == "deploy") {
		if err = s.setPreviewState(ctx, tx, id, "updating", "deploy", "", 0); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
