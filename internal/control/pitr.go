package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

type pitrConfiguration struct {
	Policy worker.PITRPolicy   `json:"policy"`
	Backup worker.BackupConfig `json:"backup"`
}

func (s *Server) decodePITR(encrypted []byte) (pitrConfiguration, error) {
	var config pitrConfiguration
	data, err := unseal(s.Key, encrypted)
	if err != nil || json.Unmarshal(data, &config) != nil {
		return config, errors.New("PITR configuration unavailable")
	}
	return config, worker.ValidatePITR(config.Policy.Settings)
}

func (s *Server) getPITR(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var encrypted []byte
	var node string
	var operation *string
	var capable bool
	err := s.DB.QueryRow(r.Context(), `SELECT w.node_id,COALESCE((n.capabilities->>'postgres_pitr')::boolean,false),p.configuration,p.operation_id FROM workloads w JOIN nodes n ON n.id=w.node_id LEFT JOIN workload_pitr p ON p.workload_id=w.id WHERE w.id=$1`, r.PathValue("id")).Scan(&node, &capable, &encrypted, &operation)
	if err != nil {
		http.Error(w, "PITR configuration unavailable", 503)
		return
	}
	if encrypted == nil {
		respond(w, 200, map[string]any{"configured": false, "supported": capable, "settings": nil, "status": nil})
		return
	}
	config, err := s.decodePITR(encrypted)
	if err != nil {
		http.Error(w, "PITR configuration unavailable", 503)
		return
	}
	response, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "pitr_status", ID: r.PathValue("id")})
	if err != nil || response.PITR == nil {
		http.Error(w, "PITR status unavailable; verify worker connectivity", 502)
		return
	}
	respond(w, 200, map[string]any{"configured": true, "supported": capable, "settings": config.Policy.Settings, "storage_id": config.Backup.StorageID, "status": response.PITR, "operation_id": operation})
}

