package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var errTransportListenerClaim = errors.New("transport listener is already reserved")

func (s *Server) transportListenerClaims(ctx context.Context, tx pgx.Tx, workload, node string, routes []worker.Route) error {
	wanted := map[string]bool{}
	for _, route := range routes {
		if route.IsTransportRoute() {
			wanted[route.Policy.Listener] = true
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	for _, query := range []struct {
		sql     string
		pending bool
	}{
		{"SELECT configuration FROM workloads WHERE node_id=$1 AND id<>$2", false},
		{"SELECT o.payload FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE w.node_id=$1 AND w.id<>$2 AND o.state IN ('queued','running')", true},
	} {
		rows, err := tx.Query(ctx, query.sql, node, workload)
		if err != nil {
			return err
		}
		for rows.Next() {
			var encrypted []byte
			if err := rows.Scan(&encrypted); err != nil {
				rows.Close()
				return err
			}
			data, err := unseal(s.Key, encrypted)
			if err != nil {
				rows.Close()
				return err
			}
			var owned []worker.Route
			if query.pending {
				var request worker.Request
				if err := json.Unmarshal(data, &request); err != nil {
					rows.Close()
					return err
				}
				owned = request.Routes
			} else {
				var input workloadInput
				if err := json.Unmarshal(data, &input); err != nil {
					rows.Close()
					return err
				}
				owned = input.Routes
			}
			for _, route := range owned {
				if route.IsTransportRoute() && wanted[route.Policy.Listener] {
					rows.Close()
					return fmt.Errorf("%w: %s", errTransportListenerClaim, route.Policy.Listener)
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
