package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"net/http"
	"path"

	"github.com/jackc/pgx/v5"
)

func (s *Server) getBackupPolicy(w http.ResponseWriter, r *http.Request) {
	var encrypted []byte
	err := s.DB.QueryRow(r.Context(), "SELECT configuration FROM workloads WHERE id=$1", r.PathValue("id")).Scan(&encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	data, err := unseal(s.Key, encrypted)
	var config workloadInput
	if err != nil || json.Unmarshal(data, &config) != nil {
		http.Error(w, "configuration unavailable", 500)
		return
	}
	if config.Backup != nil {
		if config.Backup.StorageID != "" {
			c, err := s.readConnection(r.Context(), config.Backup.StorageID, actor(r).Team, "storage")
			if err != nil {
				http.Error(w, "storage location unavailable", 503)
				return
			}
			config.Backup.Repository = "rclone:cove:" + path.Join(c.Storage.Path, r.PathValue("id"))
		}
		config.Backup.Remote = nil
		config.Backup.Password, config.Backup.AccessKey, config.Backup.SecretKey, config.Backup.SessionToken = "", "", "", ""
	}
	respond(w, 200, map[string]any{"backup": config.Backup})
}

func (s *Server) setBackupPolicy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Backup *worker.BackupConfig `json:"backup"`
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
	var encrypted []byte
	err = tx.QueryRow(r.Context(), "SELECT configuration FROM workloads WHERE id=$1 FOR UPDATE", r.PathValue("id")).Scan(&encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	data, err := unseal(s.Key, encrypted)
	var config workloadInput
	if err != nil || json.Unmarshal(data, &config) != nil {
		http.Error(w, "configuration unavailable", 500)
		return
	}
	if in.Backup != nil {
		if in.Backup.KeepDaily > 0 || in.Backup.KeepWeekly > 0 || in.Backup.KeepMonthly > 0 {
			var capable bool
			if err = tx.QueryRow(r.Context(), `SELECT COALESCE((n.capabilities->>'backup_retention')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.PathValue("id")).Scan(&capable); err != nil {
				http.Error(w, "worker capabilities unavailable", 503)
				return
			}
			if !capable {
				http.Error(w, "upgrade and verify this worker before configuring daily, weekly or monthly retention", 409)
				return
			}
		}
		if in.Backup.Database != nil {
			var capable bool
			if err = tx.QueryRow(r.Context(), "SELECT COALESCE((n.capabilities->>'logical_backups')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1", r.PathValue("id")).Scan(&capable); err != nil || !capable {
				http.Error(w, "upgrade and verify this worker before configuring logical backups", 409)
				return
			}
		}
		if in.Backup.Remote != nil {
			http.Error(w, "select a storage location instead of supplying remote credentials", 400)
			return
		}
		if in.Backup.StorageID != "" {
			var raw []byte
			err = tx.QueryRow(r.Context(), `SELECT configuration FROM infrastructure_connections WHERE id=$1 AND team_id=$2 AND kind='storage' FOR SHARE`, in.Backup.StorageID, actor(r).Team).Scan(&raw)
			if err != nil {
				http.Error(w, "storage location not available to this team", 403)
				return
			}
			in.Backup.Repository = "storage:" + in.Backup.StorageID
			in.Backup.AccessKey = ""
			in.Backup.SecretKey = ""
			in.Backup.SessionToken = ""
			in.Backup.CACertificate = ""
		}
		if config.Kind == "job" {
			http.Error(w, "backup policies require a service", 400)
			return
		}
		if config.Backup != nil && config.Backup.Repository == in.Backup.Repository {
			if in.Backup.CACertificate == "" {
				in.Backup.CACertificate = config.Backup.CACertificate
			}
			if in.Backup.Password == "" {
				in.Backup.Password = config.Backup.Password
			}
			if in.Backup.AccessKey == "" {
				in.Backup.AccessKey = config.Backup.AccessKey
			}
			if in.Backup.SecretKey == "" {
				in.Backup.SecretKey = config.Backup.SecretKey
			}
			if in.Backup.SessionToken == "" {
				in.Backup.SessionToken = config.Backup.SessionToken
			}
		}
		if in.Backup.DrillIntervalHours > 0 {
			var supported bool
			if err = tx.QueryRow(r.Context(), `SELECT COALESCE((n.capabilities->>'restore_drills')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.PathValue("id")).Scan(&supported); err != nil {
				http.Error(w, "worker capabilities unavailable", 503)
				return
			}
			if !supported {
				http.Error(w, "upgrade and verify the worker before scheduling restore drills", 409)
				return
			}
		}
		if err = worker.ValidateBackup(in.Backup); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	}
	_, err = tx.Exec(r.Context(), "DELETE FROM workload_connections WHERE workload_id=$1 AND kind='storage'", r.PathValue("id"))
	if err == nil && in.Backup != nil && in.Backup.StorageID != "" {
		_, err = tx.Exec(r.Context(), `INSERT INTO workload_connections(workload_id,kind,connection_id) VALUES($1,'storage',$2)`, r.PathValue("id"), in.Backup.StorageID)
	}
	if err != nil {
		http.Error(w, "could not bind storage location", 503)
		return
	}
	config.Backup = in.Backup
	data, err = json.Marshal(config)
	if err == nil {
		encrypted, err = seal(s.Key, data)
	}
	if err == nil {
		interval := 0
		drillInterval := 0
		if in.Backup != nil {
			interval = in.Backup.IntervalHours
			drillInterval = in.Backup.DrillIntervalHours
		}
		_, err = tx.Exec(r.Context(), "UPDATE workloads SET configuration=$2,backup_interval=$3,drill_interval=$4,next_drill_at=CASE WHEN drill_interval<>$4 THEN now()+make_interval(hours=>$4) ELSE next_drill_at END,backup_policy_since=CASE WHEN backup_interval<>$3 THEN now() ELSE backup_policy_since END,next_backup_at=now()+make_interval(hours=>$3) WHERE id=$1", r.PathValue("id"), encrypted, interval, drillInterval)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save backup policy", 500)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) scheduleBackups(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "SELECT id,configuration FROM workloads WHERE backup_interval>0 AND next_backup_at<=now() ORDER BY next_backup_at LIMIT 32 FOR UPDATE SKIP LOCKED")
	if err != nil {
		return err
	}
	type due struct {
		id        string
		encrypted []byte
	}
	var jobs []due
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
		data, err := unseal(s.Key, job.encrypted)
		if err != nil {
			return err
		}
		var config workloadInput
		if err = json.Unmarshal(data, &config); err != nil {
			return err
		}
		request := worker.Request{ID: resourceID(), Workload: job.id, Action: "backup", Kind: config.Kind, Backup: config.Backup}
		if err = worker.ValidateRequest(request); err != nil {
			return err
		}
		data, err = json.Marshal(request)
		if err != nil {
			return err
		}
		payload, err := seal(s.Key, data)
		if err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `INSERT INTO operations(id,workload_id,action,payload)
		 SELECT $1,$2,'backup',$3 WHERE EXISTS(SELECT 1 FROM operations WHERE workload_id=$2 AND action IN ('deploy','rollback') AND state='succeeded')
		 ON CONFLICT DO NOTHING`, request.ID, job.id, payload)
		if err != nil {
			return err
		}
		if result.RowsAffected() > 0 {
			if _, err = tx.Exec(ctx, "INSERT INTO audit_events(actor,action,subject,status) VALUES('scheduler','backup',$1,202)", job.id); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, "UPDATE workloads SET next_backup_at=CASE WHEN $2 THEN now()+make_interval(hours=>backup_interval) ELSE now()+interval '1 minute' END WHERE id=$1", job.id, result.RowsAffected() > 0); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
