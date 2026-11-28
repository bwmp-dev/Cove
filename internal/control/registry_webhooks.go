package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type registryWebhookStatus struct {
	OwnershipRequired bool                          `json:"ownership_required"`
	Configuration     *registryWebhookConfiguration `json:"configuration"`
	Endpoint          *string                       `json:"endpoint"`
	Registry          map[string]string             `json:"registry"`
	Matches           bool                          `json:"matches_saved_registry"`
	Supported         bool                          `json:"worker_supported"`
	Received          *time.Time                    `json:"last_received_at"`
	Matched           *time.Time                    `json:"last_matched_at"`
	Pending           *time.Time                    `json:"pending_at"`
	LastOperation     *registryWebhookOperation     `json:"last_operation"`
	LastError         string                        `json:"last_error"`
	Ignored           string                        `json:"last_ignored_reason"`
	Token             string                        `json:"token,omitempty"`
}

type registryWebhookOperation struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) connectedWebhookRegistry(ctx context.Context, tx pgx.Tx, workload string) (string, string, string, error) {
	var id, name string
	var encrypted []byte
	err := tx.QueryRow(ctx, `SELECT c.id,c.name,c.configuration FROM infrastructure_connections c JOIN workload_connections b ON b.connection_id=c.id JOIN workloads w ON w.id=b.workload_id WHERE b.workload_id=$1 AND b.kind='registry' AND c.kind='registry' AND c.team_id=w.team_id FOR SHARE OF c`, workload).Scan(&id, &name, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", nil
	}
	if err != nil {
		return "", "", "", err
	}
	data, err := unseal(s.Key, encrypted)
	var config connectionConfig
	if err != nil || json.Unmarshal(data, &config) != nil || config.Registry == nil {
		return "", "", "", errors.New("connected registry configuration is unavailable")
	}
	return id, name, config.Registry.Server, nil
}

func (s *Server) readRegistryWebhook(ctx context.Context, workload string) (registryWebhookStatus, error) {
	status := registryWebhookStatus{}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return status, err
	}
	defer tx.Rollback(ctx)
	if err = tx.QueryRow(ctx, `SELECT w.kind='service' AND COALESCE((n.capabilities->>'image_lifecycle')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1`, workload).Scan(&status.Supported); err != nil {
		return status, err
	}
	registry, name, server, err := s.connectedWebhookRegistry(ctx, tx, workload)
	if err != nil {
		return status, err
	}
	if registry != "" {
		status.Registry = map[string]string{"id": registry, "name": name, "server": server}
	}
	var id, operation string
	var filters []byte
	var config registryWebhookConfiguration
	err = tx.QueryRow(ctx, `SELECT id,registry_id,registry_server,provider,enabled,filters,last_received_at,last_matched_at,pending_at,COALESCE(last_operation_id,''),last_error,last_ignored_reason FROM registry_webhooks WHERE workload_id=$1`, workload).Scan(&id, &config.RegistryID, &config.RegistryServer, &config.Provider, &config.Enabled, &filters, &status.Received, &status.Matched, &status.Pending, &operation, &status.LastError, &status.Ignored)
	if errors.Is(err, pgx.ErrNoRows) {
		return status, nil
	}
	if err != nil || json.Unmarshal(filters, &config.Filters) != nil || validateRegistryWebhook(config) != nil {
		return status, errors.New("registry webhook configuration is unavailable")
	}
	endpoint := strings.TrimRight(s.PublicURL, "/") + "/hooks/registry/" + id
	status.Configuration, status.Endpoint = &config, &endpoint
	if err := tx.QueryRow(ctx, `SELECT NOT (cove_automation_allowed(h.creator_id,w.team_id,w.project_id,COALESCE(w.environment_id,''),'operate') AND cove_automation_allowed(h.creator_id,w.team_id,w.project_id,COALESCE(w.environment_id,''),'edit_configuration')) FROM registry_webhooks h JOIN workloads w ON w.id=h.workload_id WHERE h.workload_id=$1`, workload).Scan(&status.OwnershipRequired); err != nil {
		return status, err
	}
	status.Matches = registry == config.RegistryID && server == config.RegistryServer
	if operation != "" {
		var last registryWebhookOperation
		if err := tx.QueryRow(ctx, "SELECT id,state,created_at FROM operations WHERE id=$1 AND workload_id=$2", operation, workload).Scan(&last.ID, &last.State, &last.CreatedAt); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return status, err
		} else if err == nil {
			status.LastOperation = &last
		}
	}
	return status, nil
}

