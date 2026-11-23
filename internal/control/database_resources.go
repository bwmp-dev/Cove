package control

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"cove/internal/catalog"
	"cove/internal/worker"
	"github.com/jackc/pgx/v5"
)

type databaseResource struct {
	PoolerHost        string                `json:"pooler_host,omitempty"`
	PrivateCompose    string                `json:"private_compose,omitempty"`
	Variant           string                `json:"variant,omitempty"`
	Extensions        []string              `json:"extensions,omitempty"`
	PublicAccess      *databasePublicAccess `json:"public_access,omitempty"`
	PrivateHost       string                `json:"private_host,omitempty"`
	Pooler            bool                  `json:"pooler"`
	SettingsOperation string                `json:"settings_operation,omitempty"`
	Host              string                `json:"host,omitempty"`
	Engine            string                `json:"engine"`
	Version           string                `json:"version"`
	TemplateRevision  string                `json:"template_revision"`
	Service           string                `json:"service,omitempty"`
	Image             string                `json:"image,omitempty"`
	Port              int                   `json:"port,omitempty"`
}

func databaseMetadata(database *databaseResource) *databaseResource {
	if database == nil {
		return nil
	}
	metadata := *database
	metadata.PrivateCompose = ""
	metadata.PoolerHost = ""
	if metadata.Pooler {
		metadata.PoolerHost = databasePoolHostname(database)
	}
	return &metadata
}

type databaseEngine struct {
	id, name, template, service, password, scheme, user, database string
	port                                                          int
	logical                                                       bool
}

var databaseEngines = []databaseEngine{
	{"postgresql", "PostgreSQL", "postgres", "postgres", "POSTGRES_PASSWORD", "postgresql", "app", "app", 5432, true},
	{"mysql", "MySQL", "mysql", "mysql", "DATABASE_PASSWORD", "mysql", "app", "app", 3306, false},
	{"mariadb", "MariaDB", "mariadb", "mariadb", "DATABASE_PASSWORD", "mysql", "app", "app", 3306, true},
	{"mongodb", "MongoDB", "mongodb", "mongo", "MONGO_PASSWORD", "mongodb", "app", "app", 27017, false},
	{"redis", "Redis", "redis", "redis", "CACHE_PASSWORD", "redis", "default", "0", 6379, true},
	{"valkey", "Valkey", "valkey", "valkey", "CACHE_PASSWORD", "redis", "default", "0", 6379, true},
	{"clickhouse", "ClickHouse", "clickhouse", "clickhouse", "DATABASE_PASSWORD", "clickhouse", "app", "app", 9000, false},
}

var databaseExportKeys = []string{"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME", "DB_URL"}
var errDatabaseTemplateChanged = errors.New("database template changed; select its current pinned version")

func findDatabaseEngine(id string) (databaseEngine, bool) {
	for _, engine := range databaseEngines {
		if engine.id == id {
			return engine, true
		}
	}
	return databaseEngine{}, false
}

func databaseTemplate(engine databaseEngine) (catalog.Template, string, error) {
	index, revision, err := catalog.Builtin()
	if err != nil {
		return catalog.Template{}, "", err
	}
	for _, definition := range index.Templates {
		if definition.ID == engine.template {
			return definition, revision, nil
		}
	}
	return catalog.Template{}, "", errors.New("database template unavailable")
}

func databaseImageVersion(image string) string {
	image, _, _ = strings.Cut(image, "@")
	_, version, _ := strings.Cut(image[strings.LastIndex(image, "/")+1:], ":")
	return version
}

func selectedDatabaseTemplate(engine databaseEngine, version string) (catalog.Template, string, error) {
	t, revision, err := databaseTemplate(engine)
	if err != nil {
		return t, revision, err
	}
	if version == databaseImageVersion(t.Images[engine.service]) {
		return t, revision, nil
	}
	for _, option := range databaseAdditionalVersions(engine.id) {
		if option.Version == version {
			t.Compose = strings.ReplaceAll(t.Compose, t.Images[engine.service], option.Image)
			if version == "17.12-alpine" {
				t.Compose = strings.ReplaceAll(t.Compose, "data:/var/lib/postgresql\n", "data:/var/lib/postgresql/data\n")
			}
			t.Images = map[string]string{engine.service: option.Image}
			return t, revision, nil
		}
	}
	return t, revision, errDatabaseTemplateChanged
}

