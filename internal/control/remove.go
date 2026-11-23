package control

import (
	"cove/internal/worker"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

func (s *Server) deleteWorkload(w http.ResponseWriter, r *http.Request) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var node string
	err = tx.QueryRow(r.Context(), "SELECT node_id FROM workloads WHERE id=$1 FOR UPDATE", r.PathValue("id")).Scan(&node)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	var active bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM object_buckets WHERE workload_id=$1 OR storage_resource_id=$1)`, r.PathValue("id")).Scan(&active); err != nil {
		http.Error(w, "bucket dependencies unavailable", 503)
		return
	}
	if active {
		http.Error(w, "revoke this resource's object bucket keys before deleting it or its storage", 409)
		return
	}
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM database_access WHERE workload_id=$1 AND revoked_at IS NULL AND expires_at>now())`, r.PathValue("id")).Scan(&active); err != nil {
		http.Error(w, "temporary access unavailable", 503)
		return
	}
	if active {
		http.Error(w, "close temporary database access and admin tools before deleting", 409)
		return
	}
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM route_certificate_issuers WHERE workload_id=$1)`, r.PathValue("id")).Scan(&active); err != nil {
		http.Error(w, "certificate issuers unavailable", 503)
		return
	}
	if active {
		http.Error(w, "remove this resource's certificate issuers before deleting", 409)
		return
	}
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM previews WHERE source_id=$1)`, r.PathValue("id")).Scan(&active); err != nil {
		http.Error(w, "previews unavailable", 503)
		return
	}
	if active {
		http.Error(w, "close this resource's previews before deleting", 409)
		return
	}
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))", r.PathValue("id")).Scan(&active); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if active {
		http.Error(w, "wait for the active operation before deleting", 409)
		return
	}
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operation_dependencies d JOIN operations prerequisite ON prerequisite.id=d.prerequisite_id JOIN operations dependent ON dependent.id=d.operation_id WHERE prerequisite.workload_id=$1 AND dependent.state IN ('queued','running'))`, r.PathValue("id")).Scan(&active); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if active {
		http.Error(w, "wait for dependent deployments before deleting", 409)
		return
	}
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM resource_variable_bindings WHERE source_id=$1)`, r.PathValue("id")).Scan(&active); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if active {
		http.Error(w, "remove variable bindings that reference this resource before deleting", 409)
		return
	}
	if _, err = s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "remove_workload", ID: r.PathValue("id")}); err != nil {
		var remote *RemoteError
		if errors.As(err, &remote) {
			http.Error(w, "stop the workload and resolve worker errors before deleting", 409)
		} else {
			http.Error(w, "worker unavailable; workload was not deleted", 502)
		}
		return
	}
	for _, query := range []string{"DELETE FROM api_tokens WHERE workload_id=$1", "DELETE FROM webhook_deliveries WHERE workload_id=$1", "DELETE FROM operation_dependencies WHERE prerequisite_id IN (SELECT id FROM operations WHERE workload_id=$1)", "DELETE FROM operations WHERE workload_id=$1", "DELETE FROM workloads WHERE id=$1"} {
		if _, err = tx.Exec(r.Context(), query, r.PathValue("id")); err != nil {
			http.Error(w, "could not delete workload", 500)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not delete workload", 500)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var id string
	err = tx.QueryRow(r.Context(), "SELECT id FROM nodes WHERE id=$1 FOR UPDATE", r.PathValue("id")).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	var used bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM workloads WHERE node_id=$1) OR EXISTS(SELECT 1 FROM workload_moves m JOIN operations o ON o.id=m.operation_id WHERE (m.source_node=$1 OR m.target_node=$1) AND o.state IN ('queued','running'))", id).Scan(&used); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if used {
		http.Error(w, "finish node moves and move or delete this node's workloads first", 409)
		return
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM enrollments WHERE node_id=$1", id); err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM nodes WHERE id=$1", id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not delete node", 500)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
