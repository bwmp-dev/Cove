package control

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (s *Server) registryWebhookDispatch(ctx context.Context, id string, begin bool) (bool, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var started, cancelled, allowed bool
	err = tx.QueryRow(ctx, `SELECT j.dispatch_started,o.cancel_requested,EXISTS(SELECT 1 FROM registry_webhooks h WHERE h.workload_id=w.id AND h.enabled AND h.creator_id=j.creator_id) AND cove_automation_allowed(j.creator_id,w.team_id,w.project_id,COALESCE(w.environment_id,''),'operate') AND cove_automation_allowed(j.creator_id,w.team_id,w.project_id,COALESCE(w.environment_id,''),'edit_configuration') FROM registry_webhook_runs j JOIN operations o ON o.id=j.operation_id JOIN workloads w ON w.id=o.workload_id WHERE o.id=$1 FOR UPDATE OF j,o`, id).Scan(&started, &cancelled, &allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	proceed := true
	if !allowed || cancelled {
		if !started {
			_, err = tx.Exec(ctx, `UPDATE operations SET state='cancelled',cancel_requested=true,result='{"error":"Registry deployment cancelled; its webhook or owner is no longer authorised."}',updated_at=now() WHERE id=$1`, id)
			proceed = false
		} else {
			_, err = tx.Exec(ctx, `UPDATE operations SET cancel_requested=true,updated_at=now() WHERE id=$1`, id)
			proceed = !begin
		}
	} else if begin {
		// Persist uncertainty before the first worker call; a lost reply must still be cancelled remotely.
		_, err = tx.Exec(ctx, `UPDATE registry_webhook_runs SET dispatch_started=true WHERE operation_id=$1`, id)
	}
	if err != nil {
		return false, err
	}
	return proceed, tx.Commit(ctx)
}