func (s *Server) databaseEngineOptions(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, engine := range databaseEngines {
		template, revision, err := databaseTemplate(engine)
		if err != nil {
			http.Error(w, "database catalogue unavailable", 503)
			return
		}
		image := template.Images[engine.service]
		versions := []string{databaseImageVersion(image)}
		for _, option := range databaseAdditionalVersions(engine.id) {
			versions = append(versions, option.Version)
		}
		out = append(out, map[string]any{"engine": engine.id, "name": engine.name, "version": databaseImageVersion(image), "versions": versions, "version_details": databaseAdditionalVersions(engine.id), "image": image, "service": engine.service, "port": engine.port, "template_id": engine.template, "template_revision": revision, "logical_backup": engine.logical, "minimum": template.Minimum, "storage": template.Storage, "backup": template.Backup, "docs": template.Docs})
	}
	respond(w, 200, map[string]any{"engines": out})
}

func prepareDatabase(in *workloadInput, id string) error {
	if in.Database == nil {
		return errors.New("select a database engine and pinned version")
	}
	engine, ok := findDatabaseEngine(in.Database.Engine)
	if !ok {
		return errors.New("unsupported database engine")
	}
	if in.Compose != "" || len(in.Environment) > 0 || in.Source != nil || len(in.Routes) > 0 || in.JobService != "" || in.Hooks != nil || len(in.FileSecrets) > 0 || in.Database.Image != "" || in.Database.Service != "" || in.Database.Port != 0 || in.Database.Host != "" {
		return errors.New("database configuration and credentials are generated from the built-in template")
	}
	template, revision, err := selectedDatabaseTemplate(engine, in.Database.Version)
	if err != nil {
		return err
	}
	image := template.Images[engine.service]
	if in.Database.TemplateRevision != revision {
		return errDatabaseTemplateChanged
	}
	draft, err := catalog.Draft(template, nil, "", false)
	if err != nil {
		return err
	}
	version := in.Database.Version
	in.Database = &databaseResource{Engine: engine.id, Version: version, TemplateRevision: revision, Image: image, Service: engine.service, Port: engine.port, Host: databaseHostname(id)}
	for _, option := range databaseAdditionalVersions(engine.id) {
		if option.Version == version {
			in.Database.Variant = option.Variant
			if option.Variant == "postgis" {
				in.Database.Extensions = []string{"postgis", "postgis_topology", "postgis_tiger_geocoder", "fuzzystrmatch"}
			}
		}
	}
	in.Compose, in.Environment, in.Kind = draft.Compose, draft.Environment, "service"
	return nil
}

func resourceKind(config workloadInput) string {
	if config.Database != nil {
		return "database"
	}
	return config.Kind
}

func databaseHostname(id string) string { return "db-" + id + ".internal" }

func databaseExports(config workloadInput, resolve func(string) (string, error)) (map[string]string, error) {
	if config.Database == nil {
		return nil, errors.New("resource is not a typed database")
	}
	engine, ok := findDatabaseEngine(config.Database.Engine)
	if !ok {
		return nil, errors.New("unsupported database engine")
	}
	password, ok := config.Environment[engine.password]
	if !ok || password == "" {
		return nil, errors.New("database credentials are unavailable")
	}
	var err error
	if resolve != nil {
		password, err = resolve(password)
		if err != nil {
			return nil, err
		}
	}
	host := config.Database.Host
	u := &url.URL{Scheme: engine.scheme, Host: net.JoinHostPort(host, strconv.Itoa(engine.port)), User: url.UserPassword(engine.user, password), Path: "/" + engine.database}
	if engine.id == "mongodb" {
		u.RawQuery = "authSource=admin"
	}
	if config.Database.PublicAccess != nil {
		query := u.Query()
		ca := "/run/secrets/" + databaseCAFile(config.Database)
		switch engine.id {
		case "postgresql":
			query.Set("sslmode", "verify-full")
			query.Set("sslrootcert", ca)
		case "mysql", "mariadb":
			query.Set("ssl-mode", "VERIFY_IDENTITY")
			query.Set("ssl-ca", ca)
		case "mongodb":
			query.Set("tls", "true")
			query.Set("tlsCAFile", ca)
		}
		u.RawQuery = query.Encode()
	}
	values := map[string]string{"DB_HOST": host, "DB_PORT": strconv.Itoa(engine.port), "DB_USER": engine.user, "DB_PASSWORD": password, "DB_NAME": engine.database, "DB_URL": u.String()}
	directURL := *u
	if config.Database.Pooler {
		for _, key := range databaseExportKeys {
			values[strings.Replace(key, "DB_", "DB_POOL_", 1)] = values[key]
		}
		poolHost := databasePoolHostname(config.Database)
		u.RawQuery = ""
		u.Host = net.JoinHostPort(poolHost, "6432")
		values["DB_POOL_HOST"], values["DB_POOL_PORT"], values["DB_POOL_URL"] = poolHost, "6432", u.String()
	}
	if public := config.Database.PublicAccess; public != nil {
		publicURL := directURL
		publicURL.Host = net.JoinHostPort(public.Hostname, strconv.Itoa(public.Port))
		if engine.id == "redis" || engine.id == "valkey" {
			publicURL.Scheme = "rediss"
		}
		if engine.id == "clickhouse" {
			publicURL.Scheme = "clickhouses"
		}
		values["DB_PUBLIC_HOST"], values["DB_PUBLIC_PORT"], values["DB_PUBLIC_URL"] = public.Hostname, strconv.Itoa(public.Port), publicURL.String()
		values["DB_TLS_CA"] = config.Environment["COVE_DB_TLS_CA"]
	}
	return values, nil
}

