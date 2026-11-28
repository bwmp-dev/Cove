package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
)

func DatabaseExtension(name string) bool {
	switch name {
	case "pg_trgm", "hstore", "uuid-ossp", "pg_stat_statements", "vector", "postgis":
		return true
	}
	return false
}

type DatabaseInspectionRequest struct {
	DisableExtension string `json:"disable_extension,omitempty"`
	Workload         string `json:"workload"`
	Engine           string `json:"engine"`
	Service          string `json:"service"`
}

type DatabaseInspection struct {
	Version              string   `json:"version"`
	Extensions           []string `json:"extensions"`
	FeatureCompatibility string   `json:"feature_compatibility,omitempty"`
	Arch                 string   `json:"arch"`
}

func (e *Engine) InspectDatabase(ctx context.Context, in DatabaseInspectionRequest) (*DatabaseInspection, error) {
	services := map[string]string{"postgresql": "postgres", "mysql": "mysql", "mariadb": "mariadb", "mongodb": "mongo"}
	if !workloadIdentifier.MatchString(in.Workload) || services[in.Engine] != in.Service || in.Service == "" {
		return nil, errors.New("unsupported database inspection")
	}
	container, err := e.databaseService(ctx, in.Workload, in.Service)
	if err != nil {
		return nil, err
	}
	var command string
	switch in.Engine {
	case "postgresql":
		command = postgresTLSClient + `PGPASSWORD="$POSTGRES_PASSWORD" psql -h "${COVE_DB_PUBLIC_HOST:-$HOSTNAME}" -U app -d app -w -At -v ON_ERROR_STOP=1 -c "SELECT json_build_object('version',current_setting('server_version'),'extensions',(SELECT json_agg(extname ORDER BY extname) FROM pg_extension))"`
	case "mysql":
		command = databaseSQLClientArguments(in.Engine) + `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -u root -sN -e "SELECT JSON_OBJECT('version',VERSION(),'extensions',JSON_ARRAY())"`
	case "mariadb":
		command = `MYSQL_PWD="$MARIADB_ROOT_PASSWORD" mariadb -u root -sN -e "SELECT JSON_OBJECT('version',VERSION(),'extensions',JSON_ARRAY())"`
	case "mongodb":
		command = mongoTLSClient + `mongosh "$@" --quiet --eval 'const a=db.getSiblingDB("admin");if(a.auth("app",process.env.MONGO_INITDB_ROOT_PASSWORD).ok!==1)quit(1);print(JSON.stringify({version:db.version(),extensions:[],feature_compatibility:a.runCommand({getParameter:1,featureCompatibilityVersion:1}).featureCompatibilityVersion.version}))'`
	}
	output, err := e.output(ctx, "docker", "exec", container, "sh", "-ec", command)
	if err != nil {
		return nil, errors.New("database compatibility inspection failed")
	}
	var result DatabaseInspection
	if json.Unmarshal([]byte(output), &result) != nil || result.Version == "" {
		return nil, errors.New("database compatibility response invalid")
	}
	arch, err := e.output(ctx, "docker", "info", "--format", "{{.Architecture}}")
	if err != nil {
		return nil, errors.New("worker architecture unavailable")
	}
	result.Arch = arch
	if in.DisableExtension != "" {
		if in.Engine != "postgresql" || !DatabaseExtension(in.DisableExtension) {
			return nil, errors.New("unsupported extension inspection")
		}
		statement := databaseExtensionStatement(in.DisableExtension, true)
		command := postgresTLSClient + `PGPASSWORD="$POSTGRES_PASSWORD" psql -h "${COVE_DB_PUBLIC_HOST:-$HOSTNAME}" -U app -d app -w -v ON_ERROR_STOP=1 -c 'BEGIN; ` + statement + `; ROLLBACK;'`
		if e.Run(ctx, e.Root, []string{"docker", "exec", container, "sh", "-ec", command}, nil, io.Discard) != nil {
			return nil, errors.New("extension has dependent objects; no change was made")
		}
	}
	return &result, nil
}

func databaseExtensionStatement(name string, disable bool) string {
	if !disable {
		return `CREATE EXTENSION IF NOT EXISTS "` + name + `"`
	}
	if name == "postgis" {
		return `DROP EXTENSION IF EXISTS postgis_tiger_geocoder RESTRICT; DROP EXTENSION IF EXISTS postgis_topology RESTRICT; DROP EXTENSION IF EXISTS fuzzystrmatch RESTRICT; DROP EXTENSION IF EXISTS postgis RESTRICT`
	}
	return `DROP EXTENSION IF EXISTS "` + name + `" RESTRICT`
}
