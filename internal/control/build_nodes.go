package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

type buildNodeQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type buildNodeInput struct {
	Node string `json:"node_id"`
}

type buildNodeChoice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
}

func selectBuildNode(ctx context.Context, q buildNodeQuery, workload, selected string) (string, error) {
	var runtime, team string
	var runtimeProbe []byte
	if err := q.QueryRow(ctx, `SELECT w.node_id,w.team_id,COALESCE(n.capabilities,'{}') FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, workload).Scan(&runtime, &team, &runtimeProbe); err != nil {
		return "", errors.New("build node settings could not be read")
	}
	if selected == "" {
		selected = runtime
	}
	var state string
	var capabilities []byte
	var granted bool
	if err := q.QueryRow(ctx, `SELECT state,COALESCE(capabilities,'{}'),EXISTS(SELECT 1 FROM team_nodes WHERE node_id=nodes.id AND team_id=$2) FROM nodes WHERE id=$1`, selected, team).Scan(&state, &capabilities, &granted); err != nil {
		return "", errors.New("the selected build node is unavailable")
	}
	var target, source worker.Probe
	if json.Unmarshal(capabilities, &target) != nil || json.Unmarshal(runtimeProbe, &source) != nil {
		return "", errors.New("worker capabilities could not be read")
	}
	if state != "ready" || !target.BuildPublish {
		return "", errors.New("the build node must be ready and report build and publish support")
	}
	if selected != runtime {
		if !granted {
			return "", errors.New("the build node must be assigned to this team")
		}
		if !target.RemoteBuilds {
			return "", errors.New("upgrade and verify the build worker for designated builds and cleanup")
		}
		if target.OS != "linux" || source.OS != "linux" || target.Arch == "" || target.Arch != source.Arch {
			return "", errors.New("build and runtime nodes must report the same Linux architecture")
		}
	}
	return selected, nil
}

func buildExecutionNode(ctx context.Context, q buildNodeQuery, workload, operation string) (string, error) {
	var selected string
	if err := q.QueryRow(ctx, `SELECT COALESCE((SELECT execution_node_id FROM operations WHERE id=$2 AND workload_id=w.id AND action='build'),(SELECT node_id FROM workload_build_nodes WHERE workload_id=w.id),w.node_id) FROM workloads w WHERE w.id=$1`, workload, operation).Scan(&selected); err != nil {
		return "", errors.New("build execution node could not be read")
	}
	return selectBuildNode(ctx, q, workload, selected)
}

func (s *Server) saveBuildOutcome(ctx context.Context, id string, request worker.Request, response worker.RPCResponse) error {
	op := response.Operation
	valid := op != nil && op.ID == id && op.Workload == request.Workload && op.Action == "build"
	if valid {
		switch op.State {
		case "queued", "running", "succeeded", "failed", "cancelled", "interrupted":
		default:
			valid = false
		}
	}
	if !valid {
		return s.deferBuildResult(ctx, id)
	}
	result, err := json.Marshal(op)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `UPDATE operations SET state=$2,result=$3,updated_at=now() WHERE id=$1`, id, op.State, result)
	return err
}

func (s *Server) deferBuildResult(ctx context.Context, id string) error {
	_, err := s.DB.Exec(ctx, `UPDATE operations SET updated_at=now(),result=jsonb_build_object('error','Waiting for the original build worker to confirm the operation. Verify that worker or cancel; its inputs and execution node will not be changed.') WHERE id=$1`, id)
	return err
}

func (s *Server) cancelBuild(ctx context.Context, node string, request worker.Request) (worker.RPCResponse, error) {
	response, err := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "cancel_build", Request: &request})
	var remote *RemoteError
	if errors.As(err, &remote) {
		return s.nodeRPC(ctx, node, worker.RPCRequest{Method: "cancel", Request: &request})
	}
	return response, err
}

func (s *Server) buildNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == "GET" {
		var selected, runtime, team string
		err := s.DB.QueryRow(r.Context(), `SELECT COALESCE(b.node_id,''),w.node_id,w.team_id FROM workloads w LEFT JOIN workload_build_nodes b ON b.workload_id=w.id WHERE w.id=$1`, id).Scan(&selected, &runtime, &team)
		if err != nil {
			http.Error(w, "build node settings unavailable", 503)
			return
		}
		rows, err := s.DB.Query(r.Context(), `SELECT id,name FROM nodes WHERE id=$1 OR id=$2 OR EXISTS(SELECT 1 FROM team_nodes WHERE node_id=nodes.id AND team_id=$3) ORDER BY name,id`, runtime, selected, team)
		if err != nil {
			http.Error(w, "build nodes unavailable", 503)
			return
		}
		choices := []buildNodeChoice{}
		for rows.Next() {
			var c buildNodeChoice
			if err = rows.Scan(&c.ID, &c.Name); err != nil {
				break
			}
			choices = append(choices, c)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			http.Error(w, "build nodes unavailable", 503)
			return
		}
		for i := range choices {
			_, problem := selectBuildNode(r.Context(), s.DB, id, choices[i].ID)
			choices[i].Eligible = problem == nil
			if problem != nil {
				choices[i].Reason = problem.Error()
			}
		}
		effective := selected
		if effective == "" {
			effective = runtime
		}
		respond(w, 200, map[string]any{"node_id": selected, "runtime_node_id": runtime, "effective_node_id": effective, "nodes": choices})
		return
	}
	var in buildNodeInput
	if !decode(w, r, &in) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var runtime string
	if err = tx.QueryRow(r.Context(), `SELECT node_id FROM workloads WHERE id=$1 FOR UPDATE`, id).Scan(&runtime); err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	var active bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))`, id).Scan(&active); err != nil || active {
		http.Error(w, "wait for active operations before changing the build node", 409)
		return
	}
	if in.Node != "" && in.Node != runtime {
		if _, err = selectBuildNode(r.Context(), tx, id, in.Node); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO workload_build_nodes(workload_id,node_id) VALUES($1,$2) ON CONFLICT(workload_id) DO UPDATE SET node_id=EXCLUDED.node_id`, id, in.Node)
	} else {
		_, err = tx.Exec(r.Context(), `DELETE FROM workload_build_nodes WHERE workload_id=$1`, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save build node", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) retireRemoteBuilds(ctx context.Context, tx pgx.Tx, workload, runtime string) error {
	rows, err := tx.Query(ctx, `SELECT execution_node_id,payload FROM operations WHERE workload_id=$1 AND action='build' AND execution_node_id IS NOT NULL AND execution_node_id<>$2 ORDER BY created_at`, workload, runtime)
	if err != nil {
		return err
	}
	type build struct {
		node    string
		payload []byte
	}
	var builds []build
	for rows.Next() {
		var b build
		if err = rows.Scan(&b.node, &b.payload); err != nil {
			break
		}
		builds = append(builds, b)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, b := range builds {
		var request worker.Request
		if err = s.unsealJSON(b.payload, &request); err != nil {
			return err
		}
		if _, err = s.nodeRPC(ctx, b.node, worker.RPCRequest{Method: "retire_build", Request: &request}); err != nil {
			return err
		}
	}
	return nil
}
