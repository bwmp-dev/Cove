package control

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

type panelRecoveryConfiguration struct {
	Config  panelBackupConfig       `json:"config"`
	Storage *worker.StorageLocation `json:"storage,omitempty"`
}

func (s *Server) registerPanelRecovery(mux *http.ServeMux) {
	for _, pattern := range []string{"GET /api/settings/panel-backup", "PUT /api/settings/panel-backup"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.panelBackupSettings)))
	}
	mux.Handle("POST /api/settings/panel-backup/actions", s.admin(http.HandlerFunc(s.panelBackupQueue)))
	mux.HandleFunc("POST /api/internal/panel-host/claim", s.panelHostClaim)
	mux.HandleFunc("POST /api/internal/panel-host/report", s.panelHostReport)
}

type panelRecoverySnapshot struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Version   string    `json:"version"`
	Revision  string    `json:"revision"`
	Bytes     int64     `json:"bytes"`
	Verified  bool      `json:"verified"`
	Offsite   bool      `json:"offsite"`
	Origin    string    `json:"origin"`
}

func defaultPanelBackupConfig() panelBackupConfig {
	return panelBackupConfig{IntervalHours: 24, KeepLast: 7, KeepDays: 30, MaxBytes: 8 << 30}
}

func (s *Server) panelBackupSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var encrypted []byte
	err := s.DB.QueryRow(r.Context(), "SELECT configuration FROM panel_recovery WHERE id=1").Scan(&encrypted)
	if err != nil {
		http.Error(w, "panel backup settings unavailable", 503)
		return
	}
	config := panelRecoveryConfiguration{Config: defaultPanelBackupConfig()}
	if len(encrypted) > 0 {
		plain, err := unseal(s.Key, encrypted)
		if err != nil || json.Unmarshal(plain, &config) != nil {
			http.Error(w, "panel backup settings unavailable", 503)
			return
		}
	}
	if r.Method == http.MethodPut {
		var in panelBackupConfig
		if !decode(w, r, &in) {
			return
		}
		if in.Password == "" && in.StorageID == config.Config.StorageID {
			in.Password = config.Config.Password
		}
		var deferred bool
		if err := s.DB.QueryRow(r.Context(), "SELECT last_error<>'' FROM panel_recovery WHERE id=1").Scan(&deferred); err != nil {
			http.Error(w, "panel host status unavailable", 503)
			return
		}
		if deferred && in.StorageID != config.Config.StorageID {
			http.Error(w, "finish the host journal before changing its offsite destination; credentials for the same destination can be corrected", 409)
			return
		}
		if err := validatePanelBackupConfig(in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var storage *worker.StorageLocation
		if in.StorageID != "" {
			connection, err := s.readConnection(r.Context(), in.StorageID, actor(r).Team, "storage")
			if err != nil || connection.Storage == nil {
				http.Error(w, "choose a storage location in the current team", 400)
				return
			}
			storage = connection.Storage
		} else {
			in.Password = ""
		}
		config = panelRecoveryConfiguration{Config: in, Storage: storage}
		plain, err := json.Marshal(config)
		if err == nil {
			encrypted, err = seal(s.Key, plain)
		}
		if err == nil {
			result, updateErr := s.DB.Exec(r.Context(), "UPDATE panel_recovery SET configuration=$1 WHERE id=1 AND NOT EXISTS(SELECT 1 FROM panel_recovery_jobs WHERE state IN ('queued','running'))", encrypted)
			err = updateErr
			if err == nil && result.RowsAffected() != 1 {
				http.Error(w, "finish the pending panel recovery action first", 409)
				return
			}
		}
		if err != nil {
			http.Error(w, "could not save panel backup settings", 503)
			return
		}
	}
	passwordSet := config.Config.Password != ""
	config.Config.Password = ""
	var agent, errorText string
	var checked *time.Time
	var paused bool
	var pending bool
	var offsitePending bool
	var snapshots json.RawMessage
	err = s.DB.QueryRow(r.Context(), "SELECT agent_id,checked_at,last_error,updates_paused,pending_recovery,pending_remote_job<>'',snapshots FROM panel_recovery WHERE id=1").Scan(&agent, &checked, &errorText, &paused, &pending, &offsitePending, &snapshots)
	if err != nil {
		http.Error(w, "panel recovery status unavailable", 503)
		return
	}
	rows, err := s.DB.Query(r.Context(), "SELECT id,action,snapshot_id,state,requested_at,finished_at,last_error FROM panel_recovery_jobs ORDER BY requested_at DESC,id DESC LIMIT 100")
	if err != nil {
		http.Error(w, "panel recovery history unavailable", 503)
		return
	}
	defer rows.Close()
	type job struct {
		ID          string     `json:"id"`
		Action      string     `json:"action"`
		SnapshotID  string     `json:"snapshot_id"`
		State       string     `json:"state"`
		RequestedAt time.Time  `json:"requested_at"`
		FinishedAt  *time.Time `json:"finished_at"`
		Error       string     `json:"error"`
	}
	jobs := []job{}
	for rows.Next() {
		var item job
		if rows.Scan(&item.ID, &item.Action, &item.SnapshotID, &item.State, &item.RequestedAt, &item.FinishedAt, &item.Error) != nil {
			http.Error(w, "panel recovery history unavailable", 503)
			return
		}
		jobs = append(jobs, item)
	}
	if rows.Err() != nil {
		http.Error(w, "panel recovery history unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"config": config.Config, "password_set": passwordSet, "agent_id": agent, "checked_at": checked, "last_error": errorText, "updates_paused": paused, "pending_recovery": pending, "offsite_pending": offsitePending, "snapshots": snapshots, "jobs": jobs, "public_origin": s.PublicURL})
}

func (s *Server) panelBackupQueue(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action     string `json:"action"`
		SnapshotID string `json:"snapshot_id"`
		Confirm    bool   `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Action != "backup" && in.Action != "restore" && in.Action != "resume_updates" || in.Action != "backup" && !in.Confirm || in.Action == "restore" && !validPanelSnapshot(in.SnapshotID) || in.Action != "restore" && in.SnapshotID != "" {
		http.Error(w, "choose and confirm a valid recovery action", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var recent bool
	var pending bool
	var raw json.RawMessage
	err = tx.QueryRow(r.Context(), "SELECT agent_id<>'' AND checked_at>now()-interval '3 minutes',snapshots,pending_recovery FROM panel_recovery WHERE id=1 FOR UPDATE").Scan(&recent, &raw, &pending)
	if err != nil {
		http.Error(w, "panel recovery status unavailable", 503)
		return
	}
	if !recent {
		http.Error(w, "install or reconnect the panel host recovery timer first", 409)
		return
	}
	if pending {
		http.Error(w, "the host is recovering its existing journal; correct its configuration or inspect the host before another action", 409)
		return
	}
	if in.Action == "restore" {
		var snapshots []panelRecoverySnapshot
		if json.Unmarshal(raw, &snapshots) != nil {
			http.Error(w, "snapshot inventory unavailable", 503)
			return
		}
		found := false
		for _, item := range snapshots {
			if item.ID == in.SnapshotID && item.Verified && item.Origin == s.PublicURL {
				found = true
			}
		}
		if !found {
			http.Error(w, "choose a verified retained snapshot for this public origin", 400)
			return
		}
	}
	id := randomToken()
	_, err = tx.Exec(r.Context(), "INSERT INTO panel_recovery_jobs(id,actor_id,action,snapshot_id) VALUES($1,$2,$3,$4)", id, actor(r).ID, in.Action, in.SnapshotID)
	if err != nil {
		http.Error(w, "another panel recovery action is pending", 409)
		return
	}
	if tx.Commit(r.Context()) != nil {
		http.Error(w, "could not queue recovery action", 503)
		return
	}
	respond(w, 202, map[string]string{"id": id, "state": "queued"})
}

func (s *Server) panelHostClaim(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	data, ok := s.authenticatePanelHost(w, r)
	if !ok {
		return
	}
	var in struct {
		AgentID         string `json:"agent_id"`
		UpdatesPaused   bool   `json:"updates_paused"`
		ForceBackup     bool   `json:"force_backup"`
		ResumeJobID     string `json:"resume_job_id"`
		PendingRecovery bool   `json:"pending_recovery"`
	}
	if json.Unmarshal(data, &in) != nil || !panelPlanID(in.AgentID) || in.ResumeJobID != "" && !panelPlanID(in.ResumeJobID) {
		http.Error(w, "invalid host identity", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var agent string
	var encrypted []byte
	var scheduled *time.Time
	err = tx.QueryRow(r.Context(), "SELECT agent_id,configuration,last_scheduled_at FROM panel_recovery WHERE id=1 FOR UPDATE").Scan(&agent, &encrypted, &scheduled)
	if err != nil {
		http.Error(w, "panel backup configuration unavailable", 503)
		return
	}
	if agent != "" && agent != in.AgentID {
		http.Error(w, "a different panel host is already registered", 409)
		return
	}
	config := panelRecoveryConfiguration{Config: defaultPanelBackupConfig()}
	if len(encrypted) > 0 {
		plain, err := unseal(s.Key, encrypted)
		if err != nil || json.Unmarshal(plain, &config) != nil {
			http.Error(w, "panel backup configuration unavailable", 503)
			return
		}
	}
	_, err = tx.Exec(r.Context(), "UPDATE panel_recovery SET agent_id=$1,checked_at=now(),updates_paused=$2,pending_recovery=$3 WHERE id=1", in.AgentID, in.UpdatesPaused, in.PendingRecovery)
	if err != nil {
		http.Error(w, "could not register panel host", 503)
		return
	}
	plan := panelHostPlan{Kind: "cove.panel-backup.v1", AgentID: in.AgentID, Config: config.Config, Storage: config.Storage}
	proxy, err := s.outboundConfig(r.Context())
	if err != nil {
		http.Error(w, "outbound configuration unavailable", 503)
		return
	}
	plan.OutboundProxy = &proxy
	if in.ResumeJobID != "" {
		err = tx.QueryRow(r.Context(), "SELECT id,actor_id,action,snapshot_id FROM panel_recovery_jobs WHERE id=$1 AND action='backup'", in.ResumeJobID).Scan(&plan.JobID, &plan.ActorID, &plan.Action, &plan.SnapshotID)
		if err != nil {
			http.Error(w, "pending backup receipt unavailable", 409)
			return
		}
	} else {
		err = tx.QueryRow(r.Context(), "SELECT id,actor_id,action,snapshot_id FROM panel_recovery_jobs WHERE state IN ('queued','running') ORDER BY requested_at LIMIT 1").Scan(&plan.JobID, &plan.ActorID, &plan.Action, &plan.SnapshotID)
	}
	if errors.Is(err, pgx.ErrNoRows) && (in.ForceBackup || config.Config.Enabled && (scheduled == nil || time.Since(*scheduled) >= time.Duration(config.Config.IntervalHours)*time.Hour)) {
		plan.JobID, plan.ActorID, plan.Action = randomToken(), "host-schedule", "backup"
		_, err = tx.Exec(r.Context(), "INSERT INTO panel_recovery_jobs(id,actor_id,action) VALUES($1,$2,$3)", plan.JobID, plan.ActorID, plan.Action)
		if err == nil {
			_, err = tx.Exec(r.Context(), "UPDATE panel_recovery SET last_scheduled_at=now() WHERE id=1")
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if tx.Commit(r.Context()) != nil {
			http.Error(w, "could not register panel host", 503)
			return
		}
		respond(w, 200, map[string]any{"sealed": "", "config": map[string]any{"enabled": config.Config.Enabled}})
		return
	}
	if err != nil {
		http.Error(w, "panel recovery queue unavailable", 503)
		return
	}
	if validatePanelHostPlan(plan) != nil {
		http.Error(w, "panel recovery plan invalid", 503)
		return
	}
	plain, err := json.Marshal(plan)
	if err == nil {
		encrypted, err = seal(s.Key, plain)
	}
	if err == nil && in.ResumeJobID == "" {
		_, err = tx.Exec(r.Context(), "UPDATE panel_recovery_jobs SET state='running' WHERE id=$1", plan.JobID)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not claim panel recovery action", 503)
		return
	}
	respond(w, 200, map[string]string{"sealed": base64.StdEncoding.EncodeToString(encrypted)})
	s.recordAudit(r, "panel-host:"+in.AgentID, 200)
}

func (s *Server) panelHostReport(w http.ResponseWriter, r *http.Request) {
	data, ok := s.authenticatePanelHost(w, r)
	if !ok {
		return
	}
	var in struct {
		AgentID          string                  `json:"agent_id"`
		JobID            string                  `json:"job_id"`
		Action           string                  `json:"action"`
		SnapshotID       string                  `json:"snapshot_id"`
		ActorID          string                  `json:"actor_id"`
		State            string                  `json:"state"`
		Error            string                  `json:"error"`
		UpdatesPaused    bool                    `json:"updates_paused"`
		PendingRecovery  bool                    `json:"pending_recovery"`
		PendingRemoteJob string                  `json:"pending_remote_job"`
		Snapshots        []panelRecoverySnapshot `json:"snapshots"`
	}
	if json.Unmarshal(data, &in) != nil || !panelPlanID(in.AgentID) || in.JobID != "" && !panelPlanID(in.JobID) || in.PendingRemoteJob != "" && !panelPlanID(in.PendingRemoteJob) || in.State != "succeeded" && in.State != "failed" || len(in.Error) > 1000 || strings.ContainsAny(in.Error, "\x00\r\n") || len(in.Snapshots) > 102 {
		http.Error(w, "invalid host report", 400)
		return
	}
	seen := map[string]bool{}
	for _, item := range in.Snapshots {
		if !validPanelSnapshot(item.ID) || seen[item.ID] || item.Bytes < 0 || item.Bytes > worker.MaxTransferBytes || item.CreatedAt.After(time.Now().Add(5*time.Minute)) || item.CreatedAt.Before(time.Unix(0, 0)) || len(item.Version) > 128 || len(item.Revision) > 128 || len(item.Origin) > 2048 {
			http.Error(w, "invalid snapshot inventory", 400)
			return
		}
		seen[item.ID] = true
	}
	raw, err := json.Marshal(in.Snapshots)
	if err != nil {
		http.Error(w, "invalid host report", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var agent string
	if tx.QueryRow(r.Context(), "SELECT agent_id FROM panel_recovery WHERE id=1 FOR UPDATE").Scan(&agent) != nil {
		http.Error(w, "host status unavailable", 503)
		return
	}
	if agent != "" && agent != in.AgentID {
		http.Error(w, "different panel host registered", 409)
		return
	}
	_, err = tx.Exec(r.Context(), "UPDATE panel_recovery SET agent_id=$1,checked_at=now(),updates_paused=$2,last_error=$3,snapshots=$4,pending_recovery=$5,pending_remote_job=$6 WHERE id=1", in.AgentID, in.UpdatesPaused, in.Error, raw, in.PendingRecovery, in.PendingRemoteJob)
	if err == nil && in.JobID != "" {
		if in.Action != "" && in.Action != "backup" && in.Action != "restore" && in.Action != "resume_updates" || len(in.ActorID) > 256 || in.SnapshotID != "" && !validPanelSnapshot(in.SnapshotID) {
			http.Error(w, "invalid host recovery receipt", 400)
			return
		}
		if in.Action != "" {
			_, err = tx.Exec(r.Context(), "INSERT INTO panel_recovery_jobs(id,actor_id,action,snapshot_id,state,finished_at,last_error) VALUES($1,$2,$3,$4,$5,now(),$6) ON CONFLICT(id) DO UPDATE SET state=EXCLUDED.state,finished_at=EXCLUDED.finished_at,last_error=EXCLUDED.last_error", in.JobID, in.ActorID, in.Action, in.SnapshotID, in.State, in.Error)
		} else {
			_, err = tx.Exec(r.Context(), "UPDATE panel_recovery_jobs SET state=$2,finished_at=now(),last_error=$3 WHERE id=$1 AND state IN ('queued','running')", in.JobID, in.State, in.Error)
		}
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM panel_recovery_jobs WHERE id<>$1 AND state NOT IN ('queued','running') AND (requested_at<now()-interval '30 days' OR id IN (SELECT id FROM panel_recovery_jobs WHERE state NOT IN ('queued','running') ORDER BY requested_at DESC,id DESC OFFSET 998))", in.PendingRemoteJob)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save panel recovery status", 503)
		return
	}
	respond(w, 200, map[string]bool{"accepted": true})
	if in.JobID != "" {
		s.recordAudit(r, "panel-host:"+in.AgentID, 200)
	}
}
