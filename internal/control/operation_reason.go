package control

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
)

func validOperationReason(reason string) bool {
	return len(reason) <= 1000 && !strings.ContainsRune(reason, 0)
}

func saveOperationReason(ctx context.Context, tx pgx.Tx, id, reason, by string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, "UPDATE operations SET reason=$2 WHERE id=$1", id, reason); err != nil {
		return err
	}
	details, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status,team_id,details) SELECT $1,'deployment reason',o.id,200,w.team_id,$3 FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE o.id=$2`, by, id, details)
	return err
}