type registryWebhookInput struct {
	Provider string                  `json:"provider"`
	Enabled  *bool                   `json:"enabled"`
	Filters  []registryWebhookFilter `json:"filters"`
}

func (s *Server) registryWebhook(w http.ResponseWriter, r *http.Request) {
	workload := r.PathValue("id")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == "GET" || r.Method == "HEAD" {
		status, err := s.readRegistryWebhook(r.Context(), workload)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
		} else if err != nil {
			http.Error(w, "registry webhook unavailable", 503)
		} else {
			respond(w, 200, status)
		}
		return
	}
	var input registryWebhookInput
	if r.Method == "PUT" && !decode(w, r, &input) {
		return
	}
	if r.Method != "DELETE" && (r.Method == "POST" || input.Enabled != nil && *input.Enabled) && !s.requirePermission(w, r, "workload", workload, "operate") {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "registry webhook unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var supported, preview bool
	var configuration []byte
	err = tx.QueryRow(r.Context(), `SELECT COALESCE((n.capabilities->>'image_lifecycle')::boolean,false),w.kind<>'service' OR EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id) OR EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id),w.configuration FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 FOR UPDATE OF w`, workload).Scan(&supported, &preview, &configuration)
	if err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	var id, previousRegistry, previousServer, previousProvider string
	err = tx.QueryRow(r.Context(), `SELECT id,registry_id,registry_server,provider FROM registry_webhooks WHERE workload_id=$1 FOR UPDATE`, workload).Scan(&id, &previousRegistry, &previousServer, &previousProvider)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "registry webhook unavailable", 503)
		return
	}
	cancelOwned := r.Method == "DELETE" || r.Method == "PUT" && input.Enabled != nil && !*input.Enabled
	if _, err := tx.Exec(r.Context(), `UPDATE operations o SET cancel_requested=true,updated_at=now() WHERE o.workload_id=$1 AND o.state IN ('queued','running') AND EXISTS(SELECT 1 FROM registry_webhook_runs j WHERE j.operation_id=o.id) AND ($2 OR EXISTS(SELECT 1 FROM registry_webhooks h WHERE h.workload_id=$1 AND h.creator_id IS DISTINCT FROM NULLIF($3,'')))`, workload, cancelOwned, actor(r).ID); err != nil {
		http.Error(w, "could not revoke previous registry deployments", 503)
		return
	}
	if r.Method == "DELETE" {
		if _, err = tx.Exec(r.Context(), "DELETE FROM registry_webhooks WHERE workload_id=$1", workload); err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			http.Error(w, "could not remove registry webhook", 503)
		} else {
			respond(w, 200, map[string]bool{"ok": true})
		}
		return
	}
	registry, _, server, err := s.connectedWebhookRegistry(r.Context(), tx, workload)
	if err != nil {
		http.Error(w, "connected registry unavailable", 503)
		return
	}
	if preview || registry == "" {
		http.Error(w, "connect a team registry to a service outside managed previews first", 409)
		return
	}
	secret, statusCode := "", 200
	if r.Method == "POST" {
		if id == "" || registry != previousRegistry || server != previousServer {
			http.Error(w, "save the current registry selection before rotating its webhook token", 409)
			return
		}
		secret = randomToken()
		encrypted, err := seal(s.Key, []byte(secret))
		if err == nil {
			_, err = tx.Exec(r.Context(), "UPDATE registry_webhooks SET secret=$2,creator_id=NULLIF($3,''),pending_at=NULL,updated_at=now() WHERE workload_id=$1", workload, encrypted, actor(r).ID)
		}
		if err != nil {
			http.Error(w, "could not rotate registry webhook token", 503)
			return
		}
	} else {
		if input.Enabled == nil {
			http.Error(w, "enabled is required", 400)
			return
		}
		config := registryWebhookConfiguration{Provider: input.Provider, Enabled: *input.Enabled, RegistryID: registry, RegistryServer: server, Filters: input.Filters}
		if err := validateRegistryWebhook(config); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if config.Enabled && !supported {
			http.Error(w, "upgrade and verify the worker before enabling registry redeployments", 409)
			return
		}
		if config.Provider == "ghcr" && server != "ghcr.io" || config.Provider == "docker_hub" && server != "docker.io" && server != "index.docker.io" {
			http.Error(w, "select the registry matching this webhook provider", 409)
			return
		}
		if _, err := s.registryWebhookDeployment(workload, configuration); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		filters, _ := json.Marshal(config.Filters)
		created := id == ""
		if created {
			id, secret, statusCode = resourceID(), randomToken(), 201
		} else if registry != previousRegistry || server != previousServer || config.Provider != previousProvider {
			secret = randomToken()
		}
		var encrypted []byte
		if secret != "" {
			encrypted, err = seal(s.Key, []byte(secret))
			if err != nil {
				http.Error(w, "could not protect registry webhook token", 503)
				return
			}
		}
		if created {
			_, err = tx.Exec(r.Context(), `INSERT INTO registry_webhooks(workload_id,id,registry_id,registry_server,enabled,filters,secret,provider,creator_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''))`, workload, id, registry, server, config.Enabled, filters, encrypted, config.Provider, actor(r).ID)
		} else {
			_, err = tx.Exec(r.Context(), `UPDATE registry_webhooks SET registry_id=$2,registry_server=$3,enabled=$4,filters=$5,secret=COALESCE($6,secret),provider=$7,creator_id=NULLIF($9,''),pending_at=CASE WHEN NOT $4 OR $8 OR creator_id IS DISTINCT FROM NULLIF($9,'') THEN NULL ELSE pending_at END,last_error='',last_ignored_reason='',updated_at=now() WHERE workload_id=$1`, workload, registry, server, config.Enabled, filters, encrypted, config.Provider, secret != "", actor(r).ID)
		}
		if err != nil {
			http.Error(w, "could not save registry webhook", 503)
			return
		}
		if !created && secret != "" {
			// A different provider or registry has its own event identity namespace and a new credential.
			if _, err = tx.Exec(r.Context(), "DELETE FROM registry_webhook_deliveries WHERE webhook_id=$1", id); err != nil {
				http.Error(w, "could not reset registry event identities", 503)
				return
			}
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save registry webhook", 503)
		return
	}
	status, err := s.readRegistryWebhook(r.Context(), workload)
	if err != nil {
		http.Error(w, "saved registry webhook status is unavailable; rotate the token if its response was lost", 503)
		return
	}
	status.Token = secret
	w.Header().Set("Cache-Control", "no-store")
	respond(w, statusCode, status)
}

