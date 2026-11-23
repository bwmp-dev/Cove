package control

import (
	"context"
	"fmt"

	"cove/internal/worker"
)

func (s *Server) pruneOperationHistory(ctx context.Context, days, records int) error {
	rows, err := s.DB.Query(ctx, `SELECT n.id,COALESCE((n.capabilities->>'journal_retention')::boolean,false) FROM nodes n LEFT JOIN operation_retention_runs r ON r.node_id=n.id WHERE n.state='ready' AND (r.checked_at IS NULL OR r.checked_at<now()-interval '5 minutes') ORDER BY r.checked_at NULLS FIRST LIMIT 8`)
	if err != nil {
		return err
	}
	type node struct {
		id        string
		supported bool
	}
	var nodes []node
	for rows.Next() {
		var n node
		if err = rows.Scan(&n.id, &n.supported); err != nil {
			break
		}
		nodes = append(nodes, n)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, n := range nodes {
		if !n.supported {
			if _, err = s.DB.Exec(ctx, `INSERT INTO operation_retention_runs(node_id,checked_at,error) VALUES($1,now(),'Upgrade and verify the worker to prune operation journals.') ON CONFLICT(node_id) DO UPDATE SET checked_at=now(),error=EXCLUDED.error`, n.id); err != nil {
				return err
			}
			continue
		}
		if err = s.pruneNodeOperationHistory(ctx, n.id, days, records); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) pruneNodeOperationHistory(ctx context.Context, node string, days, records int) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext('cove-operation-retention'),hashtext($1))`, node).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	rows, err := tx.Query(ctx, `WITH ranked AS (SELECT id,row_number() OVER(ORDER BY updated_at DESC,id DESC) AS rank FROM operations) SELECT o.id,o.workload_id FROM operations o JOIN workloads w ON w.id=o.workload_id JOIN ranked r ON r.id=o.id WHERE w.node_id=$1 AND o.state IN ('succeeded','failed','interrupted','cancelled') AND (o.updated_at<now()-make_interval(days=>$2) OR r.rank>$3) AND NOT EXISTS(SELECT 1 FROM operation_dependencies d WHERE d.prerequisite_id=o.id) AND NOT EXISTS(SELECT 1 FROM operations ref WHERE ref.id<>o.id AND ref.workload_id=o.workload_id AND ref.result->'rollout'->>'release'=o.id) AND NOT EXISTS(SELECT 1 FROM configuration_drafts d WHERE d.operation_ids ? o.id) AND NOT EXISTS(SELECT 1 FROM compose_adoptions a WHERE a.status='applying' AND a.resource_ids ? o.workload_id) AND (o.action NOT IN ('deploy','rollback','rollout_promote','rollout_abort') OR o.updated_at<now()-make_interval(days=>COALESCE((SELECT retention_days FROM audit_export_settings WHERE id=1),365))) AND NOT EXISTS(SELECT 1 FROM notification_deliveries d WHERE d.operation_id=o.id AND d.state='pending') AND NOT EXISTS(SELECT 1 FROM webhook_deliveries d WHERE d.operation_id=o.id) ORDER BY o.updated_at,o.id LIMIT 500 FOR UPDATE OF o SKIP LOCKED`, node, days, records)
	if err != nil {
		return err
	}
	in := worker.JournalPruneRequest{Candidates: []worker.JournalCandidate{}}
	for rows.Next() {
		var c worker.JournalCandidate
		if err = rows.Scan(&c.ID, &c.Workload); err != nil {
			break
		}
		in.Candidates = append(in.Candidates, c)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	pruned, protected, message := 0, 0, ""
	if len(in.Candidates) > 0 {
		response, rpcErr := s.nodeRPC(ctx, node, worker.RPCRequest{Method: "prune_journals", JournalPrune: &in})
		if rpcErr != nil || response.Error != "" || response.JournalPrune == nil {
			message = "Journal retention deferred; worker busy, recovering or unavailable."
		} else {
			allowed := map[string]bool{}
			for _, c := range in.Candidates {
				allowed[c.ID] = true
			}
			for _, id := range response.JournalPrune.Pruned {
				if !allowed[id] {
					return fmt.Errorf("unexpected journal pruning acknowledgement")
				}
				delete(allowed, id)
			}
			protected = response.JournalPrune.Protected
			if protected < 0 || protected > len(in.Candidates) {
				return fmt.Errorf("invalid journal protection count")
			}
			result, err := tx.Exec(ctx, `DELETE FROM operations o WHERE id=ANY($1) AND state IN ('succeeded','failed','interrupted','cancelled') AND NOT EXISTS(SELECT 1 FROM operation_dependencies d WHERE d.prerequisite_id=o.id) AND NOT EXISTS(SELECT 1 FROM operations ref WHERE ref.id<>o.id AND ref.workload_id=o.workload_id AND ref.result->'rollout'->>'release'=o.id) AND NOT EXISTS(SELECT 1 FROM configuration_drafts d WHERE d.operation_ids ? o.id) AND NOT EXISTS(SELECT 1 FROM compose_adoptions a WHERE a.status='applying' AND a.resource_ids ? o.workload_id) AND (o.action NOT IN ('deploy','rollback','rollout_promote','rollout_abort') OR o.updated_at<now()-make_interval(days=>COALESCE((SELECT retention_days FROM audit_export_settings WHERE id=1),365)))`, response.JournalPrune.Pruned)
			if err != nil {
				return err
			}
			pruned = int(result.RowsAffected())
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO operation_retention_runs(node_id,checked_at,error,pruned,protected) VALUES($1,now(),$2,$3,$4) ON CONFLICT(node_id) DO UPDATE SET checked_at=now(),error=EXCLUDED.error,pruned=EXCLUDED.pruned,protected=EXCLUDED.protected`, node, message, pruned, protected)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
