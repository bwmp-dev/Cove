package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"github.com/jackc/pgx/v5"
)

func (s *Server) coalesceWebhooks(ctx context.Context, tx pgx.Tx, workload string) error {
	// Sequence deliveries retain their exact dependency graph. Only standalone
	// service pushes that have never become operations can be superseded.
	rows, err := tx.Query(ctx, `SELECT d.delivery,d.payload FROM webhook_deliveries d
 WHERE d.workload_id=$1 AND d.operation_id IS NULL AND d.superseded_by IS NULL
 AND d.prerequisite_workload IS NULL
 AND NOT EXISTS(SELECT 1 FROM webhook_deliveries child WHERE child.prerequisite_workload=d.workload_id AND child.prerequisite_delivery=d.delivery)
 ORDER BY d.created_at DESC,d.delivery DESC`, workload)
	if err != nil {
		return err
	}
	type push struct {
		delivery string
		request  worker.Request
	}
	var pushes []push
	for rows.Next() {
		var delivery string
		var encrypted []byte
		if err = rows.Scan(&delivery, &encrypted); err != nil {
			break
		}
		var raw []byte
		raw, err = unseal(s.Key, encrypted)
		if err != nil {
			break
		}
		var request worker.Request
		if err = json.Unmarshal(raw, &request); err != nil {
			break
		}
		pushes = append(pushes, push{delivery, request})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	newest := map[string]string{}
	for _, p := range pushes {
		if p.request.Kind == "job" || p.request.Source == nil {
			continue
		}
		key := p.request.Source.Repository + "\n" + p.request.Source.Branch
		replacement, ok := newest[key]
		if !ok {
			newest[key] = p.delivery
			continue
		}
		if _, err = tx.Exec(ctx, `UPDATE webhook_deliveries SET superseded_by=$3 WHERE workload_id=$1 AND delivery=$2`, workload, p.delivery, replacement); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status) VALUES('GitHub webhook','superseded queued push',$1,200)`, workload); err != nil {
			return err
		}
	}
	return nil
}
