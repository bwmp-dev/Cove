package control

import (
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
)

func (s *Server) canCutoverDatabase(r *http.Request, source, target string) (bool, error) {
	if source == "" || target == "" || !s.teamAdmin(r, actor(r).Team) {
		return false, nil
	}
	for _, id := range []string{source, target} {
		for _, permission := range []string{"edit_configuration", "read_secrets"} {
			allowed, err := s.permission(r, "workload", id, permission)
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			if err != nil || !allowed {
				return false, err
			}
		}
	}
	ids := []string{source}
	if source != target {
		ids = append(ids, target)
	}
	var capable bool
	if err := s.DB.QueryRow(r.Context(), `SELECT count(*)=cardinality($1::text[]) AND COALESCE(bool_and(COALESCE((n.capabilities->>'managed_databases')::boolean,false)),false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=ANY($1) AND w.team_id=$2`, ids, actor(r).Team).Scan(&capable); err != nil {
		return false, err
	}
	return capable, nil
}

func (s *Server) cutoverDatabaseCopy(w http.ResponseWriter, r *http.Request) {
	if !s.teamAdmin(r, actor(r).Team) {
		http.Error(w, "team administrator required for coordinated database cut-over", 403)
		return
	}
	var in databaseCutoverInput
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm || !in.WritesStopped {
		http.Error(w, "confirm application writes have stopped and review snapshot freshness before switching bindings", 400)
		return
	}
	var source, target string
	if err := s.DB.QueryRow(r.Context(), `SELECT source_id,workload_id FROM database_maintenance WHERE id=$1 AND kind='upgrade' AND state='succeeded' AND (source_id=$2 OR workload_id=$2)`, r.PathValue("task"), r.PathValue("id")).Scan(&source, &target); err != nil {
		http.Error(w, "select a successfully verified copy", 409)
		return
	}
	if !s.requireAccess(w, r, "workload", source, 3) || !s.requireAccess(w, r, "workload", target, 3) || !s.requirePermission(w, r, "workload", source, "read_secrets") || !s.requirePermission(w, r, "workload", target, "read_secrets") {
		return
	}
	capable, err := s.canCutoverDatabase(r, source, target)
	if err != nil {
		http.Error(w, "database capabilities unavailable", 503)
		return
	}
	if !capable {
		http.Error(w, "upgrade and verify both database workers before cut-over", 409)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	rows, err := tx.Query(r.Context(), `SELECT id,configuration FROM workloads WHERE id=ANY($1) AND team_id=$2 ORDER BY id FOR UPDATE`, []string{source, target}, actor(r).Team)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	configs := map[string]workloadInput{}
	var original []byte
	for rows.Next() {
		var id string
		var raw []byte
		var config workloadInput
		if rows.Scan(&id, &raw) != nil || s.unsealJSON(raw, &config) != nil {
			rows.Close()
			http.Error(w, "database configuration unavailable", 503)
			return
		}
		configs[id] = config
		if id == source {
			original = raw
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(configs) != 2 {
		http.Error(w, "database unavailable", 503)
		return
	}
	for _, id := range []string{source, target} {
		if _, err = environmentOperationGate(r.Context(), tx, id, "drill"); err != nil {
			http.Error(w, "resume both database environments before cut-over", 409)
			return
		}
	}
	before, after := configs[source], configs[target]
	if before.Database == nil || after.Database == nil || before.Database.Engine != after.Database.Engine || databaseRevision(original) != in.Revision {
		http.Error(w, "source changed; review the current copy before cut-over", 409)
		return
	}
	var already, busy bool
	if tx.QueryRow(r.Context(), `SELECT cutover_at IS NOT NULL FROM database_maintenance WHERE id=$1 FOR UPDATE`, r.PathValue("task")).Scan(&already) != nil {
		http.Error(w, "copy record unavailable", 503)
		return
	}
	if already {
		respond(w, 200, map[string]any{"ok": true, "source_retained": true})
		return
	}
	if tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE (workload_id=ANY($1) OR workload_id IN (SELECT workload_id FROM resource_variable_bindings WHERE source_id=$2)) AND state IN ('queued','running'))`, []string{source, target}, source).Scan(&busy) != nil || busy {
		http.Error(w, "finish database and consumer operations before cut-over", 409)
		return
	}
	var pool bool
	if tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM resource_connections WHERE target_id=$1 AND target_service='pgbouncer' AND NOT removing) OR EXISTS(SELECT 1 FROM resource_variable_bindings WHERE source_id=$1 AND source_key LIKE 'DB_POOL_%')`, source).Scan(&pool) != nil || pool && !after.Database.Pooler {
		http.Error(w, "enable the target pooler before switching pooled clients", 409)
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE resource_variable_bindings SET source_id=$2 WHERE source_id=$1`, source, target); err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE resource_connections SET target_id=$2,alias=CASE WHEN target_service='pgbouncer' THEN $4 ELSE $3 END,version=version+1,updated_at=now() WHERE target_id=$1 AND NOT removing`, source, target, after.Database.Host, databasePoolHostname(after.Database))
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE database_maintenance SET cutover_at=now(),cutover_by=$2 WHERE id=$1`, r.PathValue("task"), actor(r).ID)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'confirm database copy cut-over',$2,200,$3)`, actor(r).ID, r.PathValue("task"), actor(r).Team)
	}
	if err != nil || tx.Commit(r.Context()) != nil {
		http.Error(w, "cut-over could not be recorded; bindings were not changed", 409)
		return
	}
	respond(w, 200, map[string]any{"ok": true, "source_retained": true, "redeploy_required": true, "resource_id": target})
}

type databaseCutoverInput struct {
	Confirm       bool   `json:"confirm"`
	WritesStopped bool   `json:"confirm_writes_stopped"`
	Revision      string `json:"revision"`
}