func validateDatabaseResource(config workloadInput) error {
	if config.Database == nil {
		return nil
	}
	d := config.Database
	engine, ok := findDatabaseEngine(d.Engine)
	if config.ComposeBundle != nil || config.Autoscale != nil || config.Rollout != nil {
		return errors.New("managed databases do not support Compose overrides, replica rollouts or autoscaling")
	}
	if !ok || config.Kind != "service" || config.Source != nil || d.Service != engine.service || d.Port != engine.port || d.Version == "" || !worker.ValidLinkAlias(d.Host) || d.PublicAccess == nil && !strings.HasPrefix(d.Host, "db-") || d.PublicAccess != nil && d.Host != d.PublicAccess.Hostname {
		return errors.New("invalid managed database metadata")
	}
	if !databasePinnedImage(d) {
		return errors.New("unsupported pinned database image")
	}
	if _, err := databaseExports(config, nil); err != nil {
		return err
	}
	return nil
}

func (s *Server) databaseDetails(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var encrypted []byte
	var deployed bool
	if err := s.DB.QueryRow(r.Context(), "SELECT configuration,NOT manually_stopped AND EXISTS(SELECT 1 FROM operations WHERE workload_id=w.id AND state='succeeded' AND action IN ('deploy','rollback')) FROM workloads w WHERE id=$1 AND team_id=$2", id, actor(r).Team).Scan(&encrypted, &deployed); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	var config workloadInput
	if err := s.unsealJSON(encrypted, &config); err != nil {
		http.Error(w, "database configuration unavailable", 503)
		return
	}
	if config.Database == nil {
		http.NotFound(w, r)
		return
	}
	engine, ok := findDatabaseEngine(config.Database.Engine)
	if !ok {
		http.Error(w, "database configuration unavailable", 503)
		return
	}
	if r.Method == "GET" {
		exports := []map[string]any{}
		for _, key := range databaseExportKeysFor(config.Database) {
			exports = append(exports, map[string]any{"key": key, "secret": databaseExportSecret(key)})
		}
		if !strings.HasSuffix(r.URL.Path, "/credentials") {
			dependents, hidden, err := s.databaseDependents(r, id)
			if err != nil {
				http.Error(w, "database dependents unavailable", 503)
				return
			}
			state := "succeeded"
			if config.Database.SettingsOperation != "" {
				if err = s.DB.QueryRow(r.Context(), "SELECT state FROM operations WHERE id=$1 AND workload_id=$2", config.Database.SettingsOperation, id).Scan(&state); err != nil {
					if !errors.Is(err, pgx.ErrNoRows) {
						http.Error(w, "database settings status unavailable", 503)
						return
					}
					state = "unknown"
				}
			}
			canCutover, err := s.canCutoverDatabase(r, id, id)
			if err != nil {
				http.Error(w, "database permissions unavailable", 503)
				return
			}
			respond(w, 200, map[string]any{"can_cutover": canCutover, "settings_state": state, "dependents": dependents, "hidden_dependents": hidden, "copy_targets": databaseCopyTargets(config.Database), "extension_options": []string{"pg_trgm", "hstore", "uuid-ossp", "pg_stat_statements", "vector", "postgis"}, "revision": databaseRevision(encrypted), "maintenance_locked": s.databaseLocked(r.Context(), id), "maintenance_available": deployed, "database": databaseMetadata(config.Database), "host": config.Database.Host, "user": engine.user, "name": engine.database, "exports": exports, "logical_backup": engine.logical, "connection_required": true, "tls_ca": config.Environment["COVE_DB_TLS_CA"], "admin_tools": databaseAdminTools(engine.id)})
			return
		}
	}
	if !s.requirePermission(w, r, "workload", id, "read_secrets") || !s.requireAccess(w, r, "workload", id, 3) {
		return
	}
	if s.databaseLocked(r.Context(), id) {
		http.Error(w, "database maintenance must finish before reading credentials", 409)
		return
	}
	resolved, err := s.resolveSharedVariables(r.Context(), id, config.Environment)
	if err != nil {
		http.Error(w, "database credentials could not be resolved", 409)
		return
	}
	config.Environment = resolved
	values, err := databaseExports(config, nil)
	if err != nil {
		http.Error(w, "database credentials unavailable", 409)
		return
	}
	if _, err = s.DB.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status,team_id) VALUES($1,'read database credentials',$2,200,$3)", actor(r).ID, id, actor(r).Team); err != nil {
		http.Error(w, "audit unavailable", 503)
		return
	}
	credentials := map[string]any{"host": values["DB_HOST"], "port": engine.port, "user": values["DB_USER"], "password": values["DB_PASSWORD"], "database": values["DB_NAME"], "url": values["DB_URL"], "ca_certificate": config.Environment["COVE_DB_TLS_CA"]}
	if config.Database.Pooler {
		credentials["pooler"] = map[string]any{"host": values["DB_POOL_HOST"], "port": 6432, "user": values["DB_POOL_USER"], "password": values["DB_POOL_PASSWORD"], "database": values["DB_POOL_NAME"], "url": values["DB_POOL_URL"]}
	}
	if config.Database.PublicAccess != nil {
		credentials["public"] = map[string]any{"host": values["DB_PUBLIC_HOST"], "port": config.Database.PublicAccess.Port, "user": values["DB_USER"], "password": values["DB_PASSWORD"], "database": values["DB_NAME"], "url": values["DB_PUBLIC_URL"]}
	}
	respond(w, 200, credentials)
}

