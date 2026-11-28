package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
)

type changeWindow struct {
	Days  []int  `json:"days"`
	Start string `json:"start"`
	End   string `json:"end"`
}
type changeFreeze struct {
	From   time.Time `json:"from"`
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
}

func normalizeSchedule(p *deploymentPolicy) {
	if p.Timezone == "" {
		p.Timezone = "UTC"
	}
	if p.OutsideWindow == "" {
		p.OutsideWindow = "block"
	}
	if p.Windows == nil {
		p.Windows = []changeWindow{}
	}
	if p.Freezes == nil {
		p.Freezes = []changeFreeze{}
	}
}

func validateSchedule(p *deploymentPolicy) error {
	normalizeSchedule(p)
	if !validDeploymentWindow(p.WindowStart, p.WindowEnd) {
		return errors.New("set distinct local HH:MM times or clear both")
	}
	if (p.FreezeFrom == nil) != (p.FreezeUntil == nil) || p.FreezeFrom != nil && (!p.FreezeUntil.After(*p.FreezeFrom) || p.FreezeUntil.Sub(*p.FreezeFrom) > 366*24*time.Hour) {
		return errors.New("valid freeze start/end within one year required")
	}
	if p.Timezone == "Local" {
		return errors.New("use an explicit IANA timezone")
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return errors.New("valid IANA timezone required")
	}
	if p.OutsideWindow != "block" && p.OutsideWindow != "approval" {
		return errors.New("outside_window must be block or approval")
	}
	if len(p.Windows) > 32 || len(p.Freezes) > 32 {
		return errors.New("at most 32 windows and 32 freezes allowed")
	}
	for _, w := range p.Windows {
		if len(w.Days) == 0 || len(w.Days) > 7 || !validDeploymentWindow(w.Start, w.End) || w.Start == "" {
			return errors.New("each recurring window needs weekdays 0–6 and distinct HH:MM times")
		}
		seen := map[int]bool{}
		for _, day := range w.Days {
			if day < 0 || day > 6 || seen[day] {
				return errors.New("window weekdays must be distinct values from 0 through 6")
			}
			seen[day] = true
		}
	}
	for _, f := range p.Freezes {
		if f.From.IsZero() || !f.Until.After(f.From) || f.Until.Sub(f.From) > 366*24*time.Hour || len(f.Reason) > 1000 || strings.ContainsRune(f.Reason, 0) {
			return errors.New("freeze end must follow its start within one year; reason at most 1000 bytes")
		}
	}
	return nil
}

func scheduleWindowAllows(p deploymentPolicy, now time.Time) (bool, error) {
	location, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return false, err
	}
	local := now.In(location)
	clock := local.Format("15:04")
	if !withinDeploymentWindow(p.WindowStart, p.WindowEnd, clock) {
		return false, nil
	}
	if len(p.Windows) == 0 {
		return true, nil
	}
	for _, w := range p.Windows {
		day := int(local.Weekday())
		if w.Start > w.End && clock < w.End {
			day = (day + 6) % 7
		}
		if withinDeploymentWindow(w.Start, w.End, clock) {
			for _, allowed := range w.Days {
				if allowed == day {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func scheduledDeploymentGate(ctx context.Context, tx pgx.Tx, id string, now time.Time) (string, error) {
	var required, approved, override bool
	if err := tx.QueryRow(ctx, "SELECT approval_required,approved_at IS NOT NULL,emergency_override_at IS NOT NULL FROM operations WHERE id=$1", id).Scan(&required, &approved, &override); err != nil {
		return "", err
	}
	if override {
		return "", nil
	}
	rows, err := tx.Query(ctx, `SELECT p.approval,p.freeze_from,p.freeze_until,p.window_start,p.window_end,p.timezone,p.recurring_windows,p.freezes,p.outside_window FROM deployment_policies p JOIN workloads w ON p.team_id=w.team_id AND ((p.project_id IS NULL AND p.environment_id IS NULL) OR p.project_id=w.project_id OR p.environment_id=cove_deployment_environment(w.id)) JOIN operations o ON o.workload_id=w.id WHERE o.id=$1`, id)
	if err != nil {
		return "", err
	}
	policies := []deploymentPolicy{}
	for rows.Next() {
		var p deploymentPolicy
		if err = rows.Scan(&p.Approval, &p.FreezeFrom, &p.FreezeUntil, &p.WindowStart, &p.WindowEnd, &p.Timezone, &p.Windows, &p.Freezes, &p.OutsideWindow); err != nil {
			rows.Close()
			return "", err
		}
		policies = append(policies, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", err
	}
	for _, p := range policies {
		if p.Approval {
			required = true
		}
	}
	if required && !approved {
		if _, err = tx.Exec(ctx, "UPDATE operations SET approval_required=true WHERE id=$1", id); err != nil {
			return "", err
		}
	}
	for _, p := range policies {
		if err = validateSchedule(&p); err != nil {
			return "", err
		}
		if p.Approval {
			required = true
		}
		if p.FreezeFrom != nil && !now.Before(*p.FreezeFrom) && now.Before(*p.FreezeUntil) {
			return "Deployment freeze is active until " + p.FreezeUntil.UTC().Format(time.RFC3339) + ".", nil
		}
		for _, f := range p.Freezes {
			if !now.Before(f.From) && now.Before(f.Until) {
				return "Deployment freeze is active until " + f.Until.UTC().Format(time.RFC3339) + ".", nil
			}
		}
		allowed, e := scheduleWindowAllows(p, now)
		if e != nil {
			return "", e
		}
		if !allowed {
			if p.OutsideWindow == "approval" {
				required = true
			} else {
				return "Waiting for an allowed deployment window in " + p.Timezone + ".", nil
			}
		}
	}
	if required && !approved {
		if _, err = tx.Exec(ctx, "UPDATE operations SET approval_required=true WHERE id=$1", id); err != nil {
			return "", err
		}
		return "Waiting for a team administrator to approve this deployment.", nil
	}
	return "", nil
}

func (s *Server) emergencyOverride(w http.ResponseWriter, r *http.Request) {
	if !s.teamAdmin(r, actor(r).Team) {
		http.Error(w, "team administrator required", 403)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" || len(in.Reason) > 1000 || strings.ContainsRune(in.Reason, 0) {
		http.Error(w, "an emergency reason of 1–1000 bytes is required", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "operation unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended('cove-quota:'||$1,0))`, actor(r).Team); err != nil {
		http.Error(w, "operation unavailable", 503)
		return
	}
	result, err := tx.Exec(r.Context(), `UPDATE operations o SET emergency_override_by=$2,emergency_override_reason=$3,emergency_override_at=now() FROM workloads w WHERE o.id=$1 AND w.id=o.workload_id AND w.team_id=$4 AND o.state='queued' AND o.admitted_at IS NULL AND NOT o.cancel_requested AND o.action IN ('deploy','rollback','registry_maintenance') AND o.emergency_override_at IS NULL`, r.PathValue("id"), actor(r).ID, in.Reason, actor(r).Team)
	if err != nil {
		http.Error(w, "could not override deployment", 503)
		return
	}
	if result.RowsAffected() != 1 {
		http.Error(w, "operation no longer permits an emergency override", 409)
		return
	}
	details, _ := json.Marshal(map[string]string{"reason": in.Reason})
	if _, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status,team_id,details) VALUES($1,'emergency deployment override',$2,200,$3,$4)", actor(r).ID, r.PathValue("id"), actor(r).Team, details); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not audit emergency override", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
