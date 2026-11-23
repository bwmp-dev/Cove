package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type DatabaseMaintenance struct {
	ID               string  `json:"id"`
	Kind             string  `json:"kind"`
	Engine           string  `json:"engine"`
	Service          string  `json:"service"`
	PasswordKey      string  `json:"password_key"`
	Extension        string  `json:"extension,omitempty"`
	DisableExtension bool    `json:"disable_extension,omitempty"`
	TargetVariant    string  `json:"target_variant,omitempty"`
	SourceMajor      string  `json:"source_major,omitempty"`
	TargetMajor      string  `json:"target_major,omitempty"`
	Source           string  `json:"source,omitempty"`
	PreviousPassword string  `json:"previous_password,omitempty"`
	Request          Request `json:"request"`
}

type databaseMaintenanceJournal struct {
	Digest   string `json:"digest"`
	Applied  bool   `json:"applied"`
	Complete bool   `json:"complete"`
}

var databaseSecret = regexp.MustCompile(`^[a-f0-9]{64}$`)

const postgresTLSClient = `if test -n "${COVE_DB_PUBLIC_HOST:-}"; then export PGSSLMODE=verify-full PGSSLROOTCERT=/run/secrets/cove_db_ca; fi; `

func databaseSQLClientArguments(engine string) string {
	flags := "--ssl-mode=VERIFY_IDENTITY"
	if engine == "mariadb" {
		flags = "--ssl --ssl-verify-server-cert"
	}
	return `set -- -h 127.0.0.1; if test -n "${COVE_DB_PUBLIC_HOST:-}"; then set -- -h "$COVE_DB_PUBLIC_HOST" ` + flags + ` --ssl-ca=/run/secrets/cove_db_ca; fi; `
}

const mongoTLSClient = `set --; if test -n "${COVE_DB_PUBLIC_HOST:-}"; then set -- --tls --tlsCAFile /run/secrets/cove_db_ca --host "$COVE_DB_PUBLIC_HOST"; fi; `

func ValidateDatabaseMaintenance(in DatabaseMaintenance) error {
	if !identifier.MatchString(in.ID) || in.Request.ID != in.ID || !workloadIdentifier.MatchString(in.Request.Workload) || !serviceIdentifier.MatchString(in.Service) || in.Request.Action != "deploy" || in.Request.Source != nil || len(in.Request.Routes) > 0 || in.Request.Private != nil || in.Request.Kind == "job" {
		return errors.New("invalid database maintenance request")
	}
	if err := ValidateRequest(in.Request); err != nil {
		return err
	}
	switch in.Kind {
	case "rotate":
		if !databaseSecret.MatchString(in.PreviousPassword) || !databaseSecret.MatchString(in.Request.Environment[in.PasswordKey]) {
			return errors.New("rotation requires generated credentials")
		}
		keys := map[string]string{"postgresql": "POSTGRES_PASSWORD", "mysql": "DATABASE_PASSWORD", "mariadb": "DATABASE_PASSWORD", "mongodb": "MONGO_PASSWORD", "redis": "CACHE_PASSWORD", "valkey": "CACHE_PASSWORD", "clickhouse": "DATABASE_PASSWORD"}
		if keys[in.Engine] != in.PasswordKey || in.PasswordKey == "" {
			return errors.New("unsupported database rotation")
		}
	case "extension":
		if in.Engine != "postgresql" || !DatabaseExtension(in.Extension) {
			return errors.New("unsupported PostgreSQL extension")
		}
	case "upgrade":
		if !strings.Contains("|postgresql|mysql|mariadb|mongodb|", "|"+in.Engine+"|") || in.Engine == "" || !workloadIdentifier.MatchString(in.Source) || in.Source == in.Request.Workload {
			return errors.New("upgrades require a separate supported database resource")
		}
	default:
		return errors.New("unsupported database maintenance")
	}
	return nil
}

