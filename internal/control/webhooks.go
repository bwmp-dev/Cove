package control

import (
	"context"
	"cove/internal/worker"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *Server) getWebhook(w http.ResponseWriter, r *http.Request) {
	var enabled bool
	err := s.DB.QueryRow(r.Context(), "SELECT webhook_secret IS NOT NULL FROM workloads WHERE id=$1", r.PathValue("id")).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	var managed bool
	if err = s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM github_sources WHERE workload_id=$1)", r.PathValue("id")).Scan(&managed); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"app_managed": managed, "enabled": enabled, "url": s.PublicURL + "/hooks/github/" + r.PathValue("id")})
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	secret := randomToken()
	encrypted, err := seal(s.Key, []byte(secret))
	if err != nil {
		http.Error(w, "could not protect webhook", 500)
		return
	}
	result, err := s.DB.Exec(r.Context(), "UPDATE workloads SET webhook_secret=$2 WHERE id=$1", r.PathValue("id"), encrypted)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if result.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	respond(w, 201, map[string]string{"secret": secret, "url": s.PublicURL + "/hooks/github/" + r.PathValue("id")})
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	result, err := tx.Exec(r.Context(), "UPDATE workloads SET webhook_secret=NULL WHERE id=$1", r.PathValue("id"))
	if err == nil && result.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM webhook_deliveries WHERE workload_id=$1 AND operation_id IS NULL", r.PathValue("id"))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not disable webhook", 500)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
	if err != nil {
		http.Error(w, "invalid webhook body", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var encrypted, configuration []byte
	var stopped bool
	err = tx.QueryRow(r.Context(), "SELECT webhook_secret,configuration,(manually_stopped OR EXISTS(SELECT 1 FROM environments e WHERE e.id=workloads.environment_id AND e.paused) OR EXISTS(SELECT 1 FROM environment_suspensions c WHERE c.workload_id=workloads.id)) FROM workloads WHERE id=$1 FOR UPDATE", r.PathValue("id")).Scan(&encrypted, &configuration, &stopped)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && len(encrypted) == 0 {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	secret, err := unseal(s.Key, encrypted)
	if err != nil {
		http.Error(w, "webhook unavailable", 500)
		return
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	signature, err := hex.DecodeString(strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256="))
	if err != nil || !strings.HasPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=") || !hmac.Equal(signature, mac.Sum(nil)) {
		http.Error(w, "invalid webhook signature", 401)
		return
	}
	if stopped {
		respond(w, 200, map[string]string{"status": "ignored", "reason": "manually stopped"})
		return
	}
	if r.Header.Get("X-GitHub-Event") == "ping" {
		respond(w, 200, map[string]string{"status": "ok"})
		return
	}
	if r.Header.Get("X-GitHub-Event") != "push" {
		http.Error(w, "only push events are supported", 400)
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if !regexp.MustCompile(`^[a-zA-Z0-9-]{1,100}$`).MatchString(delivery) {
		http.Error(w, "invalid delivery ID", 400)
		return
	}
	var event struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Deleted    bool   `json:"deleted"`
		Repository struct {
			CloneURL string `json:"clone_url"`
		} `json:"repository"`
	}
	if json.Unmarshal(body, &event) != nil {
		http.Error(w, "invalid push event", 400)
		return
	}
	data, err := unseal(s.Key, configuration)
	var config workloadInput
	if err != nil || json.Unmarshal(data, &config) != nil {
		http.Error(w, "configuration unavailable", 500)
		return
	}
	if event.Deleted || config.Source == nil || event.Ref != "refs/heads/"+config.Source.Branch || strings.TrimSuffix(event.Repository.CloneURL, ".git") != strings.TrimSuffix(config.Source.Repository, ".git") {
		respond(w, 200, map[string]string{"status": "ignored"})
		return
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(event.After) || event.After == strings.Repeat("0", 40) {
		http.Error(w, "invalid commit", 400)
		return
	}
	if !pushMatchesWatchPaths(body, config.Source.WatchPaths) {
		if _, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status) VALUES('GitHub webhook','push skipped: no watched paths changed',$1,200)", r.PathValue("id")); err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			http.Error(w, "could not record webhook", 503)
			return
		}
		respond(w, 200, map[string]string{"status": "ignored", "reason": "no watched paths changed"})
		return
	}
	config.Source.Revision = event.After
	request := worker.Request{ID: resourceID(), Workload: r.PathValue("id"), Action: "deploy", Source: config.Source, Compose: config.Compose, Environment: config.Environment, Kind: config.Kind, JobService: config.JobService, Routes: config.Routes, Private: config.Private, Limits: config.Limits, Hooks: config.Hooks, FileSecrets: config.FileSecrets, GuardedDeploy: config.GuardedDeploy, Rollout: config.Rollout}
	if err = worker.ValidateRequest(request); err != nil {
		http.Error(w, "invalid deployment configuration", 400)
		return
	}
	data, err = json.Marshal(request)
	if err == nil {
		encrypted, err = seal(s.Key, data)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO webhook_deliveries(workload_id,delivery,payload) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", request.Workload, delivery, encrypted)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status) VALUES('GitHub webhook','push',$1,202)", request.Workload)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not queue webhook", 500)
		return
	}
	respond(w, 202, map[string]string{"status": "accepted"})
}

func (s *Server) dispatchWebhooks(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT d.workload_id FROM webhook_deliveries d LEFT JOIN webhook_deliveries p ON p.workload_id=d.prerequisite_workload AND p.delivery=d.prerequisite_delivery WHERE d.operation_id IS NULL AND d.superseded_by IS NULL AND (d.prerequisite_workload IS NULL OR p.operation_id IS NOT NULL OR p.workload_id IS NULL) LIMIT 32`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.dispatchWebhook(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) dispatchWebhook(ctx context.Context, id string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var enabled bool
	var configuration []byte
	if err = tx.QueryRow(ctx, "SELECT NOT manually_stopped AND NOT EXISTS(SELECT 1 FROM environments e WHERE e.id=workloads.environment_id AND e.paused) AND NOT EXISTS(SELECT 1 FROM environment_suspensions c WHERE c.workload_id=workloads.id) AND (webhook_secret IS NOT NULL OR EXISTS(SELECT 1 FROM github_sources WHERE workload_id=$1 AND automatic)),configuration FROM workloads WHERE id=$1 FOR UPDATE", id).Scan(&enabled, &configuration); err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	var active bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))", id).Scan(&active); err != nil {
		return err
	}
	if active {
		return nil
	}
	if err = s.coalesceWebhooks(ctx, tx, id); err != nil {
		return err
	}
	var delivery string
	var encrypted []byte
	err = tx.QueryRow(ctx, "SELECT delivery,payload FROM webhook_deliveries WHERE workload_id=$1 AND operation_id IS NULL AND superseded_by IS NULL ORDER BY created_at,delivery LIMIT 1", id).Scan(&delivery, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var prerequisite string
	var waiting, abandoned bool
	err = tx.QueryRow(ctx, `SELECT COALESCE(p.operation_id,''), d.prerequisite_workload IS NOT NULL AND p.operation_id IS NULL, d.prerequisite_workload IS NOT NULL AND p.workload_id IS NULL FROM webhook_deliveries d LEFT JOIN webhook_deliveries p ON p.workload_id=d.prerequisite_workload AND p.delivery=d.prerequisite_delivery WHERE d.workload_id=$1 AND d.delivery=$2`, id, delivery).Scan(&prerequisite, &waiting, &abandoned)
	if err != nil {
		return err
	}
	if abandoned {
		if _, err = tx.Exec(ctx, "DELETE FROM webhook_deliveries WHERE workload_id=$1 AND delivery=$2 AND operation_id IS NULL", id, delivery); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if waiting {
		return nil
	}
	data, err := unseal(s.Key, encrypted)
	if err != nil {
		return err
	}
	var request worker.Request
	if err = json.Unmarshal(data, &request); err != nil {
		return err
	}
	// Routes change live after a push is received, so deploy the saved ones instead of replaying
	// hostnames that may since have been released to another resource.
	if data, err = unseal(s.Key, configuration); err != nil {
		return err
	}
	var config workloadInput
	if err = json.Unmarshal(data, &config); err != nil {
		return err
	}
	request.Routes = config.Routes
	request.Limits = config.Limits
	request.Private = config.Private
	if err = loadNetworkRequest(ctx, tx, &request); err != nil {
		return err
	}
	data, err = json.Marshal(request)
	if err != nil {
		return err
	}
	encrypted, err = seal(s.Key, data)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'deploy',$3)", request.ID, id, encrypted); err != nil {
		return err
	}
	if prerequisite != "" {
		if _, err = tx.Exec(ctx, "INSERT INTO operation_dependencies(operation_id,prerequisite_id) VALUES($1,$2)", request.ID, prerequisite); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, "UPDATE webhook_deliveries SET operation_id=$3 WHERE workload_id=$1 AND delivery=$2", id, delivery, request.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
