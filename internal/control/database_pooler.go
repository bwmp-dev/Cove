package control

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"cove/internal/worker"
	"go.yaml.in/yaml/v3"
)

const databasePoolerImage = "pgbouncer/pgbouncer:1.25.2@sha256:c0c55b277858ca308eb5df7baf8b83fbaea60b2c666604f29d8fe25e9decac68"

func databaseConnectionAllowed(database *databaseResource, service string, port int, alias string) bool {
	if database == nil {
		return true
	}
	if database.Pooler && service == "pgbouncer" && port == 6432 && alias == databasePoolHostname(database) {
		return true
	}
	return service == database.Service && alias == database.Host && (port == database.Port || database.Engine == "clickhouse" && port == 8123)
}

func configureDatabasePooler(config *workloadInput, enabled bool) error {
	if config.Database == nil || config.Database.Engine != "postgresql" {
		return errors.New("pooling requires PostgreSQL")
	}
	var compose map[string]any
	if err := yaml.Unmarshal([]byte(config.Compose), &compose); err != nil {
		return err
	}
	services, ok := compose["services"].(map[string]any)
	if !ok {
		return errors.New("database services unavailable")
	}
	config.FileSecrets = slices.DeleteFunc(config.FileSecrets, func(secret worker.FileSecret) bool { return secret.Name == "cove_pooler_users" })
	for i := range config.FileSecrets {
		if config.FileSecrets[i].Name == "cove_db_ca" {
			config.FileSecrets[i].Services = slices.DeleteFunc(config.FileSecrets[i].Services, func(service string) bool { return service == "pgbouncer" })
			if enabled && config.Database.PublicAccess != nil {
				config.FileSecrets[i].Services = append(config.FileSecrets[i].Services, "pgbouncer")
			}
		}
	}
	delete(config.Environment, "COVE_POOLER_AUTH")
	delete(services, "pgbouncer")
	if !enabled {
		delete(config.Limits, "pgbouncer")
	}
	if enabled {
		if config.Limits == nil {
			config.Limits = map[string]worker.ServiceLimits{}
		}
		if _, exists := config.Limits["pgbouncer"]; !exists {
			config.Limits["pgbouncer"] = worker.ServiceLimits{CPUs: 0.25, MemoryMB: 128}
		}
		config.Environment["COVE_POOLER_AUTH"] = "\"app\" \"" + config.Environment["POSTGRES_PASSWORD"] + "\"\n"
		config.FileSecrets = append(config.FileSecrets, worker.FileSecret{Name: "cove_pooler_users", Variable: "COVE_POOLER_AUTH", Services: []string{"pgbouncer"}})
		services["pgbouncer"] = map[string]any{
			"image": databasePoolerImage, "restart": "unless-stopped",
			"depends_on": map[string]any{config.Database.Service: map[string]any{"condition": "service_healthy"}},
			"environment": map[string]string{
				"DATABASES_HOST": config.Database.Service, "DATABASES_PORT": "5432", "DATABASES_USER": "app", "DATABASES_DBNAME": "app", "DATABASES_PASSWORD": "${POSTGRES_PASSWORD:?Set POSTGRES_PASSWORD}",
				"PGBOUNCER_LISTEN_ADDR": "0.0.0.0", "PGBOUNCER_LISTEN_PORT": "6432", "PGBOUNCER_POOL_MODE": "transaction", "PGBOUNCER_AUTH_TYPE": "scram-sha-256", "PGBOUNCER_AUTH_FILE": "/run/secrets/cove_pooler_users",
				"PGBOUNCER_ADMIN_USERS": "", "PGBOUNCER_STATS_USERS": "", "PGBOUNCER_MAX_CLIENT_CONN": "100", "PGBOUNCER_DEFAULT_POOL_SIZE": "20", "PGBOUNCER_SERVER_TLS_SSLMODE": "prefer", "QUIET": "1",
			},
			"healthcheck": map[string]any{"test": []string{"CMD-SHELL", "nc -z 127.0.0.1 6432"}, "interval": "5s", "timeout": "3s", "retries": 12},
		}
		if config.Database.PublicAccess != nil {
			environment := services["pgbouncer"].(map[string]any)["environment"].(map[string]string)
			environment["DATABASES_HOST"] = config.Database.Host
			environment["PGBOUNCER_SERVER_TLS_SSLMODE"] = "verify-full"
			environment["PGBOUNCER_SERVER_TLS_CA_FILE"] = "/run/secrets/cove_db_ca"
		}
	}
	encoded, err := yaml.Marshal(compose)
	if err != nil {
		return err
	}
	config.Compose, config.Database.Pooler = string(encoded), enabled
	return nil
}

