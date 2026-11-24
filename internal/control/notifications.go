package control

import (
	"bytes"
	"context"
	"cove/internal/netpolicy"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"io"
	"net"
	"net/http"
	"time"
)

func publicWebhookIP(ip net.IP) bool { return netpolicy.PublicIP(ip) }

func publicWebhookTransport() *http.Transport {
	return &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("webhook host unavailable")
		}
		for _, ip := range ips {
			if !publicWebhookIP(ip.IP) {
				return nil, errors.New("webhook must resolve to a public address")
			}
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second}
}

func (s *Server) sendNotification(ctx context.Context, c *notificationConfig, event map[string]string) error {
	var payload any = event
	message := event["message"]
	switch c.Provider {
	case "discord":
		payload = map[string]any{"content": message, "allowed_mentions": map[string]any{"parse": []string{}}}
	case "slack":
		payload = map[string]any{"text": message, "mrkdwn": false, "unfurl_links": false, "unfurl_media": false}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Cove")
	req.Header.Set("X-Cove-Delivery", event["delivery_id"])
	transport := publicWebhookTransport()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if s.NotificationTransport != nil {
		client.Transport = s.NotificationTransport
	}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("webhook request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (s *Server) dispatchNotifications(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `UPDATE notification_deliveries d SET attempts=attempts+1,next_attempt=now()+interval '1 minute'
 WHERE (operation_id,connection_id) IN (SELECT operation_id,connection_id FROM notification_deliveries WHERE state='pending' AND next_attempt<=now() ORDER BY next_attempt LIMIT 4 FOR UPDATE SKIP LOCKED)
 RETURNING operation_id,connection_id,attempts`)
	if err != nil {
		return err
	}
	type job struct {
		operation, connection string
		attempt               int
	}
	jobs := []job{}
	for rows.Next() {
		var j job
		if err = rows.Scan(&j.operation, &j.connection, &j.attempt); err != nil {
			break
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return err
	}
	errorsOut := make(chan error, len(jobs))
	for _, j := range jobs {
		go func() { errorsOut <- s.deliverNotification(ctx, j.operation, j.connection, j.attempt) }()
	}
	for range jobs {
		if e := <-errorsOut; e != nil && err == nil {
			err = e
		}
	}
	return err
}

func (s *Server) deliverNotification(ctx context.Context, operation, connection string, attempt int) error {
	var team, name, action, state string
	err := s.DB.QueryRow(ctx, `SELECT w.team_id,w.name,o.action,o.state FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE o.id=$1`, operation).Scan(&team, &name, &action, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	c, err := s.readConnection(ctx, connection, team, "notification")
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	status := "sent"
	message := ""
	n := c.Notification
	scanEvent, scanSummary, scanAllowed, scanErr := s.scanNotification(ctx, operation, action)
	if errors.Is(scanErr, errScanBaselinePending) {
		_, err := s.DB.Exec(ctx, `UPDATE notification_deliveries SET attempts=GREATEST(0,attempts-1),next_attempt=now()+interval '1 minute',last_error='Waiting for an earlier automatic scan result before comparing findings.' WHERE operation_id=$1 AND connection_id=$2`, operation, connection)
		return err
	}
	if scanErr != nil {
		return scanErr
	}
	if !n.Enabled || !scanAllowed || state == "succeeded" && !n.Success || state != "succeeded" && !n.Failure {
		status = "skipped"
	} else {
		event := map[string]string{"delivery_id": operation + ":" + connection, "event": "operation." + state, "operation_id": operation, "resource": name, "action": action, "state": state, "url": s.PublicURL + "/operations/" + operation, "message": "Cove: " + action + " " + state + " for " + name + ". " + s.PublicURL + "/operations/" + operation}
		if scanEvent != "" {
			event["event"] = scanEvent
			event["message"] = "Cove: vulnerability scan for " + name + ". " + scanSummary + " " + event["url"]
		}
		var updateSummary string
		updateErr := s.DB.QueryRow(ctx, `SELECT summary FROM image_update_events WHERE operation_id=$1`, operation).Scan(&updateSummary)
		if updateErr != nil && !errors.Is(updateErr, pgx.ErrNoRows) {
			return updateErr
		}
		if updateErr == nil {
			event["event"] = "image.update_available"
			event["message"] = "Cove: image updates available for " + name + ". " + updateSummary + " " + event["url"]
		}
		if err = s.sendNotification(ctx, n, event); err != nil {
			message = err.Error()
			status = "pending"
			if attempt >= 6 {
				status = "failed"
			}
		}
	}
	_, err = s.DB.Exec(ctx, `UPDATE notification_deliveries SET state=$3,last_error=$4,next_attempt=now()+make_interval(secs=>LEAST(3600,30*power(2,attempts)::integer)) WHERE operation_id=$1 AND connection_id=$2`, operation, connection, status, message)
	return err
}

func (s *Server) notificationHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), `SELECT d.operation_id,c.name,d.state,d.attempts,d.last_error FROM notification_deliveries d JOIN infrastructure_connections c ON c.id=d.connection_id WHERE c.team_id=$1 ORDER BY d.created_at DESC LIMIT 50`, actor(r).Team)
	if err != nil {
		http.Error(w, "notification history unavailable", 503)
		return
	}
	defer rows.Close()
	type delivery struct {
		Operation string `json:"operation_id"`
		Name      string `json:"name"`
		State     string `json:"state"`
		Attempts  int    `json:"attempts"`
		Error     string `json:"error"`
	}
	items := []delivery{}
	for rows.Next() {
		var d delivery
		if rows.Scan(&d.Operation, &d.Name, &d.State, &d.Attempts, &d.Error) != nil {
			http.Error(w, "notification history unavailable", 503)
			return
		}
		items = append(items, d)
	}
	if rows.Err() != nil {
		http.Error(w, "notification history unavailable", 503)
		return
	}
	respond(w, 200, items)
}
