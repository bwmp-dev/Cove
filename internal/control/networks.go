package control

import (
	"context"
	"cove/internal/worker"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *Server) sharedNetworks(w http.ResponseWriter, r *http.Request) {
	p := actor(r)
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), "SELECT id,name,node_id FROM shared_networks WHERE team_id=$1 ORDER BY name,id", p.Team)
		if err != nil {
			groupError(w, err)
			return
		}
		defer rows.Close()
		type network struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			NodeID string `json:"node_id"`
		}
		out := []network{}
		for rows.Next() {
			var n network
			if err = rows.Scan(&n.ID, &n.Name, &n.NodeID); err != nil {
				groupError(w, err)
				return
			}
			out = append(out, n)
		}
		if rows.Err() != nil {
			groupError(w, rows.Err())
			return
		}
		respond(w, 200, out)
		return
	}
	var in struct {
		Name   string `json:"name"`
		NodeID string `json:"node_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 120 {
		http.Error(w, "name must contain 1-120 characters", 400)
		return
	}
	var allowed bool
	if err := s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM nodes n WHERE n.id=$1 AND ($3 OR EXISTS(SELECT 1 FROM team_nodes WHERE node_id=n.id AND team_id=$2)))", in.NodeID, p.Team, p.Admin).Scan(&allowed); err != nil {
		groupError(w, err)
		return
	}
	if !allowed {
		http.Error(w, "node is not assigned to this team", 403)
		return
	}
	id := resourceID()
	if _, err := s.DB.Exec(r.Context(), "INSERT INTO shared_networks(id,name,node_id,team_id) VALUES($1,$2,$3,$4)", id, in.Name, in.NodeID, p.Team); err != nil {
		groupError(w, err)
		return
	}
	respond(w, 201, map[string]string{"id": id})
}

func (s *Server) workloadNetworks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	p := actor(r)
	if r.Method == "GET" {
		rows, err := s.DB.Query(ctx, "SELECT network_id,service,alias FROM workload_networks WHERE workload_id=$1 ORDER BY network_id,service", id)
		if err != nil {
			groupError(w, err)
			return
		}
		defer rows.Close()
		out := []worker.NetworkAttachment{}
		for rows.Next() {
			var n worker.NetworkAttachment
			if err = rows.Scan(&n.ID, &n.Service, &n.Alias); err != nil {
				groupError(w, err)
				return
			}
			out = append(out, n)
		}
		if rows.Err() != nil {
			groupError(w, rows.Err())
			return
		}
		respond(w, 200, out)
		return
	}
	var in struct {
		Networks []worker.NetworkAttachment `json:"networks"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := worker.ValidateNetworks(in.Networks); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	sort.Slice(in.Networks, func(i, j int) bool {
		if in.Networks[i].ID == in.Networks[j].ID {
			return in.Networks[i].Service < in.Networks[j].Service
		}
		return in.Networks[i].ID < in.Networks[j].ID
	})
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		groupError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var node string
	if err = tx.QueryRow(ctx, "SELECT node_id FROM workloads WHERE id=$1 AND team_id=$2 FOR UPDATE", id, p.Team).Scan(&node); err != nil {
		groupError(w, err)
		return
	}
	var active bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))", id).Scan(&active); err != nil {
		groupError(w, err)
		return
	}
	if active {
		http.Error(w, "wait for the active operation before changing networks", 409)
		return
	}
	if _, err = tx.Exec(ctx, "DELETE FROM workload_networks WHERE workload_id=$1", id); err != nil {
		groupError(w, err)
		return
	}
	for _, n := range in.Networks {
		var found string
		err = tx.QueryRow(ctx, "SELECT id FROM shared_networks WHERE id=$1 AND team_id=$2 AND node_id=$3 FOR UPDATE", n.ID, p.Team, node).Scan(&found)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "select a network in this team on the resource's node", 400)
			return
		}
		if err != nil {
			groupError(w, err)
			return
		}
		var collision bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM workload_networks WHERE network_id=$1 AND (alias=$2 OR service=$2 OR alias=$3))", n.ID, n.Alias, n.Service).Scan(&collision); err != nil {
			groupError(w, err)
			return
		}
		if collision {
			http.Error(w, "DNS alias conflicts with another connection or Compose service on this network", 409)
			return
		}
		if _, err = tx.Exec(ctx, "INSERT INTO workload_networks(workload_id,network_id,service,alias) VALUES($1,$2,$3,$4)", id, n.ID, n.Service, n.Alias); err != nil {
			groupError(w, err)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		groupError(w, err)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

var errNetworkWorkerUnsupported = errors.New("upgrade and verify this worker before deploying shared networks")

func loadNetworkRequest(ctx context.Context, tx pgx.Tx, request *worker.Request) error {
	rows, err := tx.Query(ctx, "SELECT network_id,service,alias FROM workload_networks WHERE workload_id=$1 ORDER BY network_id,service", request.Workload)
	if err != nil {
		return err
	}
	request.Networks = nil
	for rows.Next() {
		var n worker.NetworkAttachment
		if err = rows.Scan(&n.ID, &n.Service, &n.Alias); err != nil {
			break
		}
		request.Networks = append(request.Networks, n)
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return err
	}
	if len(request.Networks) == 0 {
		return nil
	}
	var supported bool
	if err = tx.QueryRow(ctx, "SELECT COALESCE((n.capabilities->>'shared_networks')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1", request.Workload).Scan(&supported); err != nil {
		return err
	}
	if !supported {
		return errNetworkWorkerUnsupported
	}
	return nil
}
