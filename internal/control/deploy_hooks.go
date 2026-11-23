package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

func (s *Server) deployHookSettings(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method != "GET" && (!s.requireAccess(w, r, "workload", id, 3) || !s.requirePersistentResource(w, r, id)) {
		return
	}
	if r.Method != "GET" {
		tx, err := s.DB.Begin(r.Context())
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		defer tx.Rollback(r.Context())
		var old string
		err = tx.QueryRow(r.Context(), `SELECT id FROM deploy_hooks WHERE workload_id=$1 FOR UPDATE`, id).Scan(&old)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "hook unavailable", 503)
			return
		}
		if old != "" {
			if _, err = tx.Exec(r.Context(), `DELETE FROM webhook_deliveries WHERE workload_id=$1 AND delivery LIKE $2 AND operation_id IS NULL`, id, "hook-"+old+"-%"); err != nil {
				http.Error(w, "could not revoke queued hooks", 503)
				return
			}
			if _, err = tx.Exec(r.Context(), `DELETE FROM deploy_hooks WHERE id=$1`, old); err != nil {
				http.Error(w, "could not revoke hook", 503)
				return
			}
		}
		if r.Method == "DELETE" {
			if err = tx.Commit(r.Context()); err != nil {
				http.Error(w, "could not revoke hook", 503)
				return
			}
			respond(w, 200, map[string]bool{"ok": true})
			return
		}
		hook, token := resourceID(), randomToken()
		expires := time.Now().UTC().Add(90 * 24 * time.Hour)
		if _, err = tx.Exec(r.Context(), `INSERT INTO deploy_hooks(id,workload_id,token_hash,creator_id,expires_at) VALUES($1,$2,$3,$4,$5)`, hook, id, tokenHash(token), actor(r).ID, expires); err != nil {
			http.Error(w, "could not create hook", 503)
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			http.Error(w, "could not create hook", 503)
			return
		}
		respond(w, 201, map[string]any{"id": hook, "url": s.PublicURL + "/hooks/deploy/" + hook, "token": token, "expires_at": expires})
		return
	}
	var hook string
	var expires time.Time
	err := s.DB.QueryRow(r.Context(), `SELECT id,expires_at FROM deploy_hooks WHERE workload_id=$1`, id).Scan(&hook, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		respond(w, 200, nil)
		return
	}
	if err != nil {
		http.Error(w, "hook unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"id": hook, "url": s.PublicURL + "/hooks/deploy/" + hook, "expires_at": expires})
}

func (s *Server) hookAudit(ctx context.Context, hook, action string, status int) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.DB.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES('deploy hook',$2,$1,$3,(SELECT w.team_id FROM deploy_hooks h JOIN workloads w ON w.id=h.workload_id WHERE h.id=$1))`, hook, action, status); err != nil {
		slog.Error("deploy hook audit could not be persisted")
	}
}

func (s *Server) inboundDeployHook(w http.ResponseWriter, r *http.Request) {
	hook := r.PathValue("hook")
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(hook) || len(bearer(r)) > 200 {
		http.Error(w, "unauthorized", 401)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var workload string
	var configuration []byte
	var stopped bool
	err = tx.QueryRow(r.Context(), `SELECT w.id,w.configuration,w.manually_stopped FROM deploy_hooks h JOIN workloads w ON w.id=h.workload_id JOIN users u ON u.id=h.creator_id WHERE h.id=$1 AND h.token_hash=$2 AND h.expires_at>now() AND u.email_verified AND NOT u.disabled AND NOT EXISTS(SELECT 1 FROM authentication_failures f WHERE f.key='account:'||u.id AND f.locked_until>now()) AND (u.is_admin OR cove_permission(u.id,w.team_id,w.project_id,COALESCE(w.environment_id,''),'operate')) AND (NOT ((SELECT require_second_factor FROM control_settings WHERE id=1) OR (SELECT require_second_factor FROM teams WHERE id=w.team_id)) OR EXISTS(SELECT 1 FROM account_totp WHERE user_id=u.id AND confirmed) OR EXISTS(SELECT 1 FROM passkeys WHERE user_id=u.id)) FOR UPDATE OF h,w`, hook, tokenHash(bearer(r))).Scan(&workload, &configuration, &stopped)
	if errors.Is(err, pgx.ErrNoRows) {
		s.hookAudit(r.Context(), hook, "rejected authentication", 401)
		http.Error(w, "unauthorized", 401)
		return
	}
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	if r.Method == "GET" {
		var operation, state string
		err = tx.QueryRow(r.Context(), `SELECT receipt.request_id,CASE WHEN receipt.cancelled THEN 'cancelled' WHEN delivery.superseded_by IS NOT NULL THEN 'superseded' ELSE COALESCE(o.state,'pending') END FROM deploy_hook_receipts receipt LEFT JOIN webhook_deliveries delivery ON delivery.workload_id=$3 AND delivery.delivery=receipt.queue_delivery LEFT JOIN operations o ON o.id=receipt.request_id WHERE receipt.hook_id=$1 AND receipt.delivery=$2`, hook, r.PathValue("delivery"), workload).Scan(&operation, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "delivery unavailable", 503)
			return
		}
		respond(w, 200, map[string]string{"operation_id": operation, "state": state})
		return
	}
	if stopped {
		s.hookAudit(r.Context(), hook, "rejected stopped workload", 409)
		http.Error(w, "workload is manually stopped", 409)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var in struct {
		Delivery string `json:"delivery"`
		Reason   string `json:"reason,omitempty"`
		Revision string `json:"revision,omitempty"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || !validOperationReason(in.Reason) || !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,100}$`).MatchString(in.Delivery) || (in.Revision != "" && (!gitCommit.MatchString(in.Revision) || in.Revision == strings.Repeat("0", 40))) {
		http.Error(w, "delivery ID and optional immutable commit SHA required", 400)
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		http.Error(w, "invalid request", 400)
		return
	}
	var existing string
	err = tx.QueryRow(r.Context(), `SELECT request_id FROM deploy_hook_receipts WHERE hook_id=$1 AND delivery=$2`, hook, in.Delivery).Scan(&existing)
	if err == nil {
		s.hookAudit(r.Context(), hook, "duplicate delivery", 200)
		respond(w, 200, map[string]string{"operation_id": existing, "delivery": in.Delivery})
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "delivery unavailable", 503)
		return
	}
	var permitted bool
	if err = tx.QueryRow(r.Context(), `UPDATE deploy_hooks SET window_start=CASE WHEN window_start<now()-interval '1 minute' THEN now() ELSE window_start END,window_count=CASE WHEN window_start<now()-interval '1 minute' THEN 1 ELSE window_count+1 END WHERE id=$1 AND (window_start<now()-interval '1 minute' OR window_count<10) RETURNING true`, hook).Scan(&permitted); errors.Is(err, pgx.ErrNoRows) {
		w.Header().Set("Retry-After", "60")
		s.hookAudit(r.Context(), hook, "rate limited", 429)
		http.Error(w, "deploy hook rate limit exceeded", 429)
		return
	} else if err != nil {
		http.Error(w, "rate limit unavailable", 503)
		return
	}
	var config workloadInput
	plain, err := unseal(s.Key, configuration)
	if err != nil || json.Unmarshal(plain, &config) != nil {
		http.Error(w, "configuration unavailable", 503)
		return
	}
	if in.Revision != "" {
		if config.Source == nil {
			http.Error(w, "revision requires a Git source", 400)
			return
		}
		config.Source.Revision = in.Revision
	}
	request := worker.Request{ID: resourceID(), Workload: workload, Action: "deploy", ComposeBundle: config.ComposeBundle, Compose: config.Compose, Source: config.Source, Environment: config.Environment, Kind: config.Kind, JobService: config.JobService, Routes: config.Routes, Private: config.Private, Limits: config.Limits, Hooks: config.Hooks, FileSecrets: config.FileSecrets, GuardedDeploy: config.GuardedDeploy, Rollout: config.Rollout}
	if err = worker.ValidateRequest(request); err != nil {
		http.Error(w, "deployment configuration is invalid", 400)
		return
	}
	payload, err := s.sealPreview(request)
	if err != nil {
		http.Error(w, "could not protect deployment", 500)
		return
	}
	queueDelivery := "hook-" + hook + "-" + in.Delivery
	if _, err = tx.Exec(r.Context(), `INSERT INTO webhook_deliveries(workload_id,delivery,payload) VALUES($1,$2,$3)`, workload, queueDelivery, payload); err != nil {
		http.Error(w, "could not queue deployment", 503)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO deploy_hook_receipts(hook_id,delivery,request_id,queue_delivery,reason) VALUES($1,$2,$3,$4,$5)`, hook, in.Delivery, request.ID, queueDelivery, in.Reason); err != nil {
		http.Error(w, "could not save delivery receipt", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not queue deployment", 503)
		return
	}
	s.hookAudit(r.Context(), hook, "accepted delivery", 202)
	respond(w, 202, map[string]string{"operation_id": request.ID, "delivery": in.Delivery})
}
