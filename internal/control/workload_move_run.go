package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
)

func (s *Server) movePhase(ctx context.Context, id, phase, message string) error {
	_, err := s.DB.Exec(ctx, `UPDATE workload_moves SET phase=$2,last_error=$3,updated_at=now() WHERE operation_id=$1`, id, phase, message)
	return err
}

func (s *Server) transferMove(ctx context.Context, id, workload, source, target string, daemons ...string) error {
	sourceDaemon, targetDaemon := "", ""
	if len(daemons) == 2 {
		sourceDaemon, targetDaemon = daemons[0], daemons[1]
	}
	state, err := s.moveRPC(ctx, target, id, workload, "status", targetDaemon)
	if err != nil {
		return err
	}
	if state.Phase == "imported" || state.Phase == "starting" || state.Phase == "started" || state.Phase == "committed" {
		return nil
	}
	request := worker.MoveRequest{ID: id, Workload: workload, Daemon: sourceDaemon}
	return s.nodeStream(ctx, source, worker.RPCRequest{Method: "stream", Stream: &worker.StreamRequest{Action: "move_download", Move: &request}}, nil, func(meta worker.StreamResponse, body io.Reader) error {
		if meta.Error != "" || meta.Size < 1 || meta.Size > 64<<30 || len(meta.SHA256) != 64 {
			return errors.New("invalid source move stream")
		}
		targetRequest := request
		targetRequest.Daemon = targetDaemon
		return s.nodeStream(ctx, target, worker.RPCRequest{Method: "stream", Stream: &worker.StreamRequest{Action: "move_upload", Move: &targetRequest, Size: meta.Size, SHA256: meta.SHA256}}, body, func(result worker.StreamResponse, _ io.Reader) error {
			if result.Error != "" {
				return fmt.Errorf("target move import: %s", result.Error)
			}
			if result.BackupID != id || result.SHA256 != meta.SHA256 || result.Size != meta.Size {
				return errors.New("target did not confirm the imported move")
			}
			return nil
		})
	})
}

