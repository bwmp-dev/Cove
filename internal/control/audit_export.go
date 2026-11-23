package control

import (
	"bytes"
	"context"
	"cove/internal/outbound"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type auditExportConfig struct {
	Webhook   string `json:"webhook_url"`
	Secret    string `json:"webhook_secret,omitempty"`
	Syslog    string `json:"syslog_address"`
	TLS       bool   `json:"syslog_tls"`
	Retention int    `json:"retention_days"`
}

func (s *Server) auditExportSettings(w http.ResponseWriter, r *http.Request) {
	var encrypted []byte
	var c auditExportConfig
	err := s.DB.QueryRow(r.Context(), "SELECT configuration FROM audit_export_settings WHERE id=1").Scan(&encrypted)
	if err == nil {
		plain, e := unseal(s.Key, encrypted)
		if e != nil || json.Unmarshal(plain, &c) != nil {
			http.Error(w, "settings unavailable", 503)
			return
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "settings unavailable", 503)
		return
	}
	if c.Retention == 0 {
		c.Retention = 365
	}
	if r.Method != "GET" {
		var in auditExportConfig
		if !decode(w, r, &in) {
			return
		}
		if in.Secret == "" && in.Webhook != "" {
			in.Secret = c.Secret
		}
		if in.Webhook != "" {
			u, e := url.Parse(in.Webhook)
			if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || len(in.Webhook) > 2048 || len(in.Secret) < 32 || len(in.Secret) > 4096 {
				http.Error(w, "HTTPS webhook and secret of at least 32 bytes required", 400)
				return
			}
		}
		if in.Syslog != "" {
			host, port, e := net.SplitHostPort(in.Syslog)
			number, _ := strconv.Atoi(port)
			if e != nil || host == "" || number < 1 || number > 65535 {
				http.Error(w, "valid syslog host:port required", 400)
				return
			}
		}
		if in.Retention < 1 || in.Retention > 3650 {
			http.Error(w, "retention must be 1-3650 days", 400)
			return
		}
		if in.Webhook == "" {
			in.Secret = ""
		}
		raw, e := json.Marshal(in)
		if e == nil {
			raw, e = seal(s.Key, raw)
		}
		if e != nil {
			http.Error(w, "settings unavailable", 503)
			return
		}
		if _, e = s.DB.Exec(r.Context(), "INSERT INTO audit_export_settings(id,configuration,webhook_enabled,syslog_enabled,retention_days) VALUES(1,$1,$2,$3,$4) ON CONFLICT(id) DO UPDATE SET configuration=$1,webhook_enabled=$2,syslog_enabled=$3,retention_days=$4", raw, in.Webhook != "", in.Syslog != "", in.Retention); e != nil {
			http.Error(w, "settings unavailable", 503)
			return
		}
		c = in
	}
	configured := c.Secret != ""
	c.Secret = ""
	var pending int64
	if s.DB.QueryRow(r.Context(), "SELECT count(*) FROM audit_deliveries").Scan(&pending) != nil {
		http.Error(w, "settings unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"configuration": c, "webhook_secret_configured": configured, "pending_deliveries": pending})
}
func (s *Server) deliverAudit(ctx context.Context, c auditExportConfig, destination string, id int64, payload []byte) error {
	if destination == "webhook" {
		request, err := http.NewRequestWithContext(ctx, "POST", c.Webhook, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		signCoveWebhook(request, c.Secret, payload)
		request.Header.Set("X-Cove-Delivery", strconv.FormatInt(id, 10))
		config, err := s.outboundConfig(ctx)
		if err != nil {
			return errors.New("outbound configuration unavailable")
		}
		transport := publicWebhookTransport(config)
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if s.NotificationTransport != nil {
			client.Transport = s.NotificationTransport
		}
		response, err := client.Do(request)
		if err != nil {
			return errors.New("audit webhook failed")
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return errors.New("audit webhook rejected delivery")
		}
		return nil
	}
	config, err := s.outboundConfig(ctx)
	if err != nil {
		return errors.New("outbound configuration unavailable")
	}
	scheme := "http"
	if c.TLS {
		scheme = "https"
	}
	proxy, err := config.For(scheme, c.Syslog)
	if err != nil {
		return errors.New("audit syslog proxy settings unavailable")
	}
	connection, err := outbound.Tunnel(ctx, proxy, "tcp", c.Syslog)
	if err != nil {
		return errors.New("audit syslog connection failed")
	}
	defer connection.Close()
	if c.TLS {
		host, _, e := net.SplitHostPort(c.Syslog)
		if e != nil {
			return e
		}
		secured := tls.Client(connection, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err = secured.HandshakeContext(ctx); err != nil {
			return errors.New("audit syslog TLS connection failed")
		}
		connection = secured
	}
	if err != nil {
		return errors.New("audit syslog connection failed")
	}
	if err = connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	message := fmt.Sprintf("<134>1 %s - cove - audit [cove@32473 delivery=\"%d\"] %s", time.Now().UTC().Format(time.RFC3339Nano), id, payload)
	_, err = fmt.Fprintf(connection, "%d %s", len(message), message)
	return err
}
func (s *Server) runAuditExport(ctx context.Context) {
	timer := time.NewTicker(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := s.dispatchAuditExport(ctx); err != nil {
			slog.Error("audit export unavailable")
		}
	}
}
func (s *Server) dispatchAuditExport(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id int64
	var destination string
	var encrypted, payload []byte
	var attempts int
	err = tx.QueryRow(ctx, "SELECT id,destination,configuration,payload,attempts FROM audit_deliveries WHERE next_attempt_at<=now() ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1").Scan(&id, &destination, &encrypted, &payload, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	plain, err := unseal(s.Key, encrypted)
	var c auditExportConfig
	if err != nil {
		return err
	}
	if err = json.Unmarshal(plain, &c); err != nil {
		return err
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err = s.deliverAudit(deliveryCtx, c, destination, id, payload); err == nil {
		_, err = tx.Exec(ctx, "DELETE FROM audit_deliveries WHERE id=$1", id)
	} else {
		if attempts == 0 {
			if _, queueErr := tx.Exec(ctx, `SELECT cove_queue_channel_event('audit_export.failed','Audit export delivery failed. Its durable queue will retry.',NULL,'','audit-export:'||$1::bigint::text)`, id); queueErr != nil {
				return queueErr
			}
		}
		delay := time.Duration(1<<min(attempts, 10)) * time.Second
		_, err = tx.Exec(ctx, "UPDATE audit_deliveries SET attempts=attempts+1,next_attempt_at=$2 WHERE id=$1", id, time.Now().Add(delay))
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	return err
}
