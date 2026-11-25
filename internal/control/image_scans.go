package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type imageScanningInput struct {
	Enabled   *bool `json:"enabled"`
	Notify    *bool `json:"notify"`
	Automatic *bool `json:"automatic"`
}

var scanSummaryCVE = regexp.MustCompile(`^CVE-[0-9]{4}-[0-9]{4,20}$`)

func (s *Server) scanNotification(ctx context.Context, id, action string) (event, summary string, allowed bool, err error) {
	if action != "image_scan" {
		return "", "", true, nil
	}
	var data []byte
	var operationState string
	var created time.Time
	err = s.DB.QueryRow(ctx, `SELECT o.result,COALESCE(c.enabled AND c.notify,false),o.state,o.created_at FROM operations o JOIN workloads w ON w.id=o.workload_id LEFT JOIN image_scanning_settings c ON c.team_id=w.team_id AND c.node_id=w.node_id WHERE o.id=$1`, id).Scan(&data, &allowed, &operationState, &created)
	if err != nil || !allowed {
		return
	}
	event, summary = "image.scan_unknown", "Coverage is unknown; inspect the scan operation."
	var operation worker.Operation
	if len(data) > 2*1024*1024 || json.Unmarshal(data, &operation) != nil {
		return
	}
	report := operation.ImageScan
	automatic := automaticScanOrigin(operation.ScanOrigin)
	if report == nil {
		if !automatic {
			return
		}
		report = &worker.ImageScan{State: "unknown", CheckedAt: created}
	}
	var current bool
	if err = s.DB.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM operations newer WHERE newer.workload_id=o.workload_id AND newer.action IN ('deploy','rollback','start','restart','stop','resume','suspend','move','rollout_promote','rollout_abort') AND newer.updated_at>$2) FROM operations o WHERE o.id=$1`, id, report.CheckedAt).Scan(&current); err != nil {
		return "", "", false, err
	}
	if operationState == "succeeded" && current && report.Complete(time.Now().UTC()) {
		event, summary = "image.scan_completed", "Scan completed; review findings and coverage."
	}
	images := report.Images
	if automatic {
		if !current || !currentAutomaticScanReport(report, time.Now().UTC()) {
			return "", "", false, nil
		}
		outcome := event
		if outcome == "image.scan_completed" && scanHasFindings(images) {
			outcome = "image.vulnerabilities"
		}
		images, allowed, err = s.automaticScanNotificationImages(ctx, id, outcome, report)
		if err != nil || !allowed {
			return "", "", false, err
		}
	}
	ids := map[string]bool{}
	counts := map[string]int{}
	findings := false
	for _, image := range images {
		for severity, count := range image.Counts {
			if count > 0 {
				counts[severity] += min(count, 32768)
				findings = true
			}
		}
		for _, finding := range image.Findings {
			if scanSummaryCVE.MatchString(finding.ID) {
				ids[finding.ID] = true
			}
		}
	}
	if findings {
		for _, severity := range []string{"CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"} {
			if counts[severity] > 0 {
				summary += " " + severity + ": " + strconv.Itoa(counts[severity]) + "."
			}
		}
		if event == "image.scan_completed" {
			event = "image.vulnerabilities"
		}
	}
	if len(ids) > 0 {
		ordered := []string{}
		for value := range ids {
			ordered = append(ordered, value)
		}
		sort.Strings(ordered)
		if len(ordered) > 10 {
			ordered = ordered[:10]
		}
		summary += " CVEs include " + strings.Join(ordered, ", ") + "."
	}
	return
}

func (s *Server) imageScanning(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var team, node string
	var ready, supported bool
	if err := s.DB.QueryRow(r.Context(), `SELECT w.team_id,w.node_id,n.state='ready',COALESCE((n.capabilities->>'image_scanning')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, id).Scan(&team, &node, &ready, &supported); err != nil {
		http.Error(w, "image scanning settings unavailable", 503)
		return
	}
	manage := s.teamAdmin(r, team)
	if r.Method == "PUT" {
		if !manage {
			http.Error(w, "a team administrator must opt this team and node into image scanning", 403)
			return
		}
		var input imageScanningInput
		if !decode(w, r, &input) {
			return
		}
		if input.Enabled == nil || input.Notify == nil {
			http.Error(w, "enabled and notify are required", 400)
			return
		}
		if *input.Enabled && (!ready || !supported) {
			http.Error(w, "the node operator must enable scanning and verify a ready worker first", 409)
			return
		}
		if input.Automatic != nil && *input.Automatic {
			var capable bool
			if err := s.DB.QueryRow(r.Context(), `SELECT COALESCE((capabilities->>'automatic_image_scans')::boolean,false) FROM nodes WHERE id=$1`, node).Scan(&capable); err != nil || !*input.Enabled || !ready || !supported || !capable {
				http.Error(w, "enable scanning on an upgraded ready worker before enabling automatic scans", 409)
				return
			}
		}
		if _, err := s.DB.Exec(r.Context(), `INSERT INTO image_scanning_settings(team_id,node_id,enabled,notify,automatic) VALUES($1,$2,$3,$4,COALESCE($5,false)) ON CONFLICT(team_id,node_id) DO UPDATE SET enabled=$3,notify=$4,automatic=COALESCE($5,image_scanning_settings.automatic)`, team, node, *input.Enabled, *input.Notify, input.Automatic); err != nil {
			http.Error(w, "could not save image scanning settings", 503)
			return
		}
	}
	var enabled, notify, automatic bool
	err := s.DB.QueryRow(r.Context(), `SELECT enabled,notify,automatic FROM image_scanning_settings WHERE team_id=$1 AND node_id=$2`, team, node).Scan(&enabled, &notify, &automatic)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "image scanning settings unavailable", 503)
		return
	}
	var active *string
	if err = s.DB.QueryRow(r.Context(), `SELECT (SELECT id FROM operations WHERE workload_id=$1 AND state IN ('queued','running') LIMIT 1)`, id).Scan(&active); err != nil {
		http.Error(w, "active operations unavailable", 503)
		return
	}
	var automaticSupported, automaticRunning bool
	var nextCheck *time.Time
	var automaticMessage string
	if err = s.DB.QueryRow(r.Context(), `SELECT COALESCE((capabilities->>'automatic_image_scans')::boolean,false),EXISTS(SELECT 1 FROM automatic_image_scans a WHERE a.workload_id=$2),(SELECT next_check_at FROM image_scan_automation_state WHERE workload_id=$2),COALESCE((SELECT last_error FROM image_scan_automation_state WHERE workload_id=$2),'') FROM nodes WHERE id=$1`, node, id).Scan(&automaticSupported, &automaticRunning, &nextCheck, &automaticMessage); err != nil {
		http.Error(w, "automatic scan status unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"enabled": enabled, "notify": notify, "automatic": automatic, "automatic_supported": automaticSupported, "automatic_running": automaticRunning, "next_automatic_check_at": nextCheck, "automatic_message": automaticMessage, "can_manage": manage, "node_id": node, "worker_supported": supported, "node_ready": ready, "active_operation_id": active, "scanner_image": worker.ScannerImage, "java_archives": "not_scanned", "database_limit_bytes": int64(2 * 1024 * 1024 * 1024), "database_max_age_hours": 24})
}

func (s *Server) requireImageScanning(ctx context.Context, id string) error {
	var enabled bool
	if err := s.DB.QueryRow(ctx, `SELECT n.state='ready' AND COALESCE((n.capabilities->>'image_scanning')::boolean,false) AND COALESCE(c.enabled,false) FROM workloads w JOIN nodes n ON n.id=w.node_id LEFT JOIN image_scanning_settings c ON c.team_id=w.team_id AND c.node_id=w.node_id WHERE w.id=$1`, id).Scan(&enabled); err != nil {
		return err
	}
	if !enabled {
		return errors.New("image scanning requires node-operator and team/node opt-in on a ready worker")
	}
	return nil
}

func (s *Server) workloadImageScans(w http.ResponseWriter, r *http.Request) {
	var id, state string
	var data []byte
	err := s.DB.QueryRow(r.Context(), `SELECT id,state,result FROM operations WHERE workload_id=$1 AND action='image_scan' AND state NOT IN ('queued','running') ORDER BY created_at DESC,id DESC LIMIT 1`, r.PathValue("id")).Scan(&id, &state, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		respond(w, 200, map[string]any{"state": "unknown", "stale": true, "operation_id": nil, "result": nil, "message": "No vulnerability scan has completed."})
		return
	}
	if err != nil {
		http.Error(w, "vulnerability results unavailable", 503)
		return
	}
	var operation worker.Operation
	if len(data) > 2*1024*1024 || (len(data) > 0 && json.Unmarshal(data, &operation) != nil) {
		http.Error(w, "vulnerability result is invalid", 503)
		return
	}
	stale := true
	if operation.ImageScan != nil {
		if err = s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND action IN ('deploy','rollback','start','restart','stop','resume','suspend','move','rollout_promote','rollout_abort') AND updated_at>$2)`, r.PathValue("id"), operation.ImageScan.CheckedAt).Scan(&stale); err != nil {
			http.Error(w, "vulnerability result freshness unavailable", 503)
			return
		}
		stale = stale || !operation.ImageScan.Database.Fresh(time.Now().UTC()) || operation.ImageScan.CheckedAt.IsZero() || operation.ImageScan.CheckedAt.After(time.Now().UTC())
	}
	effective := "unknown"
	if state == "succeeded" && operation.ImageScan != nil && operation.ImageScan.Complete(time.Now().UTC()) && !stale {
		effective = "complete"
	}
	respond(w, 200, map[string]any{"state": effective, "operation_state": state, "origin": operation.ScanOrigin, "stale": stale, "operation_id": id, "result": operation.ImageScan})
}