func (s *Server) finishMove(ctx context.Context, id, phase, state, message string) error {
	result, err := json.Marshal(worker.Operation{ID: id, Action: "move", State: state, Error: message})
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE workload_moves SET phase=$2,last_error=$3,updated_at=now() WHERE operation_id=$1`, id, phase, message); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE operations SET state=$2,result=$3,updated_at=now() WHERE id=$1`, id, state, result); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) processWorkloadMove(ctx context.Context, id string, intent workloadMoveIntent) error {
	var workload, source, target, phase string
	var cancelled bool
	err := s.DB.QueryRow(ctx, `SELECT o.workload_id,m.source_node,m.target_node,m.phase,o.cancel_requested FROM workload_moves m JOIN operations o ON o.id=m.operation_id WHERE m.operation_id=$1`, id).Scan(&workload, &source, &target, &phase, &cancelled)
	if err != nil {
		return err
	}
	if phase == "succeeded" || phase == "rolled_back" {
		return nil
	}
	if phase != "queued" {
		if _, err = s.DB.Exec(ctx, `UPDATE operations SET state='running',updated_at=now() WHERE id=$1 AND state IN ('queued','running')`, id); err != nil {
			return err
		}
	}
	if cancelled && phase != "committing" && phase != "cleaning" && phase != "rolling_back" && phase != "discarding" {
		return s.movePhase(ctx, id, "rolling_back", "Cancellation is waiting for the destination fence before the source can restart.")
	}
	fail := func(err error) error {
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("node move requires rollback", "operation", id, "phase", phase, "reason", err)
		return s.movePhase(ctx, id, "rolling_back", "Move could not be confirmed. Both nodes are retained; the source restarts only after the destination is confirmed stopped.")
	}
	rpc := func(node, action string) (*worker.MoveState, error) {
		daemon := intent.SourceDaemon
		if node == target {
			daemon = intent.TargetDaemon
		}
		return s.moveRPC(ctx, node, id, workload, action, daemon)
	}
	switch phase {
	case "queued":
		admitted, err := s.admitDeployment(ctx, id)
		if err != nil || !admitted {
			return err
		}
		return s.movePhase(ctx, id, "freezing", "")
	case "freezing":
		if _, err = rpc(source, "freeze"); err != nil {
			return fail(err)
		}
		return s.movePhase(ctx, id, "exporting", "")
	case "exporting":
		if _, err = rpc(source, "export"); err != nil {
			return fail(err)
		}
		return s.movePhase(ctx, id, "transferring", "")
	case "transferring":
		if err = s.transferMove(ctx, id, workload, source, target, intent.SourceDaemon, intent.TargetDaemon); err != nil {
			return fail(err)
		}
		return s.movePhase(ctx, id, "starting", "")
	case "starting":
		if _, err = rpc(target, "start"); err != nil {
			return fail(err)
		}
		return s.movePhase(ctx, id, "committing", "")
	case "committing":
		var placement string
		if err = s.DB.QueryRow(ctx, `SELECT node_id FROM workloads WHERE id=$1`, workload).Scan(&placement); err != nil {
			return err
		}
		if placement == target {
			return s.movePhase(ctx, id, "cleaning", "")
		}
		if placement != source {
			return errors.New("move placement changed outside its operation")
		}
		if _, err = rpc(target, "start"); err != nil {
			return fail(err)
		}
		original, err := unseal(s.Key, intent.Configuration)
		if err != nil {
			return err
		}
		var configuration workloadInput
		if json.Unmarshal(original, &configuration) != nil {
			return errors.New("invalid protected move configuration")
		}
		configuration.NodeID = target
		data, err := json.Marshal(configuration)
		if err != nil {
			return err
		}
		encrypted, err := seal(s.Key, data)
		if err != nil {
			return err
		}
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		tag, err := tx.Exec(ctx, `UPDATE workloads SET node_id=$2,configuration=$3 WHERE id=$1 AND node_id=$4 AND configuration=$5`, workload, target, encrypted, source, intent.Configuration)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			_ = tx.Rollback(ctx)
			return s.movePhase(ctx, id, "rolling_back", "Resource configuration changed; restoring the source without overwriting the newer settings.")
		}
		if _, err = tx.Exec(ctx, `UPDATE workload_moves SET phase='cleaning',last_error='',updated_at=now() WHERE operation_id=$1`, id); err != nil {
			return err
		}
		return tx.Commit(ctx)
	case "cleaning":
		if _, err = rpc(target, "commit"); err != nil {
			return s.movePhase(ctx, id, "cleaning", "Placement is committed; waiting for destination confirmation before removing source data.")
		}
		if _, err = rpc(source, "depart"); err != nil {
			return s.movePhase(ctx, id, "cleaning", "Destination is committed. Source cleanup is incomplete; retain both nodes and retry after connectivity or storage ownership is repaired.")
		}
		return s.finishMove(ctx, id, "succeeded", "succeeded", "")
	case "rolling_back":
		state, err := rpc(target, "status")
		if err != nil {
			return nil
		}
		if state.Phase != "absent" && state.Phase != "discarded" && state.Phase != "fenced" {
			if _, err = rpc(target, "fence"); err != nil {
				return nil
			}
		}
		state, err = rpc(source, "status")
		if err != nil {
			return nil
		}
		if state.Phase != "absent" && state.Phase != "rolled_back" {
			if _, err = rpc(source, "rollback"); err != nil {
				return nil
			}
		}
		return s.movePhase(ctx, id, "discarding", "Source restored. Removing only the fenced destination copy.")
	case "discarding":
		state, err := rpc(target, "status")
		if err != nil {
			return nil
		}
		if state.Phase != "absent" && state.Phase != "discarded" {
			if _, err = rpc(target, "discard"); err != nil {
				return nil
			}
		}
		stateName := "failed"
		if cancelled {
			stateName = "cancelled"
		}
		return s.finishMove(ctx, id, "rolled_back", stateName, "Move did not commit; the original running containers and restart policies were restored on the source.")
	default:
		return errors.New("unsupported persisted move phase")
	}
}
