package control

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strings"
	"time"
)

type deploymentPolicy struct {
	Timezone      string         `json:"timezone"`
	Windows       []changeWindow `json:"recurring_windows"`
	Freezes       []changeFreeze `json:"freezes"`
	OutsideWindow string         `json:"outside_window"`
	WindowStart   string         `json:"window_start"`
	WindowEnd     string         `json:"window_end"`
	Approval      bool           `json:"approval"`
	FreezeFrom    *time.Time     `json:"freeze_from"`
	FreezeUntil   *time.Time     `json:"freeze_until"`
}

func (s *Server) deploymentPolicy(w http.ResponseWriter, r *http.Request) {
	var project, environment string
	if strings.Contains(r.URL.Path, "/projects/") {
		project = r.PathValue("id")
	} else if strings.Contains(r.URL.Path, "/environments/") {
		environment = r.PathValue("id")
	}
	team := actor(r).Team
	if strings.Contains(r.URL.Path, "/teams/") {
		team = r.PathValue("id")
	}
	if project != "" || environment != "" {
		var belongs bool
		if err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1 AND team_id=$3) OR EXISTS(SELECT 1 FROM environments e JOIN projects p ON p.id=e.project_id WHERE e.id=$2 AND p.team_id=$3)`, project, environment, team).Scan(&belongs); err != nil {
			http.Error(w, "policy scope unavailable", 503)
			return
		}
		if !belongs {
			http.Error(w, "policy scope does not belong to the selected team", 403)
			return
		}
	}
	if r.Method == "GET" {
		var policy deploymentPolicy
		err := s.DB.QueryRow(r.Context(), `SELECT approval,freeze_from,freeze_until,window_start,window_end,timezone,recurring_windows,freezes,outside_window FROM deployment_policies WHERE team_id=$1 AND project_id IS NOT DISTINCT FROM NULLIF($2,'') AND environment_id IS NOT DISTINCT FROM NULLIF($3,'')`, team, project, environment).Scan(&policy.Approval, &policy.FreezeFrom, &policy.FreezeUntil, &policy.WindowStart, &policy.WindowEnd, &policy.Timezone, &policy.Windows, &policy.Freezes, &policy.OutsideWindow)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "deployment policy unavailable", 503)
			return
		}
		normalizeSchedule(&policy)
		respond(w, 200, policy)
		return
	}
	if !s.teamAdmin(r, team) {
		http.Error(w, "team administrator required", 403)
		return
	}
	var policy deploymentPolicy
	if !decode(w, r, &policy) {
		return
	}
	if err := validateSchedule(&policy); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "policy unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended('cove-quota:'||$1,0))`, team); err != nil {
		http.Error(w, "policy unavailable", 503)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO deployment_policies(team_id,project_id,environment_id,approval,freeze_from,freeze_until,window_start,window_end,timezone,recurring_windows,freezes,outside_window) VALUES($1,NULLIF($2,''),NULLIF($3,''),$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(team_id,project_id,environment_id) DO UPDATE SET approval=$4,freeze_from=$5,freeze_until=$6,window_start=$7,window_end=$8,timezone=$9,recurring_windows=$10,freezes=$11,outside_window=$12`, team, project, environment, policy.Approval, policy.FreezeFrom, policy.FreezeUntil, policy.WindowStart, policy.WindowEnd, policy.Timezone, policy.Windows, policy.Freezes, policy.OutsideWindow); err != nil {
		http.Error(w, "could not save policy", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save policy", 503)
		return
	}
	respond(w, 200, policy)
}

func (s *Server) approveDeployment(w http.ResponseWriter, r *http.Request) {
	if !s.teamAdmin(r, actor(r).Team) {
		http.Error(w, "team administrator required", 403)
		return
	}
	by := actor(r).ID
	if by == "" {
		by = "instance-administrator"
	}
	result, err := s.DB.Exec(r.Context(), `UPDATE operations SET approved_by=$2,approved_at=now(),updated_at=now() WHERE id=$1 AND state='queued' AND approval_required AND approved_at IS NULL AND admitted_at IS NULL AND NOT cancel_requested`, r.PathValue("id"), by)
	if err != nil {
		http.Error(w, "could not approve deployment", 503)
		return
	}
	if result.RowsAffected() != 1 {
		http.Error(w, "deployment no longer awaits approval", 409)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func deploymentGate(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	now := time.Now()
	reason, err := scheduledDeploymentGate(ctx, tx, id, now)
	if err != nil || reason != "" {
		return reason, err
	}
	var start, end string
	var override bool
	err = tx.QueryRow(ctx, `SELECT d.window_start,d.window_end,o.emergency_override_at IS NOT NULL FROM image_update_deployments d JOIN operations o ON o.id=d.operation_id WHERE d.operation_id=$1`, id).Scan(&start, &end, &override)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !override && !withinDeploymentWindow(start, end, now.UTC().Format("15:04")) {
		return "Waiting for the image update UTC maintenance window.", nil
	}
	return "", nil
}

func validDeploymentWindow(start, end string) bool {
	if start == "" && end == "" {
		return true
	}
	if start == end {
		return false
	}
	for _, value := range []string{start, end} {
		parsed, err := time.Parse("15:04", value)
		if err != nil || parsed.Format("15:04") != value {
			return false
		}
	}
	return true
}

func withinDeploymentWindow(start, end, now string) bool {
	if !validDeploymentWindow(start, end) {
		return false
	}
	if start == "" {
		return true
	}
	if start < end {
		return now >= start && now < end
	}
	return now >= start || now < end
}

func holdDeployment(ctx context.Context, tx pgx.Tx, id, message string) (bool, error) {
	result, err := json.Marshal(map[string]string{"error": message, "hold_reason": message})
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE operations SET result=$2,updated_at=now() WHERE id=$1`, id, result); err != nil {
		return false, err
	}
	return false, tx.Commit(ctx)
}
