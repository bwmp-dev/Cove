package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func sealDatabaseSnapshot(path, checksum string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, file)
	err = errors.Join(copyErr, file.Close())
	if err != nil {
		return err
	}
	if size == 0 {
		return errors.New("database safety snapshot is empty")
	}
	sealed, err := os.OpenFile(checksum, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = sealed.Write([]byte(hex.EncodeToString(hash.Sum(nil))))
	return errors.Join(err, sealed.Sync(), sealed.Close())
}

func verifyDatabaseSnapshot(path string) error {
	expected, err := os.ReadFile(path + ".sha256")
	if err != nil {
		return errors.New("database snapshot checksum unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, file)
	if err = errors.Join(copyErr, file.Close()); err != nil {
		return err
	}
	if size == 0 || string(expected) != hex.EncodeToString(hash.Sum(nil)) {
		return errors.New("database safety snapshot failed integrity verification")
	}
	return nil
}

func databaseCopyDumpCommand(engine string) (string, error) {
	switch engine {
	case "postgresql":
		return postgresTLSClient + `umask 077; PGPASSWORD="$POSTGRES_PASSWORD" pg_dump -h "${COVE_DB_PUBLIC_HOST:-$HOSTNAME}" -U app -d app -Fc --no-owner --no-acl --file="$1"; pg_restore --list "$1" >/dev/null`, nil
	case "mysql":
		return `umask 077; export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; test "$(mysql -u root -sN -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='app' AND table_type='BASE TABLE' AND engine<>'InnoDB'")" = 0; trap 'rm -f "$1.check.sql" "$1.check"' EXIT; mysql -u root -sN -e "SELECT CONCAT('CHECK TABLE app.',CHAR(96),REPLACE(table_name,CHAR(96),CONCAT(CHAR(96),CHAR(96))),CHAR(96),' FOR UPGRADE;') FROM information_schema.tables WHERE table_schema='app' AND table_type='BASE TABLE'" > "$1.check.sql"; mysql -u root -sN < "$1.check.sql" > "$1.check"; awk -F '\t' '$3=="error" || $3=="warning" {bad=1} END {exit bad}' "$1.check"; mysqldump -u root --single-transaction --routines --events --triggers --set-gtid-purged=OFF --no-tablespaces --result-file="$1" app; tail -n 2 "$1" | grep -q 'Dump completed'`, nil
	case "mariadb":
		return `umask 077; export MYSQL_PWD="$MARIADB_ROOT_PASSWORD"; test "$(mariadb -u root -sN -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='app' AND table_type='BASE TABLE' AND engine<>'InnoDB'")" = 0; mariadb-check -u root --check-upgrade app >/dev/null; mariadb-dump -u root --single-transaction --routines --events --triggers --result-file="$1" app; tail -n 2 "$1" | grep -q 'Dump completed'`, nil
	case "mongodb":
		return `COVE_DUMP="$1"; ` + mongoTLSClient + `umask 077; mongodump "$@" --username app --password "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin --db app --archive="$COVE_DUMP" --gzip; mongorestore "$@" --username app --password "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin --nsInclude='app.*' --archive="$COVE_DUMP" --gzip --dryRun --stopOnError`, nil
	}
	return "", errors.New("unsupported database snapshot")
}

func (e *Engine) restoreDatabaseCopy(ctx context.Context, in DatabaseMaintenance, target, dump string) error {
	var command string
	switch in.Engine {
	case "postgresql":
		command = postgresTLSClient + `PGPASSWORD="$POSTGRES_PASSWORD" pg_restore -h "${COVE_DB_PUBLIC_HOST:-$HOSTNAME}" -U app -d app --clean --if-exists --no-owner --no-acl --exit-on-error /tmp/cove-upgrade.dump`
	case "mysql":
		command = `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -u root app < /tmp/cove-upgrade.dump`
	case "mariadb":
		command = `MYSQL_PWD="$MARIADB_ROOT_PASSWORD" mariadb -u root app < /tmp/cove-upgrade.dump`
	case "mongodb":
		if err := e.restoreMongoCopy(ctx, in, target, dump); err != nil {
			return err
		}
	default:
		return errors.New("unsupported database copy")
	}
	if command != "" {
		if err := e.Run(ctx, e.Root, []string{"docker", "exec", target, "sh", "-ec", command}, nil, io.Discard); err != nil {
			return err
		}
	}
	if in.Engine == "postgresql" && in.TargetVariant != "" {
		name := in.TargetVariant
		if name != "vector" && name != "postgis" {
			return errors.New("unsupported PostgreSQL variant")
		}
		command = postgresTLSClient + `PGPASSWORD="$POSTGRES_PASSWORD" psql -h "${COVE_DB_PUBLIC_HOST:-$HOSTNAME}" -U app -d app -v ON_ERROR_STOP=1 -c 'CREATE EXTENSION IF NOT EXISTS "` + name + `"'`
		if err := e.Run(ctx, e.Root, []string{"docker", "exec", target, "sh", "-ec", command}, nil, io.Discard); err != nil {
			return err
		}
	}
	return e.Run(ctx, e.Root, []string{"docker", "exec", target, "rm", "-f", "/tmp/cove-upgrade.dump"}, nil, io.Discard)
}

func (e *Engine) restoreMongoCopy(ctx context.Context, in DatabaseMaintenance, target, dump string) error {
	setFCV := func(version string) error {
		command := `mongosh --quiet --eval 'const a=db.getSiblingDB("admin");if(a.auth("app",process.env.MONGO_INITDB_ROOT_PASSWORD).ok!==1)quit(1);if(a.runCommand({setFeatureCompatibilityVersion:"` + version + `",confirm:true}).ok!==1)quit(1)'`
		return e.Run(ctx, e.Root, []string{"docker", "exec", target, "sh", "-ec", command}, nil, io.Discard)
	}
	if err := setFCV("7.0"); err != nil {
		return fmt.Errorf("target compatibility preparation failed: %w", err)
	}
	image := "mongo:7.0.48-jammy@sha256:e0da75c955723fb6918111a60e3249079e5ea082c70038dcbc2f5fa32e29ab8c"
	command := `mongorestore --host mongo --username app --password "$COVE_NEXT" --authenticationDatabase admin --nsInclude='app.*' --drop --stopOnError --gzip --archive=/run/cove-copy.dump`
	args := []string{"docker", "run", "--rm", "--network", "cove-" + in.Request.Workload + "_default", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "256m", "--cpus", "0.5", "-e", "COVE_NEXT", "--mount", "type=bind,source=" + dump + ",target=/run/cove-copy.dump,readonly", "--entrypoint", "sh", image, "-ec", command}
	if err := e.Run(ctx, e.Root, args, []string{"COVE_NEXT=" + in.Request.Environment[in.PasswordKey]}, io.Discard); err != nil {
		return fmt.Errorf("matching-version archive restore failed: %w", err)
	}
	if err := setFCV("8.0"); err != nil {
		return fmt.Errorf("target compatibility finalization failed: %w", err)
	}
	return nil
}

func (e *Engine) DiscardDatabaseCopy(ctx context.Context, id string) error {
	if !identifier.MatchString(id) {
		return errors.New("invalid copy maintenance ID")
	}
	e.databaseMu.Lock()
	defer e.databaseMu.Unlock()
	j, err := e.read(id)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("copy journal unavailable")
	}
	change := j.Request.DatabaseMaintenance
	if change == nil || change.Kind != "upgrade" || ValidateDatabaseMaintenance(*change) != nil || (j.Operation.State != "failed" && j.Operation.State != "interrupted" && j.Operation.State != "cancelled") {
		return errors.New("only an inactive failed copy can be discarded")
	}
	e.mu.Lock()
	busy := e.busy(change.Request.Workload)
	for _, operation := range e.pending {
		if operation.Workload == change.Request.Workload {
			busy = true
		}
	}
	e.mu.Unlock()
	if busy {
		return errors.New("wait for active copy operations before discarding")
	}
	stop := change.Request
	stop.Action = "stop"
	stop.DatabaseMaintenance = nil
	if err = e.perform(ctx, stop); err != nil {
		return errors.New("failed copy shutdown requires retry")
	}
	if _, err = e.RemoveWorkload(ctx, change.Request.Workload); err != nil {
		return err
	}
	volumes, err := e.output(ctx, "docker", "volume", "ls", "-q", "--filter", "label=com.docker.compose.project=cove-"+change.Request.Workload)
	if err != nil {
		return err
	}
	for _, volume := range strings.Fields(volumes) {
		if err = e.Run(ctx, e.Root, []string{"docker", "volume", "rm", volume}, nil, io.Discard); err != nil {
			return errors.New("failed copy volume cleanup requires retry")
		}
	}
	if err = removeDatabaseDump(filepath.Join(e.Root, "database-maintenance"), id); err != nil {
		return err
	}
	j.Operation.State = "cancelled"
	return writeJSON(e.path(id), j)
}
