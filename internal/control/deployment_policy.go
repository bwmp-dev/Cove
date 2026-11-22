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
	WindowStart string     `json:"window_start"`
	WindowEnd   string     `json:"window_end"`
	Approval    bool       `json:"approval"`
	FreezeFrom  *time.Time `json:"freeze_from"`
	FreezeUntil *time.Time `json:"freeze_until"`
}

func (s *Server) deploymentPolicy(w http.ResponseWriter, r *http.Request) {
	var project, environment string
	if strings.Contains(r.URL.Path, "/projects/") {
		project = r.PathValue("id")
	} else {
		environment = r.PathValue("id")
	}
	team := actor(r).Team
	if r.Method == "GET" {
		var policy deploymentPolicy
		err := s.DB.QueryRow(r.Context(), `SELECT approval,freeze_from,freeze_until,window_start,window_end FROM deployment_policies WHERE team_id=$1 AND project_id IS NOT DISTINCT FROM NULLIF($2,'') AND environment_id IS NOT DISTINCT FROM NULLIF($3,'')`, team, project, environment).Scan(&policy.Approval, &policy.FreezeFrom, &policy.FreezeUntil, &policy.WindowStart, &policy.WindowEnd)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "deployment policy unavailable", 503)
			return
		}
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
	if !validDeploymentWindow(policy.WindowStart, policy.WindowEnd) {
		http.Error(w, "set two different UTC times in HH:MM format, or clear both deployment window times", 400)
		return
	}
	if (policy.FreezeFrom == nil) != (policy.FreezeUntil == nil) || policy.FreezeFrom != nil && (!policy.FreezeUntil.After(*policy.FreezeFrom) || policy.FreezeUntil.Sub(*policy.FreezeFrom) > 366*24*time.Hour) {
		http.Error(w, "set both freeze dates with an end after the start, at most one year apart", 400)
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
	if _, err = tx.Exec(r.Context(), `INSERT INTO deployment_policies(team_id,project_id,environment_id,approval,freeze_from,freeze_until,window_start,window_end) VALUES($1,NULLIF($2,''),NULLIF($3,''),$4,$5,$6,$7,$8) ON CONFLICT(team_id,project_id,environment_id) DO UPDATE SET approval=$4,freeze_from=$5,freeze_until=$6,window_start=$7,window_end=$8`, team, project, environment, policy.Approval, policy.FreezeFrom, policy.FreezeUntil, policy.WindowStart, policy.WindowEnd); err != nil {
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
	var approval, approved bool
	var until *time.Time
	err := tx.QueryRow(ctx, `SELECT o.approval_required OR EXISTS(SELECT 1 FROM deployment_policies p WHERE p.team_id=w.team_id AND (p.project_id=w.project_id OR p.environment_id=w.environment_id) AND p.approval),o.approved_at IS NOT NULL,(SELECT max(p.freeze_until) FROM deployment_policies p WHERE p.team_id=w.team_id AND (p.project_id=w.project_id OR p.environment_id=w.environment_id) AND p.freeze_from<=now() AND p.freeze_until>now()) FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE o.id=$1`, id).Scan(&approval, &approved, &until)
	if err != nil {
		return "", err
	}
	if approval && !approved {
		if _, err = tx.Exec(ctx, `UPDATE operations SET approval_required=true WHERE id=$1`, id); err != nil {
			return "", err
		}
		return "Waiting for a team administrator to approve this deployment.", nil
	}
	if until != nil {
		return "Deployment freeze is active until " + until.UTC().Format(time.RFC3339) + ".", nil
	}
	rows, err := tx.Query(ctx, `SELECT p.window_start,p.window_end FROM deployment_policies p JOIN workloads w ON p.team_id=w.team_id AND (p.project_id=w.project_id OR p.environment_id=w.environment_id) JOIN operations o ON o.workload_id=w.id WHERE o.id=$1`, id)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	now := time.Now().UTC().Format("15:04")
	for rows.Next() {
		var start, end string
		if err = rows.Scan(&start, &end); err != nil {
			return "", err
		}
		if !withinDeploymentWindow(start, end, now) {
			return "Waiting for the daily UTC deployment window " + start + "–" + end + ".", nil
		}
	}
	if err = rows.Err(); err != nil {
		return "", err
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
	result, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE operations SET result=$2,updated_at=now() WHERE id=$1`, id, result); err != nil {
		return false, err
	}
	return false, tx.Commit(ctx)
}
