package control

import (
	"cove/internal/worker"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

func (s *Server) deleteManagedRegistryRecord(w http.ResponseWriter, r *http.Request) {
	if !s.teamAdmin(r, actor(r).Team) {
		http.Error(w, "team administrator required", 403)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	id := r.PathValue("id")
	var node, state string
	var retained bool
	err = tx.QueryRow(r.Context(), `SELECT w.node_id,m.state,m.data_retained FROM managed_registries m JOIN workloads w ON w.id=m.id WHERE m.id=$1 AND m.team_id=$2 FOR UPDATE OF m,w`, id, actor(r).Team).Scan(&node, &state, &retained)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "registry record unavailable", 503)
		return
	}
	if state != "removed" || retained {
		http.Error(w, "remove the registry and purge its retained data before deleting the record", 409)
		return
	}
	var busy bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running')) OR EXISTS(SELECT 1 FROM operation_dependencies d JOIN operations prerequisite ON prerequisite.id=d.prerequisite_id JOIN operations dependent ON dependent.id=d.operation_id WHERE prerequisite.workload_id=$1 AND dependent.state IN ('queued','running'))`, id).Scan(&busy)
	if err != nil || busy {
		http.Error(w, "wait for registry operations and dependent deployments before deleting its record", 409)
		return
	}
	removed, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "remove_workload", ID: id})
	if err != nil {
		http.Error(w, "worker cleanup must finish before deleting the registry record", 409)
		return
	}
	if _, err = tx.Exec(r.Context(), `DELETE FROM managed_registries WHERE id=$1`, id); err == nil {
		err = deleteWorkloadRecords(r.Context(), tx, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not delete registry record", 503)
		return
	}
	respond(w, 200, map[string]any{"ok": true, "cleanup": removed.Removal})
}
