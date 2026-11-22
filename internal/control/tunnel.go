package control

import (
	"cove/internal/worker"
	"net/http"
	"time"
)

func (s *Server) databaseAccess(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var node, address, hostKey, mode string
	var port int
	var capable bool
	if err := s.DB.QueryRow(r.Context(), `SELECT n.id,n.address,n.port,COALESCE(n.host_key,''),n.mode,COALESCE((n.capabilities->>'database_access')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, id).Scan(&node, &address, &port, &hostKey, &mode, &capable); err != nil {
		http.Error(w, "node unavailable", 503)
		return
	}
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), `SELECT id,service,port,expires_at FROM database_access WHERE workload_id=$1 AND expires_at>now() AND revoked_at IS NULL ORDER BY expires_at`, id)
		if err != nil {
			http.Error(w, "access unavailable", 503)
			return
		}
		defer rows.Close()
		leases := []map[string]any{}
		for rows.Next() {
			var lease, service string
			var targetPort int
			var expires time.Time
			if err = rows.Scan(&lease, &service, &targetPort, &expires); err != nil {
				http.Error(w, "access unavailable", 503)
				return
			}
			leases = append(leases, map[string]any{"id": lease, "service": service, "port": targetPort, "expires_at": expires})
		}
		if rows.Err() != nil {
			http.Error(w, "access unavailable", 503)
			return
		}
		respond(w, 200, leases)
		return
	}
	if r.Method == "DELETE" {
		var in struct {
			ID string `json:"id"`
		}
		if !decode(w, r, &in) {
			return
		}
		var exists bool
		if err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM database_access WHERE id=$1 AND workload_id=$2 AND revoked_at IS NULL)`, in.ID, id).Scan(&exists); err != nil {
			http.Error(w, "access unavailable", 503)
			return
		}
		if !exists {
			http.NotFound(w, r)
			return
		}
		if _, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "tunnel_close", ID: in.ID}); err != nil {
			http.Error(w, "worker could not confirm revocation; access still expires at its original deadline", 502)
			return
		}
		if _, err := s.DB.Exec(r.Context(), `UPDATE database_access SET revoked_at=now() WHERE id=$1`, in.ID); err != nil {
			http.Error(w, "access closed but record update failed", 503)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	if !capable || mode == "local" {
		http.Error(w, "temporary access requires an SSH-enrolled worker with database access support", 409)
		return
	}
	var in struct {
		Service   string `json:"service"`
		Port      int    `json:"port"`
		PublicKey string `json:"public_key"`
	}
	if !decode(w, r, &in) {
		return
	}
	lease := worker.TunnelRequest{ID: resourceID(), Workload: id, Service: in.Service, Port: in.Port, PublicKey: in.PublicKey, Expires: time.Now().UTC().Add(15 * time.Minute)}
	if err := worker.ValidateTunnel(lease); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// Record the lease before opening it so an ambiguous SSH reply remains visible and revocable.
	if _, err := s.DB.Exec(r.Context(), `INSERT INTO database_access(id,workload_id,node_id,user_id,service,port,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, lease.ID, id, node, actor(r).ID, in.Service, in.Port, lease.Expires); err != nil {
		http.Error(w, "could not record access", 503)
		return
	}
	if _, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: "tunnel_open", Tunnel: &lease}); err != nil {
		http.Error(w, "worker did not confirm access; inspect the access list to revoke this attempt", 502)
		return
	}
	respond(w, 201, map[string]any{"id": lease.ID, "expires_at": lease.Expires, "host": address, "ssh_port": port, "host_key": hostKey})
}
