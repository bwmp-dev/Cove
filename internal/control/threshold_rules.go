package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type thresholdRule struct {
	MountPath    string   `json:"mount_path"`
	WorkloadID   string   `json:"workload_id"`
	Domain       string   `json:"domain"`
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	NodeID       string   `json:"node_id"`
	Metric       string   `json:"metric"`
	Threshold    float64  `json:"threshold"`
	Confirmation int      `json:"confirmation_seconds"`
	Repeat       int      `json:"repeat_seconds"`
	Channels     []string `json:"channel_ids"`
}

func (s *Server) thresholdRules(w http.ResponseWriter, r *http.Request) {
	ctx, team := r.Context(), actor(r).Team
	if r.Method == http.MethodGet {
		rows, err := s.DB.Query(ctx, `SELECT id,name,node_id,metric,threshold,confirmation_seconds,repeat_seconds,channel_ids,COALESCE(workload_id,''),domain,mount_path FROM threshold_rules WHERE team_id=$1 ORDER BY name,id`, team)
		if err != nil {
			http.Error(w, "rules unavailable", 503)
			return
		}
		defer rows.Close()
		out := []thresholdRule{}
		for rows.Next() {
			var rule thresholdRule
			if err = rows.Scan(&rule.ID, &rule.Name, &rule.NodeID, &rule.Metric, &rule.Threshold, &rule.Confirmation, &rule.Repeat, &rule.Channels, &rule.WorkloadID, &rule.Domain, &rule.MountPath); err != nil {
				http.Error(w, "rules unavailable", 503)
				return
			}
			out = append(out, rule)
		}
		if rows.Err() != nil {
			http.Error(w, "rules unavailable", 503)
			return
		}
		respond(w, 200, out)
		return
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(ctx)
	if r.Method == http.MethodDelete {
		var id string
		err = tx.QueryRow(ctx, `DELETE FROM threshold_rules WHERE id=$1 AND team_id=$2 RETURNING id`, r.PathValue("id"), team).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM health_alerts WHERE team_id=$1 AND kind=$2`, team, "threshold_"+id)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			http.Error(w, "could not delete rule", 503)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	var rule thresholdRule
	if !decode(w, r, &rule) {
		return
	}
	rule.Name = strings.TrimSpace(rule.Name)
	if !validThresholdRule(rule) {
		http.Error(w, "select a valid metric, target, threshold, confirmation, repeat and 1-32 channels", 400)
		return
	}

	var found string
	if err = tx.QueryRow(ctx, `SELECT id FROM teams WHERE id=$1 FOR UPDATE`, team).Scan(&found); err != nil {
		http.Error(w, "team unavailable", 503)
		return
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM threshold_rules WHERE team_id=$1`, team).Scan(&count); err != nil {
		http.Error(w, "rules unavailable", 503)
		return
	}
	if count >= 100 {
		http.Error(w, "a team may have at most 100 threshold rules", 409)
		return
	}
	if rule.WorkloadID != "" {
		var configuration []byte
		var kind string
		err = tx.QueryRow(ctx, `SELECT node_id,kind,configuration FROM workloads WHERE id=$1 AND team_id=$2 FOR SHARE`, rule.WorkloadID, team).Scan(&rule.NodeID, &kind, &configuration)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && kind != "service" {
			http.Error(w, "select a service in this team", 400)
			return
		}
		if err != nil {
			http.Error(w, "resource unavailable", 503)
			return
		}
		if rule.Metric == "certificate" || rule.Metric == "uptime" {
			plain, openErr := unseal(s.Key, configuration)
			var config workloadInput
			if openErr != nil || json.Unmarshal(plain, &config) != nil {
				http.Error(w, "resource unavailable", 503)
				return
			}
			foundRoute := false
			for _, route := range config.Routes {
				if route.Domain == rule.Domain {
					foundRoute = true
				}
			}
			if !foundRoute {
				http.Error(w, "select a configured resource route", 400)
				return
			}
			if rule.Metric == "uptime" {
				var exists bool
				if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM route_checks WHERE workload_id=$1 AND domain=$2 AND team_id=$3 AND enabled)`, rule.WorkloadID, rule.Domain, team).Scan(&exists); err != nil {
					http.Error(w, "uptime checks unavailable", 503)
					return
				}
				if !exists {
					http.Error(w, "create and enable this route's uptime check first", 409)
					return
				}
			}
		}
	}
	err = tx.QueryRow(ctx, `SELECT n.id FROM nodes n JOIN team_nodes t ON t.node_id=n.id WHERE n.id=$1 AND t.team_id=$2 FOR SHARE OF n,t`, rule.NodeID, team).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "select a node assigned to this team", 400)
		return
	}
	if err != nil {
		http.Error(w, "node unavailable", 503)
		return
	}

	if rule.WorkloadID != "" && rule.Metric != "uptime" {
		var supported bool
		if err = tx.QueryRow(ctx, `SELECT COALESCE((capabilities->>'alert_observations')::boolean,false) FROM nodes WHERE id=$1`, rule.NodeID).Scan(&supported); err != nil {
			http.Error(w, "node unavailable", 503)
			return
		}
		if !supported {
			http.Error(w, "upgrade and verify the worker for certificate and container alerts", 409)
			return
		}
	}
	seen := map[string]bool{}
	if rule.MountPath != "" {
		var supported bool
		if err = tx.QueryRow(ctx, `SELECT COALESCE((capabilities->>'mount_metrics')::boolean,false) FROM nodes WHERE id=$1`, rule.NodeID).Scan(&supported); err != nil {
			http.Error(w, "node unavailable", 503)
			return
		}
		if !supported {
			http.Error(w, "upgrade and verify the worker for mount thresholds", 409)
			return
		}
	}
	for _, id := range rule.Channels {
		if seen[id] {
			http.Error(w, "select distinct channels", 400)
			return
		}
		seen[id] = true
		err = tx.QueryRow(ctx, `SELECT id FROM infrastructure_connections WHERE id=$1 AND team_id=$2 AND kind='notification' FOR SHARE`, id, team).Scan(&found)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "select notification channels from this team", 400)
			return
		}
		if err != nil {
			http.Error(w, "channels unavailable", 503)
			return
		}
	}
	rule.ID = resourceID()
	_, err = tx.Exec(ctx, `INSERT INTO threshold_rules(id,team_id,node_id,name,metric,threshold,confirmation_seconds,repeat_seconds,channel_ids,workload_id,domain,mount_path) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,$12)`, rule.ID, team, rule.NodeID, rule.Name, rule.Metric, rule.Threshold, rule.Confirmation, rule.Repeat, rule.Channels, rule.WorkloadID, rule.Domain, rule.MountPath)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		http.Error(w, "could not save rule", 503)
		return
	}
	respond(w, 201, map[string]string{"id": rule.ID})
}

func (s *Server) evaluateThresholdSample(ctx context.Context, node string, sample *worker.NodeMetrics) error {
	if sample.SampledAt.IsZero() || time.Since(sample.SampledAt) > 2*time.Minute || sample.SampledAt.After(time.Now().Add(time.Minute)) {
		return nil
	}
	rows, err := s.DB.Query(ctx, `SELECT r.id,r.team_id,r.name,r.metric,r.threshold,r.mount_path FROM threshold_rules r JOIN team_nodes t ON t.team_id=r.team_id AND t.node_id=r.node_id WHERE r.node_id=$1 AND r.workload_id IS NULL`, node)
	if err != nil {
		return err
	}
	type evaluation struct {
		id, team, name, metric, mount string
		threshold                     float64
	}
	var rules []evaluation
	for rows.Next() {
		var rule evaluation
		if err = rows.Scan(&rule.id, &rule.team, &rule.name, &rule.metric, &rule.threshold, &rule.mount); err != nil {
			break
		}
		rules = append(rules, rule)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, rule := range rules {
		value := sample.CPUPercent
		switch rule.metric {
		case "memory":
			if sample.MemoryTotal == 0 || sample.MemoryAvailable > sample.MemoryTotal {
				continue
			}
			value = 100 * (1 - float64(sample.MemoryAvailable)/float64(sample.MemoryTotal))
		case "disk":
			var known bool
			value, known = diskThresholdValue(sample, rule.mount, rule.threshold)
			if !known {
				continue
			}

		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			continue
		}
		if err = s.recordHealthRouted(ctx, rule.team, "threshold_"+rule.id, node, fmt.Sprintf("%s: %s usage %.1f%% (threshold %.1f%%)", rule.name, rule.metric+" "+rule.mount, value, rule.threshold), value >= rule.threshold, rule.id); err != nil {
			return err
		}
	}
	return nil
}
