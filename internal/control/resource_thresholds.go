package control

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"cove/internal/worker"
)

func validThresholdRule(r thresholdRule) bool {
	if r.Name == "" || len(r.Name) > 120 || math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) || r.Threshold <= 0 || r.Confirmation < 60 || r.Confirmation > 3600 || r.Repeat < 300 || r.Repeat > 604800 || len(r.Channels) < 1 || len(r.Channels) > 32 {
		return false
	}
	switch r.Metric {
	case "cpu", "memory", "disk":
		return r.WorkloadID == "" && r.Domain == "" && r.Threshold <= 100
	case "certificate":
		return r.WorkloadID != "" && r.Domain != "" && len(r.Domain) <= 253 && r.Threshold <= 3650
	case "restarts", "oom", "crashes":
		return r.WorkloadID != "" && r.Domain == "" && r.Threshold <= 1000 && math.Trunc(r.Threshold) == r.Threshold
	}
	return false
}

func resourceThresholdValue(metric, domain string, sample *worker.WorkloadObservations) (float64, bool) {
	if sample == nil || sample.SampledAt.IsZero() || time.Since(sample.SampledAt) > 2*time.Minute || sample.SampledAt.After(time.Now().Add(time.Minute)) {
		return 0, false
	}
	if metric == "certificate" {
		for _, cert := range sample.Certificates {
			if cert.Domain == domain && cert.ExpiresAt != nil {
				return cert.ExpiresAt.Sub(sample.SampledAt).Hours() / 24, true
			}
		}
		return 0, false
	}
	if !sample.EventsAvailable {
		return 0, false
	}
	value := 0
	switch metric {
	case "restarts":
		value = sample.RestartEvents
	case "oom":
		value = sample.OOMEvents
	case "crashes":
		value = sample.CrashEvents
	default:
		return 0, false
	}
	current := 0
	for _, c := range sample.Containers {
		if c.ExpectedCompleted {
			continue
		}
		if metric == "oom" && c.OOMKilled || metric == "crashes" && c.Exited {
			current++
		}
	}
	return float64(max(value, current)), true
}

func (s *Server) evaluateResourceThresholds(ctx context.Context, id string) error {
	rows, err := s.DB.Query(ctx, `SELECT r.id,r.team_id,r.name,r.metric,r.threshold,r.domain,w.node_id FROM threshold_rules r JOIN workloads w ON w.id=r.workload_id AND w.team_id=r.team_id JOIN team_nodes t ON t.team_id=w.team_id AND t.node_id=w.node_id WHERE w.id=$1`, id)
	if err != nil {
		return err
	}
	type evaluation struct {
		id, team, name, metric, domain, node string
		threshold                            float64
	}
	var rules []evaluation
	for rows.Next() {
		var r evaluation
		if err = rows.Scan(&r.id, &r.team, &r.name, &r.metric, &r.threshold, &r.domain, &r.node); err != nil {
			break
		}
		rules = append(rules, r)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	if len(rules) == 0 {
		return nil
	}
	response, rpcErr := s.nodeRPC(ctx, rules[0].node, worker.RPCRequest{Method: "observations", ID: id})
	if rpcErr != nil || response.Error != "" || response.Observations == nil {
		return nil
	}
	if len(response.Observations.Events) > 4096 || len(response.Observations.Containers) > 32 || len(response.Observations.Certificates) > 100 {
		return fmt.Errorf("resource observation exceeds limits")
	}
	windows, err := s.DB.Query(ctx, `SELECT created_at,updated_at FROM operations WHERE workload_id=$1 AND updated_at>now()-interval '11 minutes' ORDER BY updated_at DESC LIMIT 4097`, id)
	if err != nil {
		return err
	}
	type window struct{ start, end time.Time }
	var suppressions []window
	for windows.Next() {
		var w window
		if err = windows.Scan(&w.start, &w.end); err != nil {
			break
		}
		suppressions = append(suppressions, w)
	}
	windows.Close()
	if err != nil {
		return err
	}
	if windows.Err() != nil {
		return windows.Err()
	}
	if len(suppressions) > 4096 {
		return nil
	}
	sample := *response.Observations
	sample.RestartEvents = 0
	sample.OOMEvents = 0
	sample.CrashEvents = 0
	for _, event := range sample.Events {
		if event.Timestamp.Before(sample.SampledAt.Add(-10*time.Minute)) || event.Timestamp.After(sample.SampledAt) {
			continue
		}
		suppressed := false
		for _, w := range suppressions {
			if !event.Timestamp.Before(w.start.Add(-time.Second)) && !event.Timestamp.After(w.end.Add(time.Second)) {
				suppressed = true
				break
			}
		}
		if suppressed {
			continue
		}
		switch event.Action {
		case "start":
			if event.AutomaticRestart {
				sample.RestartEvents++
			}
		case "oom":
			sample.OOMEvents++
		case "die":
			sample.CrashEvents++
		}
	}
	var stopped, busy bool
	if err = s.DB.QueryRow(ctx, `SELECT manually_stopped,EXISTS(SELECT 1 FROM operations WHERE workload_id=w.id AND state IN ('queued','running')) FROM workloads w WHERE id=$1`, id).Scan(&stopped, &busy); err != nil {
		return err
	}
	if busy {
		return nil
	}
	for _, r := range rules {
		value, known := resourceThresholdValue(r.metric, r.domain, &sample)
		if !known {
			continue
		}
		bad := value >= r.threshold
		unit := "events in ten minutes"
		if r.metric == "certificate" {
			bad = value <= r.threshold
			unit = "days until expiry for " + r.domain
		} else if stopped {
			bad = false
		}
		message := fmt.Sprintf("%s: %s %.1f %s (threshold %.1f)", r.name, r.metric, value, unit, r.threshold)
		if strings.ContainsAny(message, "\r\n") {
			message = strings.NewReplacer("\r", " ", "\n", " ").Replace(message)
		}
		if err = s.recordHealthRouted(ctx, r.team, "threshold_"+r.id, id, message, bad, r.id); err != nil {
			return err
		}
	}
	return nil
}