func (e *Engine) databaseService(ctx context.Context, workload, service string) (string, error) {
	ids, err := e.output(ctx, "docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload, "--filter", "label=com.docker.compose.service="+service)
	if err != nil || len(strings.Fields(ids)) != 1 {
		return "", errors.New("database service must have exactly one running container")
	}
	return strings.TrimSpace(ids), nil
}

// The durable phase marker makes a lost SSH response retry the same credentials and copy.
func (e *Engine) MaintainDatabase(ctx context.Context, in DatabaseMaintenance) error {
	if err := ValidateDatabaseMaintenance(in); err != nil {
		return err
	}
	e.databaseMu.Lock()
	defer e.databaseMu.Unlock()
	e.mu.Lock()
	for _, op := range e.pending {
		if op.Workload == in.Request.Workload || op.Workload == in.Source {
			e.mu.Unlock()
			return errors.New("wait for queued database operations")
		}
	}
	busy := in.Source != "" && e.active[in.Source]
	e.mu.Unlock()
	if busy {
		return errors.New("wait for active database operations")
	}
	dir := filepath.Join(e.Root, "database-maintenance")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, in.ID+".json")
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	journal := databaseMaintenanceJournal{Digest: digest}
	if saved, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(saved, &journal) != nil || journal.Digest != digest {
			return errors.New("maintenance input differs from retained journal")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if journal.Complete {
		return removeDatabaseDump(dir, in.ID)
	}
	if err := writeJSON(path, journal); err != nil {
		return err
	}
	if !journal.Applied {
		if in.Kind == "upgrade" {
			if err := e.upgradeDatabase(ctx, in, dir); err != nil {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
				defer cancel()
				stop := in.Request
				stop.Action = "stop"
				stop.DatabaseMaintenance = nil
				if stopErr := e.perform(cleanup, stop); stopErr != nil {
					return errors.New("copy failed; source is intact, but target shutdown needs retry")
				}
				return fmt.Errorf("database copy failed; source is intact and target stopped; retry the retained copy: %w", err)
			}
		} else {
			container, err := e.databaseService(ctx, in.Request.Workload, in.Service)
			if err != nil {
				return err
			}
			if in.Kind == "extension" {
				if in.Extension == "pg_stat_statements" && !in.DisableExtension {
					if err = e.perform(ctx, in.Request); err != nil {
						return errors.New("could not configure PostgreSQL extension preload")
					}
					container, err = e.databaseService(ctx, in.Request.Workload, in.Service)
					if err != nil {
						return err
					}
				}
				statement := "BEGIN; " + databaseExtensionStatement(in.Extension, in.DisableExtension) + "; COMMIT;"
				command := postgresTLSClient + `PGPASSWORD="$POSTGRES_PASSWORD" psql -h "${COVE_DB_PUBLIC_HOST:-$HOSTNAME}" -U app -d app -v ON_ERROR_STOP=1 -c '` + statement + `'`
				if e.Run(ctx, e.Root, []string{"docker", "exec", container, "sh", "-ec", command}, nil, io.Discard) != nil {
					return errors.New("could not change PostgreSQL extension; dependent objects must be removed explicitly before disabling")
				}
			} else if err := e.rotateDatabase(ctx, in, container); err != nil {
				return err
			}
		}
		journal.Applied = true
		if err := writeJSON(path, journal); err != nil {
			return err
		}
	}
	if in.Kind == "rotate" || in.Kind == "extension" && in.Extension == "pg_stat_statements" && in.DisableExtension {
		if err := e.perform(ctx, in.Request); err != nil {
			return errors.New("database changed; retry maintenance to confirm its deployment")
		}
		if err := e.checkDatabasePassword(ctx, in); err != nil {
			return err
		}
	}
	journal.Complete = true
	if err := writeJSON(path, journal); err != nil {
		return err
	}
	return removeDatabaseDump(dir, in.ID)
}

func (e *Engine) rotateDatabase(ctx context.Context, in DatabaseMaintenance, container string) error {
	next := in.Request.Environment[in.PasswordKey]
	// Secrets are passed in the child environment, never command arguments or operation logs.
	env := []string{"COVE_NEXT=" + next, "COVE_PREVIOUS=" + in.PreviousPassword}
	prefix := []string{"docker", "exec", "-e", "COVE_NEXT", "-e", "COVE_PREVIOUS", container, "sh", "-ec"}
	var command string
	switch in.Engine {
	case "postgresql":
		// The image trusts loopback connections; its bridge hostname exercises password authentication.
		command = postgresTLSClient + `export PGPASSWORD="$COVE_NEXT"; psql -h "${COVE_DB_PUBLIC_HOST:-$HOSTNAME}" -U app -d app -w -c 'SELECT 1' >/dev/null 2>&1 && exit 0; export PGPASSWORD="$COVE_PREVIOUS"; printf "ALTER ROLE app PASSWORD '%s';\n" "$COVE_NEXT" | psql -h "${COVE_DB_PUBLIC_HOST:-$HOSTNAME}" -U app -d app -w -v ON_ERROR_STOP=1`
	case "mysql", "mariadb":
		client, root := "mysql", "MYSQL_ROOT_PASSWORD"
		if in.Engine == "mariadb" {
			client, root = "mariadb", "MARIADB_ROOT_PASSWORD"
		}
		command = databaseSQLClientArguments(in.Engine) + `export MYSQL_PWD="$COVE_NEXT"; ` + client + ` "$@" -u app -e 'SELECT 1' >/dev/null 2>&1 && exit 0; export MYSQL_PWD="$` + root + `"; printf "ALTER USER 'app'@'%%' IDENTIFIED BY '%s';\n" "$COVE_NEXT" | ` + client + ` -u root`
	case "mongodb":
		command = mongoTLSClient + `mongosh "$@" --quiet --eval 'const a=db.getSiblingDB("admin");try{if(a.auth("app",process.env.COVE_NEXT).ok===1)quit(0)}catch(e){};if(a.auth("app",process.env.COVE_PREVIOUS).ok!==1)quit(1);a.changeUserPassword("app",process.env.COVE_NEXT)'`
	case "redis", "valkey":
		client := "redis-cli"
		if in.Engine == "valkey" {
			client = "valkey-cli"
		}
		command = `export REDISCLI_AUTH="$COVE_NEXT"; ` + client + ` --no-auth-warning ping 2>/dev/null | grep -qx PONG && exit 0; export REDISCLI_AUTH="$COVE_PREVIOUS"; ` + client + ` --no-auth-warning CONFIG SET requirepass "$COVE_NEXT" | grep -qx OK`
	case "clickhouse":
		// The official image regenerates users.d/default-user.xml from its environment on recreation.
		return nil
	}
	if command == "" || e.Run(ctx, e.Root, append(prefix, command), env, io.Discard) != nil {
		return errors.New("database credential change was not confirmed; retry the same maintenance")
	}
	return nil
}

func (e *Engine) checkDatabasePassword(ctx context.Context, in DatabaseMaintenance) error {
	container, err := e.databaseService(ctx, in.Request.Workload, in.Service)
	if err != nil {
		return err
	}
	var command string
	switch in.Engine {
	case "postgresql":
		command = postgresTLSClient + `PGPASSWORD="$COVE_NEXT" psql -h "${COVE_DB_PUBLIC_HOST:-$HOSTNAME}" -U app -d app -w -v ON_ERROR_STOP=1 -c 'SELECT 1'`
	case "mysql":
		command = databaseSQLClientArguments(in.Engine) + `MYSQL_PWD="$COVE_NEXT" mysql "$@" -u app -e 'SELECT 1'`
	case "mariadb":
		command = databaseSQLClientArguments(in.Engine) + `MYSQL_PWD="$COVE_NEXT" mariadb "$@" -u app -e 'SELECT 1'`
	case "mongodb":
		command = mongoTLSClient + `mongosh "$@" --quiet --eval 'const a=db.getSiblingDB("admin");if(a.auth("app",process.env.COVE_NEXT).ok!==1)quit(1);if(a.runCommand({ping:1}).ok!==1)quit(1)'`
	case "redis":
		command = `REDISCLI_AUTH="$COVE_NEXT" redis-cli --no-auth-warning ping | grep -qx PONG`
	case "valkey":
		command = `REDISCLI_AUTH="$COVE_NEXT" valkey-cli --no-auth-warning ping | grep -qx PONG`
	case "clickhouse":
		command = `clickhouse-client --host 127.0.0.1 --user app --password "$COVE_NEXT" --query 'SELECT 1'`
	}
	if command == "" || e.Run(ctx, e.Root, []string{"docker", "exec", "-e", "COVE_NEXT", container, "sh", "-ec", command}, []string{"COVE_NEXT=" + in.Request.Environment[in.PasswordKey]}, io.Discard) != nil {
		return errors.New("new database credentials were not accepted; retry maintenance")
	}
	return nil
}

func (e *Engine) upgradeDatabase(ctx context.Context, in DatabaseMaintenance, dir string) error {
	source, err := e.databaseService(ctx, in.Source, in.Service)
	if err != nil {
		return err
	}
	actual, err := e.InspectDatabase(ctx, DatabaseInspectionRequest{Workload: in.Source, Engine: in.Engine, Service: in.Service})
	if err != nil {
		return err
	}
	if in.SourceMajor != "" && !strings.HasPrefix(actual.Version, in.SourceMajor+".") {
		return errors.New("source version changed")
	}
	if in.Engine == "mongodb" && actual.FeatureCompatibility != "7.0" {
		return errors.New("MongoDB source FCV must be 7.0")
	}
	if in.TargetVariant == "postgis" && actual.Arch != "x86_64" && actual.Arch != "amd64" {
		return errors.New("PostGIS image requires amd64")
	}
	dump := filepath.Join(dir, in.ID+".dump")
	if _, err = os.Stat(dump); os.IsNotExist(err) {
		if err = e.dumpUpgradeDatabase(ctx, source, in, dump+".partial"); err != nil {
			return fmt.Errorf("safety snapshot failed: %w", err)
		}
		if err = os.Rename(dump+".partial", dump); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err = verifyDatabaseSnapshot(dump); err != nil {
		return err
	}
	if err = e.perform(ctx, in.Request); err != nil {
		return fmt.Errorf("target deployment failed: %w", err)
	}
	target, err := e.databaseService(ctx, in.Request.Workload, in.Service)
	if err != nil {
		return err
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "cp", dump, target + ":/tmp/cove-upgrade.dump"}, nil, io.Discard); err != nil {
		return err
	}
	if err = e.restoreDatabaseCopy(ctx, in, target, dump); err != nil {
		return fmt.Errorf("archive import failed: %w", err)
	}
	return e.checkDatabasePassword(ctx, in)
}

func removeDatabaseDump(dir, id string) error {
	for _, suffix := range []string{".dump.sha256", ".dump.partial", ".dump"} {
		if err := os.Remove(filepath.Join(dir, id+suffix)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return syncDirectory(dir)
}

func (e *Engine) dumpUpgradeDatabase(ctx context.Context, source string, in DatabaseMaintenance, destination string) (result error) {
	containerFile := "/tmp/cove-upgrade-" + in.ID + ".dump"
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		result = errors.Join(result, e.Run(cleanup, e.Root, []string{"docker", "exec", source, "rm", "-f", containerFile}, nil, io.Discard))
	}()
	command, err := databaseCopyDumpCommand(in.Engine)
	if err != nil {
		return err
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "exec", source, "sh", "-ec", command, "cove", containerFile}, nil, io.Discard); err != nil {
		return err
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "cp", source + ":" + containerFile, destination}, nil, io.Discard); err != nil {
		return err
	}
	if err := os.Chmod(destination, 0600); err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	return sealDatabaseSnapshot(destination, strings.TrimSuffix(destination, ".partial")+".sha256")
}