func (s *Server) receiveRegistryWebhook(w http.ResponseWriter, r *http.Request) {
	hook := r.PathValue("hook")
	if _, err := hex.DecodeString(hook); err != nil || len(hook) != 32 {
		http.Error(w, "invalid registry webhook credentials", 401)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256*1024))
	if err != nil {
		http.Error(w, "invalid registry notification body", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "registry notification unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var workload string
	err = tx.QueryRow(r.Context(), `SELECT w.id FROM workloads w JOIN registry_webhooks h ON h.workload_id=w.id WHERE h.id=$1 FOR UPDATE OF w`, hook).Scan(&workload)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "invalid registry webhook credentials", 401)
		return
	}
	if err != nil {
		http.Error(w, "registry notification unavailable", 503)
		return
	}
	var config registryWebhookConfiguration
	var encrypted, filters []byte
	err = tx.QueryRow(r.Context(), `SELECT registry_id,registry_server,provider,enabled,filters,secret FROM registry_webhooks WHERE id=$1 FOR UPDATE`, hook).Scan(&config.RegistryID, &config.RegistryServer, &config.Provider, &config.Enabled, &filters, &encrypted)
	if err != nil {
		http.Error(w, "registry notification unavailable", 503)
		return
	}
	secret, err := unseal(s.Key, encrypted)
	if err != nil {
		http.Error(w, "registry notification unavailable", 503)
		return
	}
	if !registryWebhookAuthenticated(config.Provider, secret, body, r) {
		http.Error(w, "invalid registry webhook credentials", 401)
		return
	}
	if json.Unmarshal(filters, &config.Filters) != nil || validateRegistryWebhook(config) != nil {
		http.Error(w, "registry notification configuration unavailable", 503)
		return
	}
	events, err := registryProviderEvents(config.Provider, body, r, time.Now())
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	registry, _, server, err := s.connectedWebhookRegistry(r.Context(), tx, workload)
	if err != nil {
		http.Error(w, "connected registry unavailable", 503)
		return
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM registry_webhook_deliveries WHERE webhook_id=$1 AND received_at<now()-interval '7 days'`, hook); err != nil {
		http.Error(w, "registry delivery tracking unavailable", 503)
		return
	}
	added := 0
	matching := config
	matching.Enabled = true
	if registry == config.RegistryID && server == config.RegistryServer {
		for _, event := range events {
			if !registryPushMatches(matching, event) {
				continue
			}
			image := sha256.Sum256([]byte(event.Repository + "\x00" + event.Tag))
			imageKey := hex.EncodeToString(image[:])
			result, err := tx.Exec(r.Context(), `INSERT INTO registry_webhook_deliveries(webhook_id,event_key,source_key,image_key) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, hook, event.Key, event.SourceKey, imageKey)
			if err != nil {
				http.Error(w, "registry delivery tracking unavailable", 503)
				return
			}
			if result.RowsAffected() == 0 {
				var conflicting bool
				if err := tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM registry_webhook_deliveries WHERE webhook_id=$1 AND (event_key=$2 OR source_key=$3) AND (image_key<>$4 OR (event_key=$2 AND source_key<>$3)))", hook, event.Key, event.SourceKey, imageKey).Scan(&conflicting); err != nil {
					http.Error(w, "registry delivery tracking unavailable", 503)
					return
				}
				if conflicting {
					http.Error(w, "registry event identity was reused for a different image", 409)
					return
				}
			}
			added += int(result.RowsAffected())
		}
	}
	var count int
	if err = tx.QueryRow(r.Context(), "SELECT count(*) FROM registry_webhook_deliveries WHERE webhook_id=$1", hook).Scan(&count); err != nil {
		http.Error(w, "registry delivery tracking unavailable", 503)
		return
	}
	if count > 4096 {
		w.Header().Set("Retry-After", "3600")
		http.Error(w, "registry delivery tracking is full; retry later", 429)
		return
	}
	requested := added > 0 && config.Enabled
	ignored := ""
	if requested {
		ignored, err = registryWebhookStopReason(r.Context(), tx, workload)
		if err != nil {
			http.Error(w, "resource state unavailable", 503)
			return
		}
		requested = ignored == ""
	}
	if !config.Enabled {
		ignored = "Webhook disabled."
	} else if registry != config.RegistryID || server != config.RegistryServer {
		ignored = "Registry selection changed; save webhook setup again."
	}
	if requested {
		if _, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor,action,subject,status) VALUES('registry webhook','pull and redeploy requested',$1,202)`, workload); err != nil {
			http.Error(w, "could not record image check request", 503)
			return
		}
	}
	if _, err = tx.Exec(r.Context(), "UPDATE registry_webhooks SET last_received_at=now(),last_matched_at=CASE WHEN $2 THEN now() ELSE last_matched_at END,pending_at=CASE WHEN $3 THEN now() ELSE pending_at END,next_dispatch_at=CASE WHEN $3 THEN now() ELSE next_dispatch_at END,last_ignored_reason=$4 WHERE id=$1", hook, added > 0, requested, ignored); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save registry notification", 503)
		return
	}
	respond(w, 202, map[string]any{"status": "accepted", "new_events": added, "deploy_requested": requested})
}
