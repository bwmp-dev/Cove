package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
)

type notificationConfig struct {
	Owner            bool     `json:"owner,omitempty"`
	Token            string   `json:"token,omitempty"`
	TokenSet         bool     `json:"token_set,omitempty"`
	Target           string   `json:"target,omitempty"`
	UserKey          string   `json:"user_key,omitempty"`
	UserKeySet       bool     `json:"user_key_set,omitempty"`
	Events           []string `json:"events,omitempty"`
	Email            string   `json:"email,omitempty"`
	SigningSecret    string   `json:"signing_secret,omitempty"`
	SigningSecretSet bool     `json:"signing_secret_set,omitempty"`
	Health           bool     `json:"health"`
	Provider         string   `json:"provider"`
	URL              string   `json:"url"`
	Enabled          bool     `json:"enabled"`
	Success          bool     `json:"success"`
	Failure          bool     `json:"failure"`
}

type connectionConfig struct {
	Name         string                  `json:"name"`
	Registry     *worker.Registry        `json:"registry,omitempty"`
	Storage      *worker.StorageLocation `json:"storage,omitempty"`
	Notification *notificationConfig     `json:"notification,omitempty"`
}

func connectionKind(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/api/registries") {
		return "registry"
	}
	if strings.HasPrefix(r.URL.Path, "/api/storage-locations") {
		return "storage"
	}
	return "notification"
}

