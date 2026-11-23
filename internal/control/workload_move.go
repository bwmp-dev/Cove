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
	SourceID string           `json:"source_node_id"`
	TargetID string           `json:"destination_node_id"`
	Source   *worker.MovePlan `json:"source"`
	Target   *worker.MovePlan `json:"target"`
	Revision string           `json:"revision"`
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
	err := s.DB.QueryRow(ctx, `SELECT w.node_id,w.configuration,w.kind='service' AND src.state='ready' AND dst.state='ready' AND COALESCE((src.capabilities->>'workload_moves')::boolean,false) AND COALESCE((dst.capabilities->>'workload_moves')::boolean,false) AND EXISTS(SELECT 1 FROM team_nodes tn WHERE tn.team_id=w.team_id AND tn.node_id=dst.id) AND NOT EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id) AND NOT EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id) AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.workload_id=w.id AND o.state IN ('queued','running')) AND NOT EXISTS(SELECT 1 FROM node_identity_rotations i WHERE i.node_id IN (src.id,dst.id) AND i.phase NOT IN ('succeeded','cancelled')) FROM workloads w JOIN nodes src ON src.id=w.node_id JOIN nodes dst ON dst.id=$2 WHERE w.id=$1`, workload, target).Scan(&preview.SourceID, &configuration, &allowed)
	if err != nil {
		return preview, nil, err
	}
	if !allowed || preview.SourceID == target {
		return preview, nil, errors.New("choose a different assigned ready node with move support, and settle source operations or previews first")
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
	if _, err = tx.Exec(r.Context(), `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'move',$3)`, operation, id, protected); err != nil {
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