func databaseExportSecret(key string) bool {
	return strings.HasSuffix(key, "_PASSWORD") || strings.HasSuffix(key, "_URL")
}

func databaseExportKey(key string) bool {
	if key == "DB_PUBLIC_HOST" || key == "DB_PUBLIC_PORT" || key == "DB_PUBLIC_URL" || key == "DB_TLS_CA" {
		return true
	}
	key = strings.Replace(key, "DB_POOL_", "DB_", 1)
	return slices.Contains(databaseExportKeys, key)
}

func databaseExportKeysFor(database *databaseResource) []string {
	keys := slices.Clone(databaseExportKeys)
	if database.Pooler {
		for _, key := range databaseExportKeys {
			keys = append(keys, strings.Replace(key, "DB_", "DB_POOL_", 1))
		}
	}
	if database.PublicAccess != nil {
		keys = append(keys, "DB_PUBLIC_HOST", "DB_PUBLIC_PORT", "DB_PUBLIC_URL", "DB_TLS_CA")
	}
	return keys
}

func databasePoolHostname(database *databaseResource) string {
	host := database.PrivateHost
	if host == "" {
		host = database.Host
	}
	return "pool-" + strings.TrimPrefix(host, "db-")
}

func databaseAdminTools(engine string) []string {
	return worker.DatabaseAdminTools(engine)
}

func safeDatabaseJSON(raw json.RawMessage) string {
	var config *databaseResource
	if len(raw) == 0 {
		return ""
	}
	if json.Unmarshal(raw, &config) != nil {
		return `"[redacted]"`
	}
	data, err := json.Marshal(databaseMetadata(config))
	if err != nil {
		return `"[redacted]"`
	}
	return string(data)
}