func validateConnection(kind string, c connectionConfig) error {
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 120 {
		return errors.New("a name of at most 120 characters is required")
	}
	switch kind {
	case "registry":
		if c.Registry == nil || c.Storage != nil || c.Notification != nil {
			return errors.New("registry configuration required")
		}
		return worker.ValidateRegistry(c.Registry)
	case "storage":
		if c.Storage == nil || c.Registry != nil || c.Notification != nil {
			return errors.New("storage configuration required")
		}
		return worker.ValidateStorage(c.Storage)
	case "notification":
		if c.Notification == nil || c.Registry != nil || c.Storage != nil {
			return errors.New("notification configuration required")
		}
		n := c.Notification
		if err := validateNotificationRouting(n); err != nil {
			return err
		}
		if n.Provider == "email" {
			if _, ok := normalizeEmail(n.Email); !ok && !n.Owner {
				return errors.New("a valid recipient email is required")
			}
			return nil
		}
		if n.Provider == "telegram" || n.Provider == "pushover" || n.Provider == "pagerduty" {
			return nil
		}
		u, err := url.Parse(n.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || len(n.URL) > 4096 || strings.ContainsAny(n.URL, "\r\n") {
			return errors.New("an HTTPS webhook URL is required")
		}
		if slices.Contains([]string{"gotify", "matrix", "opsgenie", "ntfy"}, n.Provider) && u.RawQuery != "" {
			return errors.New("provider base URLs cannot contain a query")
		}
		if !slices.Contains([]string{"webhook", "discord", "slack", "ntfy", "gotify", "matrix", "teams", "opsgenie"}, n.Provider) {
			return errors.New("select a supported notification provider")
		}
	}
	return nil
}

func redactConnection(c connectionConfig) connectionConfig {
	if c.Registry != nil {
		v := *c.Registry
		v.Password = ""
		c.Registry = &v
	}
	if c.Storage != nil {
		v := *c.Storage
		v.Password = ""
		v.SecretKey = ""
		c.Storage = &v
	}
	if c.Notification != nil {
		v := *c.Notification
		v.URL = ""
		v.SigningSecretSet = v.SigningSecret != ""
		v.SigningSecret = ""
		v.TokenSet = v.Token != ""
		v.Token = ""
		v.UserKeySet = v.UserKey != ""
		v.UserKey = ""
		c.Notification = &v
	}
	return c
}

func (s *Server) connections(w http.ResponseWriter, r *http.Request) {
	kind := connectionKind(r)
	team := actor(r).Team
	id := r.PathValue("id")
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), `SELECT id,configuration,CASE WHEN EXISTS(SELECT 1 FROM object_buckets b WHERE b.storage_id=c.id) THEN 'Storage on the same node is not offsite. Keep an independent backup in another failure domain.' ELSE '' END FROM infrastructure_connections c WHERE team_id=$1 AND kind=$2 ORDER BY name`, team, kind)
		if err != nil {
			http.Error(w, "connections unavailable", 503)
			return
		}
		defer rows.Close()
		type item struct {
			ID      string `json:"id"`
			Warning string `json:"warning,omitempty"`
			connectionConfig
		}
		items := []item{}
		for rows.Next() {
			var v item
			var enc []byte
			if rows.Scan(&v.ID, &enc, &v.Warning) != nil {
				http.Error(w, "connections unavailable", 503)
				return
			}
			data, err := unseal(s.Key, enc)
			if err != nil || json.Unmarshal(data, &v.connectionConfig) != nil {
				http.Error(w, "connection configuration unavailable", 500)
				return
			}
			v.connectionConfig = redactConnection(v.connectionConfig)
			items = append(items, v)
		}
		if rows.Err() != nil {
			http.Error(w, "connections unavailable", 503)
			return
		}
		respond(w, 200, items)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var old connectionConfig
	if id != "" {
		var encrypted []byte
		err = tx.QueryRow(r.Context(), `SELECT configuration FROM infrastructure_connections WHERE id=$1 AND team_id=$2 AND kind=$3 FOR UPDATE`, id, team, kind).Scan(&encrypted)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "connection unavailable", 503)
			return
		}
		raw, e := unseal(s.Key, encrypted)
		if e != nil || json.Unmarshal(raw, &old) != nil {
			http.Error(w, "connection unavailable", 500)
			return
		}
	}
	if r.Method == "DELETE" {
		_, err = tx.Exec(r.Context(), "DELETE FROM infrastructure_connections WHERE id=$1", id)
		if err != nil {
			groupError(w, err)
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			http.Error(w, "could not delete connection", 503)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	var in connectionConfig
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if old.Registry != nil && in.Registry != nil && old.Registry.Server == in.Registry.Server && old.Registry.Username == in.Registry.Username && in.Registry.Password == "" {
		in.Registry.Password = old.Registry.Password
	}
	if old.Storage != nil && in.Storage != nil {
		a, b := *old.Storage, *in.Storage
		a.Password = ""
		a.SecretKey = ""
		b.Password = ""
		b.SecretKey = ""
		if a == b {
			if in.Storage.Password == "" {
				in.Storage.Password = old.Storage.Password
			}
			if in.Storage.SecretKey == "" {
				in.Storage.SecretKey = old.Storage.SecretKey
			}
		} else {
			var used bool
			if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM workload_connections WHERE connection_id=$1)", id).Scan(&used); err != nil {
				http.Error(w, "database unavailable", 503)
				return
			}
			if used {
				http.Error(w, "create a new location to change the destination of an in-use storage location", 409)
				return
			}
		}
	}
	if old.Notification != nil && in.Notification != nil && old.Notification.Provider == in.Notification.Provider && in.Notification.URL == "" {
		in.Notification.URL = old.Notification.URL
	}
	if old.Notification != nil && in.Notification != nil && old.Notification.Provider == in.Notification.Provider && in.Notification.SigningSecret == "" {
		in.Notification.SigningSecret = old.Notification.SigningSecret
	}
	if old.Notification != nil && in.Notification != nil && old.Notification.Provider == in.Notification.Provider {
		if in.Notification.Token == "" {
			in.Notification.Token = old.Notification.Token
		}
		if in.Notification.UserKey == "" {
			in.Notification.UserKey = old.Notification.UserKey
		}
	}
	if err = validateConnection(kind, in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	raw, err := json.Marshal(in)
	if err != nil {
		http.Error(w, "invalid configuration", 400)
		return
	}
	encrypted, err := seal(s.Key, raw)
	if err != nil {
		http.Error(w, "could not protect credentials", 500)
		return
	}
	status := 200
	if id == "" {
		id = resourceID()
		status = 201
		_, err = tx.Exec(r.Context(), `INSERT INTO infrastructure_connections(id,team_id,kind,name,configuration) VALUES($1,$2,$3,$4,$5)`, id, team, kind, in.Name, encrypted)
	} else {
		_, err = tx.Exec(r.Context(), `UPDATE infrastructure_connections SET name=$2,configuration=$3 WHERE id=$1`, id, in.Name, encrypted)
	}
	if err != nil {
		groupError(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save connection", 503)
		return
	}
	respond(w, status, map[string]string{"id": id})
}

func (s *Server) readConnection(ctx context.Context, id, team, kind string) (connectionConfig, error) {
	var c connectionConfig
	var encrypted []byte
	err := s.DB.QueryRow(ctx, `SELECT configuration FROM infrastructure_connections WHERE id=$1 AND team_id=$2 AND kind=$3`, id, team, kind).Scan(&encrypted)
	if err != nil {
		return c, err
	}
	data, err := unseal(s.Key, encrypted)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(data, &c)
	return c, err
}

func (s *Server) testConnection(w http.ResponseWriter, r *http.Request) {
	kind := connectionKind(r)
	if kind == "storage" {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Minute))
	}
	c, err := s.readConnection(r.Context(), r.PathValue("id"), actor(r).Team, kind)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "connection unavailable", 503)
		return
	}
	if kind == "notification" {
		if c.Notification.Owner {
			if err = s.DB.QueryRow(r.Context(), `SELECT email FROM users WHERE id=$1 AND email_verified AND NOT disabled AND email IS NOT NULL`, actor(r).ID).Scan(&c.Notification.Email); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					http.Error(w, "verify your account email before testing owner delivery", 409)
				} else {
					http.Error(w, "account unavailable", 503)
				}
				return
			}
		}
		if err = s.sendNotification(r.Context(), c.Notification, map[string]string{"event": "test", "message": "Cove notification connection test"}); err != nil {
			http.Error(w, "notification test failed; check the channel configuration and delivery permissions", 502)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	var in struct {
		Node string `json:"node_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	var allowed bool
	err = s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM nodes n JOIN team_nodes t ON t.node_id=n.id WHERE n.id=$1 AND t.team_id=$2 AND n.state='ready' AND COALESCE((n.capabilities->>'infrastructure')::boolean,false) AND (NOT $3::boolean OR COALESCE((n.capabilities->>'backblaze_b2')::boolean,false)))`, in.Node, actor(r).Team, kind == "storage" && c.Storage.Protocol == "b2").Scan(&allowed)
	if err != nil || !allowed {
		http.Error(w, "select a ready team node with an upgraded worker", 409)
		return
	}
	_, err = s.nodeRPC(r.Context(), in.Node, worker.RPCRequest{Method: kind + "_test", Registry: c.Registry, Storage: c.Storage})
	if err != nil {
		http.Error(w, "connection test failed; check credentials, endpoint, and worker connectivity", 502)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) resourceRegistry(w http.ResponseWriter, r *http.Request) {
	workload := r.PathValue("id")
	if r.Method == "GET" {
		var id string
		err := s.DB.QueryRow(r.Context(), `SELECT connection_id FROM workload_connections WHERE workload_id=$1 AND kind='registry'`, workload).Scan(&id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "registry unavailable", 503)
			return
		}
		respond(w, 200, map[string]string{"registry_id": id})
		return
	}
	var in struct {
		ID string `json:"registry_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var team string
	if err = tx.QueryRow(r.Context(), "SELECT team_id FROM workloads WHERE id=$1 FOR UPDATE", workload).Scan(&team); err != nil {
		http.NotFound(w, r)
		return
	}
	var active bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))", workload).Scan(&active); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if active {
		http.Error(w, "wait for the active operation before changing registry", 409)
		return
	}
	if in.ID != "" {
		var found string
		if err = tx.QueryRow(r.Context(), `SELECT id FROM infrastructure_connections WHERE id=$1 AND team_id=$2 AND kind='registry' FOR SHARE`, in.ID, team).Scan(&found); err != nil {
			http.Error(w, "registry not available to this team", 403)
			return
		}
	}
	_, err = tx.Exec(r.Context(), "DELETE FROM workload_connections WHERE workload_id=$1 AND kind='registry'", workload)
	if err == nil && in.ID != "" {
		_, err = tx.Exec(r.Context(), `INSERT INTO workload_connections(workload_id,kind,connection_id) VALUES($1,'registry',$2)`, workload, in.ID)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not update registry", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) prepareInfrastructure(ctx context.Context, r *worker.Request) error {
	var driftPrepared bool
	err := s.DB.QueryRow(ctx, `SELECT drift_prepared FROM operations WHERE id=$1`, r.ID).Scan(&driftPrepared)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !driftPrepared {
		if !r.InfrastructurePrepared {
			if err := s.prepareGPUAssignments(ctx, r); err != nil {
				return err
			}
			if err := s.prepareResourceDefaults(ctx, r); err != nil {
				return err
			}
			var encrypted []byte
			if err := s.DB.QueryRow(ctx, "SELECT kind='database',configuration FROM workloads WHERE id=$1", r.Workload).Scan(&r.ManagedDatabase, &encrypted); err != nil {
				return err
			}
			if r.ManagedDatabase {
				if r.DatabaseMaintenance != nil {
					if err := s.DB.QueryRow(ctx, "SELECT configuration FROM database_maintenance WHERE id=$1", r.ID).Scan(&encrypted); err != nil {
						return err
					}
				}
				var config workloadInput
				if err := s.unsealJSON(encrypted, &config); err != nil {
					return err
				}
				metadata := databaseMetadata(config.Database)
				if metadata == nil {
					return errors.New("managed database metadata unavailable")
				}
				var err error
				r.DatabaseMetadata, err = json.Marshal(metadata)
				if err != nil {
					return err
				}
			}
		}
		if !r.InfrastructurePrepared && (r.Action == "stop" || r.Action == "start" || r.Action == "restart") {
			r.Backup = nil
		}
		if !r.InfrastructurePrepared && r.OutboundProxy == nil {
			c, err := s.outboundConfig(ctx)
			if err != nil {
				return err
			}
			if s.propagateOutbound(c) {
				r.OutboundProxy = &c
			}
		}
		// Previously prepared requests may already exist on a worker with a recorded digest.
		if !r.InfrastructurePrepared && r.Action == "deploy" {
			if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'drift')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&r.RecordDrift); err != nil {
				return err
			}
		}
		payload, err := s.sealJSON(r)
		if err != nil {
			return err
		}
		if _, err = s.DB.Exec(ctx, `UPDATE operations SET payload=$2,drift_prepared=true WHERE id=$1`, r.ID, payload); err != nil {
			return err
		}
	}
	if r.Action == "deploy" && r.ComposeBundle != nil {
		var supported bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'compose_bundles')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&supported); err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade and verify the worker before deploying multiple Compose files or profiles")
		}
	}
	if r.Action == "backup" && r.Backup != nil && r.Backup.VolumeSnapshot != nil {
		var capable bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'filesystem_snapshots')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&capable); err != nil {
			return err
		}
		if !capable {
			return errors.New("upgrade and verify this worker before using filesystem snapshots")
		}
	}
	if r.Action == "backup" && r.Backup != nil && (r.Backup.KeepDaily > 0 || r.Backup.KeepWeekly > 0 || r.Backup.KeepMonthly > 0) {
		var capable bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'backup_retention')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&capable); err != nil {
			return err
		}
		if !capable {
			return errors.New("upgrade and verify the worker before applying daily, weekly or monthly retention")
		}
	}
	if r.Backup != nil && r.Backup.Database != nil && (r.Action == "backup" || r.Action == "restore" || r.Action == "drill" || r.Action == "restore_copy") {
		d := r.Backup.Database
		if d.Engine == "mongodb" || d.Engine == "redis" || d.Engine == "valkey" {
			var capable bool
			if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'extended_logical_backups')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&capable); err != nil {
				return err
			}
			if !capable {
				return errors.New("upgrade and verify the worker before using MongoDB, Redis or Valkey backups")
			}
		}
	}
	if err := checkRouteProtocol(ctx, s.DB, r.Workload, r.Routes); err != nil {
		return err
	}
	if r.Rollout != nil && r.Rollout.Strategy == "disabled" {
		r.Rollout = nil
	}
	if r.Rollout != nil || r.Action == "rollout_promote" || r.Action == "rollout_abort" {
		var supported bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'rollouts')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&supported); err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade and verify the worker before using deployment rollouts")
		}
	}
	if r.Action == "build" {
		var supported bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'build_publish')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&supported); err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade and verify the worker before building images")
		}
	}
	if r.ImageUpdates != nil {
		var supported bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'image_updates')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&supported); err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade and verify the worker before checking image updates")
		}
	}
	if r.PullLatest || r.Action == "image_check" {
		var supported bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'image_lifecycle')::boolean,false) AND ($2<>'job' OR COALESCE((n.capabilities->>'job_image_lifecycle')::boolean,false)) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload, r.Kind).Scan(&supported); err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade and verify the worker before using image lifecycle operations")
		}
	}
	if r.Action == "deploy" && worker.HasRoutePolicies(r.Routes) {
		var supported bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'route_policies')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&supported); err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade and verify the worker before applying route policies")
		}
	}
	if r.Action == "deploy" && r.Hooks != nil && (len(r.Hooks.BeforeSteps) > 0 || r.Hooks.Assertions != nil) {
		var supported bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'guarded_hooks')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&supported); err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade and verify the worker before deploying guarded hooks")
		}
	}
	var preview bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM previews WHERE workload_id=$1)`, r.Workload).Scan(&preview); err != nil {
		return err
	}
	if preview {
		r.Preview = true
		if r.Action == "deploy" {
			var supported bool
			if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'previews')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&supported); err != nil {
				return err
			}
			if !supported {
				return errors.New("upgrade and verify the worker before deploying previews")
			}
		}
	}
	if r.EnvironmentPreview == nil {
		var isolation []byte
		err := s.DB.QueryRow(ctx, `SELECT isolation FROM environment_preview_resources WHERE resource_id=$1`, r.Workload).Scan(&isolation)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			// Every operation on a preview resource carries its isolation, whichever path queued it.
			var p worker.EnvironmentPreview
			if err = json.Unmarshal(isolation, &p); err != nil {
				return err
			}
			p.Images = nil
			r.EnvironmentPreview = &p
			r.GuardedDeploy = nil
			sealed, err := s.sealJSON(r)
			if err != nil {
				return err
			}
			if _, err = s.DB.Exec(ctx, "UPDATE operations SET payload=$2,drift_prepared=true WHERE id=$1", r.ID, sealed); err != nil {
				return err
			}
		}
	}
	if r.EnvironmentPreview != nil && r.Action == "deploy" {
		var supported bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'environment_previews')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&supported); err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade and verify the worker before deploying environment previews")
		}
	}
	if r.Action == "deploy" && r.GuardedDeploy != nil && *r.GuardedDeploy {
		var supported bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'guarded_deploy')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&supported); err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade and verify the worker before guarded deployment")
		}
	}
	if r.Action == "deploy" && len(r.FileSecrets) > 0 {
		var supported bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'file_secrets')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&supported); err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade and verify the worker before deploying file secrets")
		}
	}
	if r.Action == "deploy" && len(r.ImagePins) > 0 {
		var supported bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'exact_promotion')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&supported); err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade and verify the worker before promoting images")
		}
	}
	if r.Backup != nil && r.Backup.Database != nil && (r.Action == "backup" || r.Action == "restore" || r.Action == "drill" || r.Action == "restore_copy") {
		d := r.Backup.Database
		if d.Engine == "mongodb" || d.Engine == "redis" || d.Engine == "valkey" {
			var capable bool
			if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'extended_logical_backups')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&capable); err != nil {
				return err
			}
			if !capable {
				return errors.New("upgrade and verify the worker before using MongoDB, Redis or Valkey backups")
			}
		}
	}
	if r.Action == "deploy" && r.Hooks != nil && (r.Hooks.Before != nil || r.Hooks.After != nil) {
		var supported bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'deployment_hooks')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&supported); err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade and verify the worker before deploying hooks")
		}
	}

	if r.Action == "drill" {
		var supported bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'restore_drills')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&supported); err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade and verify the worker before running restore drills")
		}
	}

	if r.Action == "deploy" && len(r.Limits) > 0 {
		var supported bool
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'resource_limits')::boolean,false) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, r.Workload).Scan(&supported); err != nil {
			return err
		}
		if !supported {
			return errors.New("upgrade and verify the worker before deploying resource limits")
		}
	}

	if r.InfrastructurePrepared {
		return nil
	}
	if r.Kind == "job" && r.Action == "deploy" && r.Job == nil {
		options, err := jobExecutionOptions(ctx, s.DB, r.Workload)
		if err != nil {
			return err
		}
		r.Job = options
	}
	var team string
	if err := s.DB.QueryRow(ctx, "SELECT team_id FROM workloads WHERE id=$1", r.Workload).Scan(&team); err != nil {
		return err
	}
	if r.Action == "deploy" || r.Action == "build" {
		if err := s.resolveDeploymentVariables(ctx, r); err != nil {
			return err
		}
		r.DatabaseCAs, err = s.databaseBindingCAs(ctx, r.Workload)
		if err != nil {
			return err
		}
	}
	if (r.Action == "deploy" || r.Action == "image_check" || r.Action == "build") && r.Registry == nil {
		var id string
		err := s.DB.QueryRow(ctx, `SELECT connection_id FROM workload_connections WHERE workload_id=$1 AND kind='registry'`, r.Workload).Scan(&id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if id != "" {
			c, err := s.readConnection(ctx, id, team, "registry")
			if err != nil {
				return err
			}
			r.Registry = c.Registry
		}
	}
	if r.Action == "build" && r.Registry == nil {
		return errors.New("connect a registry before building images")
	}
	if r.Backup != nil && r.Backup.StorageID != "" {
		c, err := s.readConnection(ctx, r.Backup.StorageID, team, "storage")
		if err != nil {
			return err
		}
		r.Backup.Remote = c.Storage
		r.Backup.Repository = "rclone:cove:" + path.Join(c.Storage.Path, r.Workload)
	}
	if r.Action != "deploy" && r.Registry == nil && (r.Backup == nil || r.Backup.StorageID == "") {
		return nil
	}
	r.InfrastructurePrepared = true
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	encrypted, err := seal(s.Key, data)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, "UPDATE operations SET payload=$2,drift_prepared=true WHERE id=$1", r.ID, encrypted)
	return err
}
