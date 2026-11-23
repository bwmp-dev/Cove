package control

import (
	"net/http"

	"cove/internal/worker"
)

func (s *Server) discardDatabaseCopy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Confirm bool   `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		http.Error(w, "confirm removal of the failed copy and its data; the source is retained", 400)
		return
	}
	var selectedTarget string
	if err := s.DB.QueryRow(r.Context(), `SELECT workload_id FROM database_maintenance WHERE id=$1 AND (workload_id=$2 OR source_id=$2)`, r.PathValue("task"), r.PathValue("id")).Scan(&selectedTarget); err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.requireAccess(w, r, "workload", selectedTarget, 3) {
		return
	}
	proxy, err := s.outboundConfig(r.Context())
	if err != nil {
		http.Error(w, "worker transport unavailable", 503)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var target, node, name, state string
	err = tx.QueryRow(r.Context(), `SELECT m.workload_id,m.node_id,w.name,m.state FROM database_maintenance m JOIN operations o ON o.id=m.id JOIN workloads w ON w.id=m.workload_id WHERE m.id=$1 AND m.kind='upgrade' AND (m.workload_id=$2 OR m.source_id=$2) AND m.state IN ('failed','discarded') AND o.state IN ('failed','interrupted','cancelled') FOR UPDATE OF m,w`, r.PathValue("task"), r.PathValue("id")).Scan(&target, &node, &name, &state)
	if err != nil {
		http.Error(w, "select an inactive failed copy", 409)
		return
	}
	if in.Name != name {
		http.Error(w, "type the failed copy's current name to confirm", 400)
		return
	}
	if state == "discarded" {
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	var busy bool
	if tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))`, target).Scan(&busy) != nil || busy {
		http.Error(w, "wait for active operations before discarding", 409)
		return
	}
	if _, err = s.nodeRPCUsing(r.Context(), tx, node, worker.RPCRequest{Method: "database_discard", ID: r.PathValue("task")}, proxy); err != nil {
		http.Error(w, "worker did not confirm failed copy cleanup; retry this action", 502)
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE database_maintenance SET state='discarded',last_error='',updated_at=now() WHERE id=$1`, r.PathValue("task")); err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE operations SET state='cancelled',updated_at=now() WHERE id=$1`, r.PathValue("task"))
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE workloads SET manually_stopped=true WHERE id=$1`, target)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'discard failed database copy',$2,200,$3)`, actor(r).ID, target, actor(r).Team)
	}
	if err != nil || tx.Commit(r.Context()) != nil {
		http.Error(w, "cleanup completed but recording needs retry", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