func (s *Server) changePITR(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in struct {
		Settings       worker.PITRSettings `json:"settings"`
		ConfirmRestart bool                `json:"confirm_restart"`
		ConfirmDelete  bool                `json:"confirm_delete"`
	}
	if !decode(w, r, &in) {
		return
	}
	action := "pitr_configure"
	if strings.HasSuffix(r.URL.Path, "/backup") {
		action = "pitr_backup"
	}
	if r.Method == http.MethodDelete {
		action = "pitr_forget"
		if !in.ConfirmDelete {
			http.Error(w, "confirm permanent deletion of PITR history", 400)
			return
		}
	}
	if action == "pitr_configure" {
		if err := worker.ValidatePITR(in.Settings); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(783529104)`); err != nil {
		http.Error(w, "PITR configuration unavailable", 503)
		return
	}
	var node, kind string
	var configuration []byte
	var capable, busy bool
	err = tx.QueryRow(r.Context(), `SELECT w.node_id,w.kind,w.configuration,COALESCE((n.capabilities->>'postgres_pitr')::boolean,false),EXISTS(SELECT 1 FROM operations WHERE workload_id=w.id AND state IN ('queued','running')) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w`, r.PathValue("id")).Scan(&node, &kind, &configuration, &capable, &busy)
	if err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	if kind == "job" || !capable || busy {
		http.Error(w, "select a service on an upgraded, verified worker and wait for its active operation", 409)
		return
	}
	var encrypted []byte
	err = tx.QueryRow(r.Context(), `SELECT configuration FROM workload_pitr WHERE workload_id=$1 FOR UPDATE`, r.PathValue("id")).Scan(&encrypted)
	var config pitrConfiguration
	if errors.Is(err, pgx.ErrNoRows) {
		if action != "pitr_configure" || !in.Settings.Enabled {
			http.Error(w, "enable PITR capture first", 409)
			return
		}
		var count, nodeCount int
		if err = tx.QueryRow(r.Context(), `SELECT count(*),count(*) FILTER(WHERE w.node_id=$1) FROM workload_pitr p JOIN workloads w ON w.id=p.workload_id`, node).Scan(&count, &nodeCount); err != nil {
			http.Error(w, "PITR inventory unavailable", 503)
			return
		}
		if count >= 256 || nodeCount >= 32 {
			http.Error(w, "PITR supports 32 resources per node and 256 per panel", 409)
			return
		}
		data, err := unseal(s.Key, configuration)
		var workload workloadInput
		if err != nil || json.Unmarshal(data, &workload) != nil || workload.Backup == nil {
			http.Error(w, "save a backup storage policy before enabling PITR", 409)
			return
		}
		config.Backup = *workload.Backup
		if config.Backup.Remote == nil && strings.HasPrefix(config.Backup.Repository, "s3:") && config.Backup.StorageID == "" {
			http.Error(w, "use a configured S3 storage location for PITR rather than a legacy inline repository", 409)
			return
		}
		var material [32]byte
		if _, err = rand.Read(material[:]); err != nil {
			http.Error(w, "encryption unavailable", 503)
			return
		}
		config.Policy = worker.PITRPolicy{Key: hex.EncodeToString(material[:]), Archive: resourceID(), Settings: in.Settings}
	} else if err != nil {
		http.Error(w, "PITR configuration unavailable", 503)
		return
	} else {
		config, err = s.decodePITR(encrypted)
		if err != nil {
			http.Error(w, "PITR configuration unavailable", 503)
			return
		}
		if action == "pitr_configure" {
			if config.Policy.Settings.Service != in.Settings.Service {
				http.Error(w, "delete the retained PITR history before selecting another service", 409)
				return
			}
			config.Policy.Settings = in.Settings
		}
	}
	if action == "pitr_configure" && !in.ConfirmRestart {
		http.Error(w, "confirm the PostgreSQL restart required by capture configuration changes", 400)
		return
	}
	if action == "pitr_forget" && config.Policy.Settings.Enabled {
		http.Error(w, "disable capture before deleting its history", 409)
		return
	}
	if action == "pitr_backup" && !config.Policy.Settings.Enabled {
		http.Error(w, "enable capture before taking a base backup", 409)
		return
	}
	request := worker.Request{ID: resourceID(), Workload: r.PathValue("id"), Action: action, Kind: "service", PITR: &config.Policy, Backup: &config.Backup}
	if err = worker.ValidateRequest(request); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err = s.prepareInfrastructure(r.Context(), &request); err != nil {
		http.Error(w, "PITR storage unavailable", 503)
		return
	}
	if err = s.insertPITROperation(r.Context(), tx, request); err != nil {
		http.Error(w, "could not queue PITR action", 503)
		return
	}
	data, err := json.Marshal(config)
	if err != nil {
		http.Error(w, "PITR configuration unavailable", 503)
		return
	}
	encrypted, err = seal(s.Key, data)
	if err != nil {
		http.Error(w, "could not protect PITR configuration", 503)
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO workload_pitr(workload_id,configuration,enabled,operation_id,next_backup_at) VALUES($1,$2,$3,$4,now()+interval '1 minute') ON CONFLICT(workload_id) DO UPDATE SET configuration=EXCLUDED.configuration,enabled=EXCLUDED.enabled,operation_id=EXCLUDED.operation_id,next_backup_at=EXCLUDED.next_backup_at`, request.Workload, encrypted, config.Policy.Settings.Enabled, request.ID)
	if err != nil || tx.Commit(r.Context()) != nil {
		http.Error(w, "could not save PITR action", 503)
		return
	}
	respond(w, 202, map[string]string{"id": request.ID})
}

func (s *Server) insertPITROperation(ctx context.Context, tx pgx.Tx, request worker.Request) error {
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	payload, err := seal(s.Key, data)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,$3,$4)`, request.ID, request.Workload, request.Action, payload)
	return err
}

func (s *Server) schedulePITR(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT p.workload_id,p.configuration FROM workload_pitr p JOIN workloads w ON w.id=p.workload_id JOIN nodes n ON n.id=w.node_id WHERE p.enabled AND p.next_backup_at<=now() AND n.state='ready' AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) ORDER BY p.next_backup_at LIMIT 8 FOR UPDATE OF w,p SKIP LOCKED`)
	if err != nil {
		return err
	}
	type due struct {
		id        string
		encrypted []byte
	}
	jobs := []due{}
	for rows.Next() {
		var job due
		if err = rows.Scan(&job.id, &job.encrypted); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, job)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, job := range jobs {
		config, err := s.decodePITR(job.encrypted)
		if err != nil {
			return err
		}
		request := worker.Request{ID: resourceID(), Workload: job.id, Action: "pitr_backup", Kind: "service", PITR: &config.Policy, Backup: &config.Backup}
		if err = s.insertPITROperation(ctx, tx, request); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE workload_pitr SET next_backup_at=now()+make_interval(hours=>$2),operation_id=$3 WHERE workload_id=$1`, job.id, config.Policy.Settings.IntervalHours, request.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status) VALUES('scheduler','pitr_backup',$1,202)`, job.id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
