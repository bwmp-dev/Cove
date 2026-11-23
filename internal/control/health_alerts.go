package control

import (
	"context"
	"cove/internal/worker"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) recordHealth(ctx context.Context, team, kind, subject, message string, bad bool) error {
	return s.recordHealthRouted(ctx, team, kind, subject, message, bad, "")
}

func (s *Server) recordHealthRouted(ctx context.Context, team, kind, subject, message string, bad bool, ruleID string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	confirmation, repeat := 120, 86400
	maxGap := 120
	var channels []string
	if ruleID != "" {
		err = tx.QueryRow(ctx, `SELECT confirmation_seconds,repeat_seconds,channel_ids,GREATEST(120,COALESCE((SELECT interval_seconds*2+timeout_seconds FROM route_checks c WHERE c.workload_id=r.workload_id AND c.domain=r.domain AND r.metric='uptime'),120)) FROM threshold_rules r WHERE id=$1 AND team_id=$2 FOR SHARE`, ruleID, team).Scan(&confirmation, &repeat, &channels, &maxGap)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO health_alerts(team_id,kind,subject,message) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, team, kind, subject, message)
	if err != nil {
		return err
	}
	if ruleID != "" && bad {
		if _, err = tx.Exec(ctx, `UPDATE health_alerts SET first_seen=now() WHERE team_id=$1 AND kind=$2 AND subject=$3 AND NOT active AND checked_at<now()-make_interval(secs=>$4)`, team, kind, subject, maxGap); err != nil {
			return err
		}
	}
	var active, notify bool
	err = tx.QueryRow(ctx, `SELECT active,first_seen<=now()-make_interval(secs=>$4) AND (last_notified IS NULL OR last_notified<=now()-make_interval(secs=>$5)) FROM health_alerts WHERE team_id=$1 AND kind=$2 AND subject=$3 FOR UPDATE`, team, kind, subject, confirmation, repeat).Scan(&active, &notify)
	if err != nil {
		return err
	}
	event := ""
	if bad && notify {
		event = kind + ".alert"
		active = true
	}
	if !bad && active {
		event = kind + ".recovered"
		message = "Recovered: " + message
		active = false
	}
	if event != "" {
		if ruleID != "" {
			if bad {
				event = "threshold.alert"
			} else {
				event = "threshold.recovered"
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO health_deliveries(id,team_id,connection_id,event,message,alert_key,subject_id) SELECT $1||':'||id,team_id,id,$3,$4,$5,$6 FROM infrastructure_connections WHERE team_id=$2 AND kind='notification' AND ($7::text[] IS NULL OR id=ANY($7))`, resourceID(), team, event, message, team+":"+kind+":"+subject, subject, channels)
		if err != nil {
			return err
		}
	}
	if !bad {
		_, err = tx.Exec(ctx, `DELETE FROM health_alerts WHERE team_id=$1 AND kind=$2 AND subject=$3`, team, kind, subject)
	} else {
		_, err = tx.Exec(ctx, `UPDATE health_alerts SET message=$4,checked_at=now(),active=$5,last_notified=CASE WHEN $6 THEN now() ELSE last_notified END WHERE team_id=$1 AND kind=$2 AND subject=$3`, team, kind, subject, message, active, event != "")
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) checkBackupHealth(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT w.id,w.team_id,w.name,GREATEST(COALESCE((SELECT max(updated_at) FROM operations WHERE workload_id=w.id AND action='backup' AND state='succeeded'),w.backup_policy_since),w.backup_policy_since)<now()-make_interval(hours=>w.backup_interval*2) FROM workloads w WHERE w.backup_interval>0`)
	if err != nil {
		return err
	}
	type item struct {
		id, team, name string
		bad            bool
	}
	var items []item
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.id, &i.team, &i.name, &i.bad); err != nil {
			break
		}
		items = append(items, i)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, i := range items {
		if err = s.recordHealth(ctx, i.team, "backup_overdue", i.id, "Scheduled backup overdue for "+i.name, i.bad); err != nil {
			return err
		}
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM health_alerts a WHERE
 (kind='backup_overdue' AND NOT EXISTS(SELECT 1 FROM workloads w WHERE w.id=a.subject AND w.team_id=a.team_id AND w.backup_interval>0)) OR
 (kind='resource_unhealthy' AND NOT EXISTS(SELECT 1 FROM workloads w WHERE w.id=a.subject AND w.team_id=a.team_id AND w.kind='service')) OR
 (kind IN ('node_unreachable','disk_low','host_key_changed') AND NOT EXISTS(SELECT 1 FROM team_nodes t WHERE t.node_id=a.subject AND t.team_id=a.team_id)) OR
 (kind LIKE 'threshold_%' AND NOT EXISTS(SELECT 1 FROM threshold_rules r JOIN team_nodes t ON t.team_id=r.team_id AND t.node_id=r.node_id WHERE a.kind='threshold_'||r.id AND a.team_id=r.team_id AND a.subject=COALESCE(r.workload_id,r.node_id)))`)
	return err
}

func (s *Server) dispatchHealthNotifications(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `UPDATE health_deliveries SET attempts=attempts+1,next_attempt=now()+interval '1 minute' WHERE id IN (SELECT id FROM health_deliveries WHERE state='pending' AND next_attempt<=now() ORDER BY next_attempt LIMIT 4 FOR UPDATE SKIP LOCKED) RETURNING id,team_id,connection_id,event,message,alert_key,subject_id,attempts`)
	if err != nil {
		return err
	}
	type job struct {
		id, team, connection, event, message, alertKey, subject string
		attempt                                                 int
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err = rows.Scan(&j.id, &j.team, &j.connection, &j.event, &j.message, &j.alertKey, &j.subject, &j.attempt); err != nil {
			break
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, j := range jobs {
		state, last := "sent", ""
		c, readErr := s.readConnection(ctx, j.connection, j.team, "notification")
		if errors.Is(readErr, pgx.ErrNoRows) {
			state = "skipped"
		} else if readErr != nil {
			return readErr
		} else if !notificationMatches(c.Notification, j.event) {
			state = "skipped"
		} else {
			if c.Notification.Owner {
				c.Notification.Email, err = s.notificationOwnerEmail(ctx, j.team, j.subject)
				if err != nil {
					return err
				}
				if c.Notification.Email == "" {
					state = "skipped"
				}
			}
			if j.alertKey != "" && (c.Notification.Provider == "pagerduty" || c.Notification.Provider == "opsgenie") {
				var earlier bool
				if err = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM health_deliveries p JOIN health_deliveries d ON d.id=$1 WHERE p.connection_id=d.connection_id AND p.alert_key=d.alert_key AND p.state='pending' AND (p.created_at,p.id)<(d.created_at,d.id))`, j.id).Scan(&earlier); err != nil {
					return err
				}
				if earlier {
					if _, err = s.DB.Exec(ctx, `UPDATE health_deliveries SET attempts=attempts-1 WHERE id=$1`, j.id); err != nil {
						return err
					}
					continue
				}
			}
			if state != "skipped" {
				if sendErr := s.sendNotification(ctx, c.Notification, map[string]string{"delivery_id": j.id, "event": j.event, "message": "Cove: " + j.message, "alert_key": j.alertKey}); sendErr != nil {
					last = sendErr.Error()
					state = "pending"
					if j.attempt >= 6 {
						state = "failed"
					}
				}
			}
		}
		if _, err = s.DB.Exec(ctx, `UPDATE health_deliveries SET state=$2,last_error=$3,next_attempt=now()+make_interval(secs=>LEAST(3600,30*power(2,attempts)::integer)) WHERE id=$1`, j.id, state, last); err != nil {
			return err
		}
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM health_deliveries WHERE state<>'pending' AND created_at<now()-interval '30 days'`)
	return err
}

func (s *Server) healthAlerts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), `SELECT kind,subject,message,first_seen,checked_at,active FROM health_alerts WHERE team_id=$1 ORDER BY first_seen DESC LIMIT 100`, actor(r).Team)
	if err != nil {
		http.Error(w, "health alerts unavailable", 503)
		return
	}
	defer rows.Close()
	type item struct {
		Kind    string    `json:"kind"`
		Subject string    `json:"subject"`
		Message string    `json:"message"`
		First   time.Time `json:"first_seen"`
		Checked time.Time `json:"checked_at"`
		Active  bool      `json:"active"`
	}
	items := []item{}
	for rows.Next() {
		var i item
		if rows.Scan(&i.Kind, &i.Subject, &i.Message, &i.First, &i.Checked, &i.Active) != nil {
			http.Error(w, "health alerts unavailable", 503)
			return
		}
		items = append(items, i)
	}
	if rows.Err() != nil {
		http.Error(w, "health alerts unavailable", 503)
		return
	}
	respond(w, 200, items)
}

func (s *Server) nodeHealth(ctx context.Context, id, state string, p *worker.Probe) error {
	rows, err := s.DB.Query(ctx, `SELECT t.team_id,n.name FROM team_nodes t JOIN nodes n ON n.id=t.node_id WHERE n.id=$1`, id)
	if err != nil {
		return err
	}
	type team struct{ id, name string }
	var teams []team
	for rows.Next() {
		var t team
		if err = rows.Scan(&t.id, &t.name); err != nil {
			break
		}
		teams = append(teams, t)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, t := range teams {
		if err = s.recordHealth(ctx, t.id, "node_unreachable", id, "Worker unavailable: "+t.name, state != "ready"); err != nil {
			return err
		}
		if p != nil && p.Metrics != nil && p.Metrics.DiskTotal > 0 {
			bad := float64(p.Metrics.DiskAvailable)/float64(p.Metrics.DiskTotal) < 0.10
			if err = s.recordHealth(ctx, t.id, "disk_low", id, "Less than 10% free space on Cove's data filesystem: "+t.name, bad); err != nil {
				return err
			}
		}
	}
	if state == "ready" && p != nil && p.Metrics != nil {
		return s.evaluateThresholdSample(ctx, id, p.Metrics)
	}
	return nil
}

func (s *Server) checkResourceHealth(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT id,node_id,team_id,name,manually_stopped FROM workloads w WHERE kind='service' AND (health_checked_at IS NULL OR health_checked_at<now()-interval '1 minute') AND EXISTS(SELECT 1 FROM operations WHERE workload_id=w.id AND action IN ('deploy','rollback') AND state='succeeded') AND NOT EXISTS(SELECT 1 FROM operations WHERE workload_id=w.id AND state IN ('queued','running')) ORDER BY health_checked_at NULLS FIRST LIMIT 8`)
	if err != nil {
		return err
	}
	type item struct {
		id, node, team, name string
		stopped              bool
	}
	var items []item
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.id, &i.node, &i.team, &i.name, &i.stopped); err != nil {
			break
		}
		items = append(items, i)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, i := range items {
		if _, err = s.DB.Exec(ctx, `UPDATE workloads SET health_checked_at=now() WHERE id=$1`, i.id); err != nil {
			return err
		}
		if err = s.evaluateResourceThresholds(ctx, i.id); err != nil {
			return err
		}
		if i.stopped {
			if _, err = s.DB.Exec(ctx, `DELETE FROM health_alerts WHERE team_id=$1 AND subject=$2 AND kind='resource_unhealthy'`, i.team, i.id); err != nil {
				return err
			}
			continue
		}
		response, rpcErr := s.nodeRPC(ctx, i.node, worker.RPCRequest{Method: "container_health", ID: i.id})
		if rpcErr != nil || response.Error != "" {
			continue
		}
		bad := len(response.Containers) == 0
		for _, container := range response.Containers {
			if !container.ExpectedCompleted && (container.State != "running" || strings.Contains(container.Status, "(unhealthy)")) {
				bad = true
			}
		}
		// An operator may have stopped or redeployed it while the SSH read was in flight.
		var suppressed bool
		if err = s.DB.QueryRow(ctx, `SELECT manually_stopped OR EXISTS(SELECT 1 FROM operations WHERE workload_id=w.id AND state IN ('queued','running')) FROM workloads w WHERE id=$1`, i.id).Scan(&suppressed); errors.Is(err, pgx.ErrNoRows) {
			continue
		} else if err != nil {
			return err
		}
		if suppressed {
			continue
		}
		if err = s.recordHealth(ctx, i.team, "resource_unhealthy", i.id, "Resource stopped unexpectedly or unhealthy: "+i.name, bad); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) healthDeliveryHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), `SELECT d.id,c.name,d.event,d.state,d.attempts,d.last_error,d.created_at FROM health_deliveries d JOIN infrastructure_connections c ON c.id=d.connection_id WHERE d.team_id=$1 AND c.team_id=$1 ORDER BY d.created_at DESC LIMIT 50`, actor(r).Team)
	if err != nil {
		http.Error(w, "health delivery history unavailable", 503)
		return
	}
	defer rows.Close()
	type item struct {
		ID       string    `json:"id"`
		Name     string    `json:"name"`
		Event    string    `json:"event"`
		State    string    `json:"state"`
		Attempts int       `json:"attempts"`
		Error    string    `json:"error"`
		Created  time.Time `json:"created_at"`
	}
	items := []item{}
	for rows.Next() {
		var i item
		if rows.Scan(&i.ID, &i.Name, &i.Event, &i.State, &i.Attempts, &i.Error, &i.Created) != nil {
			http.Error(w, "health delivery history unavailable", 503)
			return
		}
		items = append(items, i)
	}
	if rows.Err() != nil {
		http.Error(w, "health delivery history unavailable", 503)
		return
	}
	respond(w, 200, items)
}
