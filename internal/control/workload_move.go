package control

import (
	"context"
	"cove/internal/worker"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type workloadMovePreview struct {
	SourceID         string           `json:"source_node_id"`
	TargetID         string           `json:"destination_node_id"`
	Source           *worker.MovePlan `json:"source"`
	Target           *worker.MovePlan `json:"target"`
	Revision         string           `json:"revision"`
	ListenerRevision string           `json:"listener_revision,omitempty"`
}

type workloadMoveIntent struct {
	SourceDaemon  string `json:"source_daemon"`
	TargetDaemon  string `json:"target_daemon"`
	Action        string `json:"action"`
	Configuration []byte `json:"configuration"`
}

func (s *Server) moveRPC(ctx context.Context, node, id, workload, action string, daemon ...string) (*worker.MoveState, error) {
	request := worker.MoveRequest{ID: id, Workload: workload, Action: action}
	if len(daemon) > 0 {
		request.Daemon = daemon[0]
	}
	response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "move", Move: &request})
	if err != nil {
		return nil, err
	}
	if response.Error != "" {
		return nil, &RemoteError{Message: response.Error}
	}
	if response.Move == nil || response.Move.ID != id || response.Move.Workload != workload {
		return nil, errors.New("invalid node move response")
	}
	return response.Move, nil
}

func (s *Server) planWorkloadMove(ctx context.Context, workload, target string) (workloadMovePreview, []byte, error) {
	preview := workloadMovePreview{TargetID: target}
	var configuration []byte
	var allowed bool
	err := s.DB.QueryRow(ctx, `SELECT w.node_id,w.configuration,w.kind='service' AND NOT EXISTS(SELECT 1 FROM environments e WHERE e.id=w.environment_id AND e.paused) AND NOT EXISTS(SELECT 1 FROM environment_suspensions e WHERE e.workload_id=w.id) AND NOT COALESCE((w.idle_policy->>'enabled')::boolean,false) AND src.state='ready' AND dst.state='ready' AND COALESCE((src.capabilities->>'workload_moves')::boolean,false) AND COALESCE((dst.capabilities->>'workload_moves')::boolean,false) AND EXISTS(SELECT 1 FROM team_nodes tn WHERE tn.team_id=w.team_id AND tn.node_id=dst.id) AND NOT EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id) AND NOT EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id) AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) AND NOT EXISTS(SELECT 1 FROM node_identity_rotations i WHERE i.node_id IN (src.id,dst.id) AND i.phase NOT IN ('succeeded','cancelled')) FROM workloads w JOIN nodes src ON src.id=w.node_id JOIN nodes dst ON dst.id=$2 WHERE w.id=$1`, workload, target).Scan(&preview.SourceID, &configuration, &allowed)
	if err != nil {
		return preview, nil, err
	}
	if !allowed || preview.SourceID == target {
		return preview, nil, errors.New("choose a different assigned ready node with move support, and settle source operations or previews first")
	}
	var config workloadInput
	plain, err := unseal(s.Key, configuration)
	if err != nil || json.Unmarshal(plain, &config) != nil {
		return preview, nil, errors.New("move configuration is unavailable")
	}
	if err := s.checkMoveTunnelReservations(ctx, s.DB, preview.SourceID, target, config.Routes); err != nil {
		return preview, nil, err
	}
	preview.ListenerRevision, err = s.moveRouteDestination(ctx, s.DB, target, config.Routes)
	if err != nil {
		return preview, nil, err
	}
	id := resourceID()
	source, err := s.moveRPC(ctx, preview.SourceID, id, workload, "plan")
	if err != nil {
		return preview, nil, err
	}
	destination, err := s.moveRPC(ctx, target, id, workload, "node")
	if err != nil {
		return preview, nil, err
	}
	preview.Source, preview.Target = source.Plan, destination.Plan
	if source.Plan == nil || destination.Plan == nil || source.Plan.Root != destination.Plan.Root || source.Plan.Architecture != destination.Plan.Architecture || source.Plan.Daemon == destination.Plan.Daemon {
		return preview, nil, errors.New("move requires independent Docker daemons with the same worker state path and architecture")
	}
	data, err := json.Marshal(struct {
		Preview       workloadMovePreview
		Configuration []byte
	}{preview, configuration})
	if err != nil {
		return preview, nil, err
	}
	digest := sha256.Sum256(data)
	preview.Revision = hex.EncodeToString(digest[:])
	return preview, configuration, nil
}

