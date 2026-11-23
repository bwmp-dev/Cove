package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"sync"
)

func (s *Server) refreshNodes(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, "SELECT id,address,port FROM nodes WHERE (host_key IS NOT NULL OR mode='local') AND (last_checked_at IS NULL OR last_checked_at<now()-interval '1 minute') ORDER BY last_checked_at NULLS FIRST LIMIT 8")
	if err != nil {
		return err
	}
	type node struct {
		id, address string
		port        int
	}
	var nodes []node
	for rows.Next() {
		var n node
		if err = rows.Scan(&n.id, &n.address, &n.port); err != nil {
			rows.Close()
			return err
		}
		nodes = append(nodes, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	failures := make(chan error, len(nodes))
	var group sync.WaitGroup
	for _, n := range nodes {
		group.Go(func() {
			response, probeErr := s.nodeRPC(ctx, n.id, worker.RPCRequest{Method: "probe"})
			if err := s.noteHostIdentity(ctx, n.id, probeErr); err != nil {
				failures <- err
				return
			}
			state := "unreachable"
			if probeErr == nil && response.Probe != nil && response.Probe.Protocol == 1 {
				state = "ssh_verified"
				if response.Probe.Ready {
					state = "ready"
				}
			}
			data, err := json.Marshal(response.Probe)
			if err == nil {
				result, updateErr := s.DB.Exec(ctx, "UPDATE nodes SET state=$1,capabilities=$2,last_checked_at=now() WHERE id=$3 AND address=$4 AND port=$5", state, data, n.id, n.address, n.port)
				err = updateErr
				if err == nil && result.RowsAffected() == 1 {
					if response.Probe != nil {
						err = s.storeNodeMetric(ctx, n.id, response.Probe.Metrics)
					}
					if err == nil {
						err = s.nodeHealth(ctx, n.id, state, response.Probe)
					}
				}
			}
			if err != nil {
				failures <- err
			}
		})
	}
	group.Wait()
	close(failures)
	for err := range failures {
		return err
	}
	return nil
}