func (s *Server) databaseSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		databasePublicInput
		Enabled  *bool  `json:"enabled"`
		Confirm  bool   `json:"confirm"`
		Revision string `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Enabled == nil || !in.Confirm {
		http.Error(w, "select and confirm the database settings change and its client reconnection", 400)
		return
	}
	id := r.PathValue("id")
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var raw []byte
	var node string
	var capable bool
	if err = tx.QueryRow(r.Context(), `SELECT w.configuration,w.node_id,COALESCE((n.capabilities->>'managed_databases')::boolean,false) FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.id=$1 AND w.team_id=$2 FOR UPDATE OF w`, id, actor(r).Team).Scan(&raw, &node, &capable); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if !capable || databaseRevision(raw) != in.Revision {
		http.Error(w, "verify an upgraded worker and review the current database configuration", 409)
		return
	}
	var busy bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running')) OR EXISTS(SELECT 1 FROM database_maintenance WHERE (state IN ('pending','failed') AND workload_id=$1) OR (state='pending' AND source_id=$1))`, id).Scan(&busy); err != nil || busy {
		http.Error(w, "finish existing database operations before changing database settings", 409)
		return
	}
	var config workloadInput
	if s.unsealJSON(raw, &config) != nil {
		http.Error(w, "database configuration unavailable", 503)
		return
	}
	public := strings.HasSuffix(r.URL.Path, "/public-access")
	if config.Database == nil {
		http.Error(w, "select a managed database", 400)
		return
	}
	if !public && !*in.Enabled {
		var used bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM resource_connections WHERE target_id=$1 AND target_service='pgbouncer' AND NOT removing) OR EXISTS(SELECT 1 FROM resource_variable_bindings WHERE source_id=$1 AND starts_with(source_key,'DB_POOL_'))`, id).Scan(&used); err != nil || used {
			http.Error(w, "remove pooler connections and bindings before disabling it", 409)
			return
		}
	}
	oldHost := config.Database.Host
	reason, audit := "Database pooler configuration", "configure database pooler"
	if public {
		err = configureDatabasePublic(&config, *in.Enabled, in.databasePublicInput)
		reason, audit = "Database public TLS access", "configure database public TLS access"
	} else {
		err = configureDatabasePooler(&config, *in.Enabled)
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if oldHost != config.Database.Host {
		if _, err = tx.Exec(r.Context(), `UPDATE resource_connections SET alias=$3,version=version+1,updated_at=now() WHERE target_id=$1 AND target_service=$4 AND alias=$2`, id, oldHost, config.Database.Host, config.Database.Service); err != nil {
			http.Error(w, "private connection hostname conflicts with the requested TLS hostname", 409)
			return
		}
	}
	if err = validateWorkload(config); err != nil {
		http.Error(w, "database settings could not be validated", 400)
		return
	}
	task := resourceID()
	config.Database.SettingsOperation = task
	raw, err = s.sealJSON(config)
	if err != nil {
		http.Error(w, "could not protect database settings", 503)
		return
	}
	request := worker.Request{ID: task, Workload: id, Action: "deploy", Kind: "service", Compose: config.Compose, Environment: config.Environment, FileSecrets: config.FileSecrets, Limits: config.Limits}
	payload, err := s.sealJSON(request)
	if err != nil {
		http.Error(w, "could not protect the configuration operation", 503)
		return
	}
	cpu, memory := configuredUsage(config)
	if _, err = tx.Exec(r.Context(), "UPDATE workloads SET configuration=$2,manually_stopped=false,configured_cpu=$3,configured_memory_mb=$4 WHERE id=$1", id, raw, cpu, memory); quotaError(w, err) {
		return
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO operations(id,workload_id,action,payload,reason) VALUES($1,$2,'deploy',$3,$4)", task, id, payload, reason)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,$4,$2,202,$3)", actor(r).ID, id, actor(r).Team, audit)
	}
	if err != nil || tx.Commit(r.Context()) != nil {
		http.Error(w, "could not queue database settings", 503)
		return
	}
	respond(w, 202, map[string]string{"id": task, "resource_id": id})
}