func (s *Server) workloadMove(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("id")
	if r.Method == "GET" {
		var operation, source, target, phase, message string
		var updated time.Time
		err := s.DB.QueryRow(r.Context(), `SELECT m.operation_id,COALESCE(m.source_node,''),COALESCE(m.target_node,''),m.phase,m.last_error,m.updated_at FROM workload_moves m JOIN operations o ON o.id=m.operation_id WHERE o.workload_id=$1 ORDER BY o.created_at DESC LIMIT 1`, id).Scan(&operation, &source, &target, &phase, &message, &updated)
		if errors.Is(err, pgx.ErrNoRows) {
			respond(w, 200, map[string]any{"move": nil})
			return
		}
		if err != nil {
			http.Error(w, "move status unavailable", 503)
			return
		}
		respond(w, 200, map[string]any{"move": map[string]any{"id": operation, "source_node_id": source, "destination_node_id": target, "phase": phase, "last_error": message, "updated_at": updated}})
		return
	}
	var in struct {
		Target   string `json:"destination_node_id"`
		Revision string `json:"revision"`
		Confirm  bool   `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	preview, configuration, err := s.planWorkloadMove(r.Context(), id, in.Target)
	if err != nil {
		var remote *RemoteError
		if errors.As(err, &remote) {
			http.Error(w, remote.Message, 409)
		} else {
			http.Error(w, "move preview unavailable; verify both nodes, supported storage and source state", 409)
		}
		return
	}
	if strings.HasSuffix(r.URL.Path, "/preview") {
		respond(w, 200, preview)
		return
	}
	if !in.Confirm || in.Revision != preview.Revision {
		http.Error(w, "review the current move preview and confirm downtime before moving", 409)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(1129272945)`); err != nil {
		http.Error(w, "move queue unavailable", 503)
		return
	}
	if _, err = tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock($1)", routeClaimLock); err != nil {
		http.Error(w, "route reservations unavailable", 503)
		return
	}
	var config workloadInput
	plain, err := unseal(s.Key, configuration)
	if err != nil || json.Unmarshal(plain, &config) != nil {
		http.Error(w, "move configuration unavailable", 503)
		return
	}
	if err := s.checkMoveTunnelReservations(r.Context(), tx, preview.SourceID, preview.TargetID, config.Routes); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	listenerRevision, err := s.moveRouteDestination(r.Context(), tx, preview.TargetID, config.Routes)
	if err != nil || listenerRevision != preview.ListenerRevision {
		http.Error(w, "destination route listeners changed; review a fresh move plan", 409)
		return
	}
	if err = s.transportListenerClaims(r.Context(), tx, id, preview.TargetID, config.Routes); err != nil {
		http.Error(w, "destination transport listener is reserved by another resource or move", 409)
		return
	}
	var count int
	if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM workload_moves m JOIN operations o ON o.id=m.operation_id WHERE o.state IN ('queued','running')`).Scan(&count); err != nil {
		http.Error(w, "move queue unavailable", 503)
		return
	}
	if count >= 2 {
		http.Error(w, "at most two node moves may be unresolved", 409)
		return
	}
	var matches bool
	if err = tx.QueryRow(r.Context(), `SELECT configuration=$2 AND node_id=$3 FROM workloads WHERE id=$1 FOR UPDATE`, id, configuration, preview.SourceID).Scan(&matches); err != nil || !matches {
		http.Error(w, "resource changed; review a new preview", 409)
		return
	}
	operation := resourceID()
	payload, err := json.Marshal(workloadMoveIntent{Action: "move", Configuration: configuration, SourceDaemon: preview.Source.Daemon, TargetDaemon: preview.Target.Daemon})
	if err != nil {
		http.Error(w, "move request unavailable", 503)
		return
	}
	protected, err := seal(s.Key, payload)
	if err != nil {
		http.Error(w, "move configuration could not be protected", 503)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO operations(id,workload_id,action,payload,reason) VALUES($1,$2,'move',$3,'Move resource between nodes')`, operation, id, protected); err != nil {
		http.Error(w, "resource has unfinished work; refresh before retrying", 409)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO workload_moves(operation_id,source_node,target_node) VALUES($1,$2,$3)`, operation, preview.SourceID, preview.TargetID); err != nil {
		http.Error(w, "move could not be saved", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "move submission could not be confirmed; refresh operations before retrying", 503)
		return
	}
	respond(w, 202, map[string]string{"id": operation})
}

func (s *Server) checkMoveTunnelReservations(ctx context.Context, db interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, source, target string, routes []worker.Route) error {
	hosts := map[string]bool{}
	for _, route := range routes {
		if route.Domain != "" {
			hosts[route.Domain] = true
		}
	}
	rows, err := db.Query(ctx, `SELECT hostnames,state FROM node_cloudflare_tunnels WHERE node_id IN ($1,$2)`, source, target)
	if err != nil {
		return errors.New("tunnel publication reservations unavailable")
	}
	defer rows.Close()
	for rows.Next() {
		var selected []string
		var encrypted []byte
		if err := rows.Scan(&selected, &encrypted); err != nil {
			return err
		}
		plain, err := unseal(s.Key, encrypted)
		var state nodeTunnelPrivate
		if err != nil || json.Unmarshal(plain, &state) != nil {
			return errors.New("tunnel DNS cleanup ownership is unavailable")
		}
		for host := range state.Records {
			selected = append(selected, host)
		}
		for _, host := range selected {
			if hosts[host] {
				return errors.New("remove this resource's hostnames from Tunnel publishing and finish DNS cleanup before moving between nodes; configure publishing on the destination afterwards")
			}
		}
	}
	return rows.Err()
}

func (s *Server) moveRouteDestination(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, node string, routes []worker.Route) (string, error) {
	managed := false
	for _, route := range routes {
		if route.IsTransportRoute() || route.Policy != nil && (route.Policy.Visibility == "lan" || route.Policy.Visibility == "tailscale") {
			managed = true
		}
	}
	if !managed {
		return "", nil
	}
	var protocol int
	if err := db.QueryRow(ctx, "SELECT COALESCE((capabilities->>'route_protocol')::integer,0) FROM nodes WHERE id=$1", node).Scan(&protocol); err != nil || protocol < worker.RouteProtocol(routes) {
		return "", errors.New("upgrade and verify the destination worker before moving listener routes")
	}
	proxy, err := s.outboundConfig(ctx)
	if err != nil {
		return "", err
	}
	response, err := s.nodeRPCUsing(ctx, db, node, worker.RPCRequest{Method: "ingress_exposures"}, proxy)
	if err != nil || response.Exposures == nil {
		return "", errors.New("destination route listeners are unavailable; verify the node before moving")
	}
	if err := worker.ValidateRouteListeners(routes, response.Exposures.Settings); err != nil {
		return "", err
	}
	data, err := json.Marshal(response.Exposures.Settings)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
