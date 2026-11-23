package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

func databaseRevision(encrypted []byte) string {
	sum := sha256.Sum256(encrypted)
	return hex.EncodeToString(sum[:])
}

func (s *Server) databaseLocked(ctx context.Context, id string) bool {
	var locked bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM database_maintenance WHERE (state IN ('pending','failed') AND workload_id=$1) OR (state='pending' AND source_id=$1)) OR EXISTS(SELECT 1 FROM object_buckets WHERE state='pending' AND (workload_id=$1 OR storage_resource_id=$1))`, id).Scan(&locked)
	return err != nil || locked
}

func (s *Server) databaseMaintenance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), `SELECT m.id,m.kind,m.workload_id,COALESCE(m.source_id,''),m.state,m.last_error,o.state,m.created_at,m.cutover_at IS NOT NULL FROM database_maintenance m JOIN operations o ON o.id=m.id WHERE (m.workload_id=$1 OR m.source_id=$1) ORDER BY m.created_at DESC LIMIT 50`, id)
		if err != nil {
			http.Error(w, "maintenance unavailable", 503)
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var task, kind, target, source, state, message, operation string
			var created time.Time
			var cutover bool
			if err = rows.Scan(&task, &kind, &target, &source, &state, &message, &operation, &created, &cutover); err != nil {
				http.Error(w, "maintenance unavailable", 503)
				return
			}
			out = append(out, map[string]any{"id": task, "kind": kind, "resource_id": target, "source_id": source, "state": state, "operation_state": operation, "error": message, "created_at": created, "cutover_confirmed": cutover})
		}
		if rows.Err() != nil {
			http.Error(w, "maintenance unavailable", 503)
			return
		}
		rows.Close()
		for _, item := range out {
			eligible := false
			if item["kind"] == "upgrade" && item["state"] == "succeeded" && item["cutover_confirmed"] == false {
				var err error
				eligible, err = s.canCutoverDatabase(r, item["source_id"].(string), item["resource_id"].(string))
				if err != nil {
					http.Error(w, "copy permissions unavailable", 503)
					return
				}
			}
			item["can_cutover"] = eligible
		}
		respond(w, 200, out)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/retry") {
		s.retryDatabaseMaintenance(w, r)
		return
	}
	var in struct {
		Kind          string `json:"kind"`
		Revision      string `json:"revision"`
		Confirm       bool   `json:"confirm"`
		Extension     string `json:"extension"`
		Version       string `json:"version"`
		Name          string `json:"name"`
		Enabled       *bool  `json:"enabled"`
		WritesStopped bool   `json:"confirm_writes_stopped"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		http.Error(w, "confirm the maintenance and its effect on clients", 400)
		return
	}
	if in.Kind != "rotate" && in.Kind != "extension" && in.Kind != "upgrade" {
		http.Error(w, "select rotation, extension or upgrade", 400)
		return
	}
	proxy, err := s.outboundConfig(r.Context())
	if err != nil {
		http.Error(w, "worker transport configuration unavailable", 503)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var encrypted []byte
	var node, project, environment string
	var capable, active, deployed bool
	err = tx.QueryRow(r.Context(), `SELECT w.configuration,w.node_id,w.project_id,COALESCE(w.environment_id,''),COALESCE((n.capabilities->>'managed_databases')::boolean,false),NOT w.manually_stopped AND EXISTS(SELECT 1 FROM operations WHERE workload_id=w.id AND state='succeeded' AND action IN ('deploy','rollback')) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 AND w.team_id=$2 FOR UPDATE OF w`, id, actor(r).Team).Scan(&encrypted, &node, &project, &environment, &capable, &deployed)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if !capable {
		http.Error(w, "upgrade and verify the worker before managing databases", 409)
		return
	}
	if !deployed {
		http.Error(w, "deploy and start the database before queuing maintenance", 409)
		return
	}
	if in.Revision != databaseRevision(encrypted) {
		http.Error(w, "database changed; review its current configuration", 409)
		return
	}
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running')) OR EXISTS(SELECT 1 FROM database_maintenance WHERE (state IN ('pending','failed') AND workload_id=$1) OR (state='pending' AND source_id=$1))`, id).Scan(&active); err != nil {
		http.Error(w, "maintenance unavailable", 503)
		return
	}
	if active {
		http.Error(w, "finish or retry existing maintenance before starting another change", 409)
		return
	}
	var config workloadInput
	if s.unsealJSON(encrypted, &config) != nil || config.Database == nil {
		http.Error(w, "select a managed database", 400)
		return
	}
	engine, ok := findDatabaseEngine(config.Database.Engine)
	if !ok {
		http.Error(w, "unsupported engine", 400)
		return
	}
	task, target, source := resourceID(), id, ""
	change := worker.DatabaseMaintenance{ID: task, Kind: in.Kind, Engine: engine.id, Service: engine.service, PasswordKey: engine.password, Extension: in.Extension}
	switch in.Kind {
	case "rotate":
		change.PreviousPassword = config.Environment[engine.password]
		bytes := make([]byte, 32)
		if _, err = rand.Read(bytes); err != nil {
			http.Error(w, "could not generate credentials", 503)
			return
		}
		config.Environment[engine.password] = hex.EncodeToString(bytes)
		if config.Database.Pooler {
			if err = configureDatabasePooler(&config, true); err != nil {
				http.Error(w, "could not update pooled credentials", 503)
				return
			}
		}
	case "extension":
		if engine.id != "postgresql" {
			http.Error(w, "extensions require PostgreSQL", 400)
			return
		}
		enabled := in.Enabled == nil || *in.Enabled
		if err = changeDatabaseExtension(&config, in.Extension, enabled); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		change.DisableExtension = !enabled
		if !enabled {
			if _, err = s.nodeRPCUsing(r.Context(), tx, node, worker.RPCRequest{Method: "database_inspection", Database: &worker.DatabaseInspectionRequest{Workload: id, Engine: engine.id, Service: engine.service, DisableExtension: in.Extension}}, proxy); err != nil {
				http.Error(w, "extension removal requires all dependent objects to be removed first; no maintenance was queued", 409)
				return
			}
		}
	case "upgrade":
		if !in.WritesStopped {
			http.Error(w, "confirm application and external writers are stopped before taking the copy snapshot", 400)
			return
		}
		if !databaseCopyAllowed(config.Database, in.Version) {
			http.Error(w, "select a supported separate-copy target from the database version catalogue", 400)
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" || len(in.Name) > 120 {
			http.Error(w, "a target name of 1-120 characters is required", 400)
			return
		}
		target, source = resourceID(), id
		_, templateRevision, templateErr := databaseTemplate(engine)
		if templateErr != nil {
			http.Error(w, "copy template unavailable", 503)
			return
		}
		next := workloadInput{Name: in.Name, Kind: "database", Database: &databaseResource{Engine: engine.id, Version: in.Version, TemplateRevision: templateRevision}}
		if err = prepareDatabase(&next, target); err != nil {
			http.Error(w, "target template unavailable; reload the catalogue", 409)
			return
		}
		next.Limits = config.Limits
		reply, inspectErr := s.nodeRPCUsing(r.Context(), tx, node, worker.RPCRequest{Method: "database_inspection", Database: &worker.DatabaseInspectionRequest{Workload: id, Engine: engine.id, Service: engine.service}}, proxy)
		if inspectErr != nil || reply.Database == nil {
			http.Error(w, "source compatibility check unavailable; no copy was created", 502)
			return
		}
		if err = databaseCopyCompatibility(config.Database, next.Database, reply.Database); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		change.TargetVariant = next.Database.Variant
		change.SourceMajor = strings.Split(config.Database.Version, ".")[0]
		change.TargetMajor = strings.Split(next.Database.Version, ".")[0]
		if engine.id == "postgresql" {
			next.Database.Extensions = append(next.Database.Extensions, slices.Clone(reply.Database.Extensions)...)
			slices.Sort(next.Database.Extensions)
			next.Database.Extensions = slices.Compact(next.Database.Extensions)
			if next.Database.Variant != "" && !slices.Contains(next.Database.Extensions, next.Database.Variant) {
				next.Database.Extensions = append(next.Database.Extensions, next.Database.Variant)
			}
			if err = configureDatabaseExtensions(&next); err != nil {
				http.Error(w, "target extension configuration unavailable", 503)
				return
			}
			if config.Database.Pooler {
				if err = configureDatabasePooler(&next, true); err != nil {
					http.Error(w, "target pooler configuration unavailable", 503)
					return
				}
			}
		}
		config = next
		encrypted, err = s.sealJSON(config)
		if err != nil {
			http.Error(w, "could not protect copy configuration", 503)
			return
		}
		cpu, memory := configuredUsage(config)
		_, err = tx.Exec(r.Context(), `INSERT INTO workloads(id,node_id,name,configuration,kind,team_id,project_id,environment_id,configured_cpu,configured_memory_mb) VALUES($1,$2,$3,$4,'database',$5,$6,NULLIF($7,''),$8,$9)`, target, node, in.Name, encrypted, actor(r).Team, project, environment, cpu, memory)
		if err != nil {
			if !quotaError(w, err) {
				http.Error(w, "could not create isolated upgrade target", 503)
			}
			return
		}
		change.Source = source
	}
	change.Request = worker.Request{ID: task, Workload: target, Action: "deploy", Kind: "service", Compose: config.Compose, Environment: config.Environment, FileSecrets: config.FileSecrets, Limits: config.Limits, Routes: config.Routes, Private: config.Private}
	if err = worker.ValidateDatabaseMaintenance(change); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	request := change.Request
	request.DatabaseMaintenance = &change
	payload, err := s.sealJSON(request)
	if err != nil {
		http.Error(w, "could not protect maintenance", 503)
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,workload_id,action,payload,reason) VALUES($1,$2,'deploy',$3,$4)`, task, target, payload, "Database maintenance: "+in.Kind)
	if err != nil {
		http.Error(w, "could not queue maintenance", 503)
		return
	}
	encrypted, err = s.sealJSON(config)
	if err != nil {
		http.Error(w, "could not protect maintenance", 503)
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO database_maintenance(id,workload_id,source_id,node_id,kind,configuration) VALUES($1,$2,NULLIF($3,''),$4,$5,$6)`, task, target, source, node, in.Kind, encrypted)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,$2,$3,202,$4)`, actor(r).ID, "queue database "+in.Kind, id, actor(r).Team)
	}
	if err != nil || tx.Commit(r.Context()) != nil {
		http.Error(w, "could not save maintenance", 503)
		return
	}
	respond(w, 202, map[string]string{"id": task, "resource_id": target})
}

func (s *Server) finishDatabaseMaintenance(ctx context.Context, id, state string, result []byte) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var target string
	var config []byte
	if err = tx.QueryRow(ctx, `SELECT workload_id,configuration FROM database_maintenance WHERE id=$1 FOR UPDATE`, id).Scan(&target, &config); err != nil {
		return err
	}
	if state == "succeeded" {
		if _, err = tx.Exec(ctx, `UPDATE database_maintenance SET state='succeeded',last_error='',updated_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE workloads SET configuration=$2 WHERE id=$1`, target, config); err != nil {
			return err
		}
	} else if state == "failed" || state == "interrupted" || state == "cancelled" {
		if _, err = tx.Exec(ctx, `UPDATE database_maintenance SET state='failed',last_error='Maintenance was not confirmed. Retry the same maintenance before reading credentials or changing this resource.',updated_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE operations SET state=$2,result=$3,updated_at=now() WHERE id=$1`, id, state, result); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) retryDatabaseMaintenance(w http.ResponseWriter, r *http.Request) {
	var target, kind, source string
	err := s.DB.QueryRow(r.Context(), `SELECT m.workload_id,m.kind,COALESCE(m.source_id,'') FROM database_maintenance m JOIN operations o ON o.id=m.id WHERE m.id=$1 AND (m.workload_id=$2 OR m.source_id=$2) AND o.state IN ('failed','interrupted')`, r.PathValue("task"), r.PathValue("id")).Scan(&target, &kind, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "maintenance does not need retrying", 409)
		return
	}
	if err != nil {
		http.Error(w, "maintenance unavailable", 503)
		return
	}
	if !s.requireAccess(w, r, "workload", target, 3) {
		return
	}
	ids := []string{target}
	if kind == "upgrade" {
		var confirmation struct {
			WritesStopped bool `json:"confirm_writes_stopped"`
		}
		if !decode(w, r, &confirmation) {
			return
		}
		if !confirmation.WritesStopped {
			http.Error(w, "confirm writers are stopped again before retrying the copy", 400)
			return
		}
		if source == "" {
			http.Error(w, "source was removed; discard this failed copy instead", 409)
			return
		}
		if !s.requireAccess(w, r, "workload", source, 3) {
			return
		}
		ids = append(ids, source)
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "maintenance unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	rows, err := tx.Query(r.Context(), "SELECT id FROM workloads WHERE id=ANY($1) ORDER BY id FOR UPDATE", ids)
	if err != nil {
		http.Error(w, "maintenance unavailable", 503)
		return
	}
	locked := 0
	for rows.Next() {
		locked++
	}
	err = rows.Err()
	rows.Close()
	if err != nil || locked != len(ids) {
		http.Error(w, "maintenance unavailable", 503)
		return
	}
	message, err := deploymentGate(r.Context(), tx, r.PathValue("task"))
	if err != nil {
		http.Error(w, "deployment policy unavailable", 503)
		return
	}
	if message != "" {
		http.Error(w, message, 409)
		return
	}
	if _, err = tx.Exec(r.Context(), `WITH skipped AS (UPDATE operations SET state='cancelled',result='{"error":"Scheduled backup deferred for database maintenance retry."}'::jsonb,updated_at=now() WHERE workload_id=$1 AND id<>$2 AND state='queued' AND admitted_at IS NULL AND action IN ('backup','drill') RETURNING id) INSERT INTO audit_events(actor,action,subject,status,team_id) SELECT $3,'defer backup for database maintenance',id,200,$4 FROM skipped`, target, r.PathValue("task"), actor(r).ID, actor(r).Team); err != nil {
		http.Error(w, "could not defer scheduled backup", 503)
		return
	}
	var other bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=ANY($1) AND id<>$2 AND state IN ('queued','running'))", ids, r.PathValue("task")).Scan(&other); err != nil || other {
		http.Error(w, "finish source and target operations before retrying maintenance", 409)
		return
	}
	result, err := tx.Exec(r.Context(), "UPDATE operations SET state='queued',cancel_requested=false,result=NULL,updated_at=now() WHERE id=$1 AND state IN ('failed','interrupted')", r.PathValue("task"))
	if err != nil || result.RowsAffected() != 1 {
		http.Error(w, "maintenance no longer needs retrying", 409)
		return
	}
	if _, err = tx.Exec(r.Context(), "UPDATE database_maintenance SET state='pending',last_error='',retry_requested=true,updated_at=now() WHERE id=$1", r.PathValue("task")); err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'retry database maintenance',$2,202,$3)`, actor(r).ID, r.PathValue("task"), actor(r).Team)
	}
	if err != nil || tx.Commit(r.Context()) != nil {
		http.Error(w, "could not save maintenance retry", 503)
		return
	}
	respond(w, 202, map[string]string{"id": r.PathValue("task"), "resource_id": target})
}
