package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

type DatabaseBackup struct {
	Engine  string `json:"engine"`
	Service string `json:"service"`
	Name    string `json:"name"`
}

func validateDatabaseBackup(d *DatabaseBackup) error {
	if d == nil {
		return nil
	}
	if d.Engine != "postgresql" && d.Engine != "mariadb" && !extendedDatabase(d.Engine) {
		return errors.New("select PostgreSQL, MariaDB, MongoDB, Redis or Valkey")
	}
	if extendedDatabase(d.Engine) && d.Name != "all" {
		return errors.New("MongoDB, Redis and Valkey snapshots include the complete instance; select all databases")
	}
	if !serviceIdentifier.MatchString(d.Service) || !environmentIdentifier.MatchString(d.Name) || len(d.Name) > 63 {
		return errors.New("database service and a database name of 1-63 letters, digits or underscores are required")
	}
	return nil
}

func (e *Engine) databaseContainer(ctx context.Context, r Request) (string, error) {
	list, err := e.trafficContainers(ctx, r.Workload, r.Backup.Database.Service, true)
	if err != nil {
		return "", err
	}
	if len(list) != 1 {
		return "", errors.New("logical backups require exactly one running database service container")
	}
	return list[0], nil
}

func databaseCommand(engine string, restore bool) string {
	if engine == "postgresql" {
		if restore {
			return `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec pg_restore --username="${POSTGRES_USER:-postgres}" --dbname="$1" --clean --if-exists --single-transaction --exit-on-error "$2"`
		}
		return `umask 077; export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec pg_dump --username="${POSTGRES_USER:-postgres}" --dbname="$1" --format=custom --file="$2"`
	}
	if restore {
		return `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; exec mariadb --user=root < "$2"`
	}
	return `umask 077; export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; exec mariadb-dump --user=root --single-transaction --routines --events --triggers --hex-blob --add-drop-database --databases "$1" --result-file="$2"`
}

func (e *Engine) dumpDatabase(ctx context.Context, r Request, container, staging string) (result error) {
	d := r.Backup.Database
	// Restore semantics for extensions and nontransactional tables require their own backup protocols.
	query := []string{"docker", "exec", container, "sh", "-ec"}
	var validation string
	if d.Engine == "postgresql" {
		validation = `export PGPASSWORD="${POSTGRES_PASSWORD:-}"; psql --username="${POSTGRES_USER:-postgres}" --dbname="$1" -Atc "SELECT count(*) FROM pg_extension WHERE extname='timescaledb'"`
	} else {
		validation = `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; mariadb --user=root -N -B -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='$1' AND table_type='BASE TABLE' AND engine<>'InnoDB'"`
	}
	validationOut := &limitedBuffer{Limit: 4096}
	if err := e.Run(ctx, e.Root, append(query, validation, "cove", d.Name), nil, validationOut); err != nil {
		return errors.New("could not verify database backup compatibility")
	}
	if strings.TrimSpace(validationOut.data.String()) != "0" {
		return errors.New("this database needs a consistent volume backup: TimescaleDB and non-InnoDB tables are not supported by logical backup mode")
	}
	remote := "/tmp/cove-database-" + r.ID + ".dump"
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := e.Run(cleanup, e.Root, []string{"docker", "exec", container, "rm", "-f", remote}, nil, io.Discard); err != nil {
			result = errors.Join(result, errors.New("could not remove temporary database dump from the container"))
		}
	}()
	if err := e.Run(ctx, e.Root, []string{"docker", "exec", container, "sh", "-ec", databaseCommand(d.Engine, false), "cove", d.Name, remote}, nil, io.Discard); err != nil {
		return errors.New("database dump failed; verify the database name and container credentials")
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "cp", container + ":" + remote, filepath.Join(staging, "database.dump")}, nil, io.Discard); err != nil {
		return errors.New("could not retrieve database dump")
	}
	info, err := os.Lstat(filepath.Join(staging, "database.dump"))
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("database dump is missing or empty")
	}
	return os.Chmod(filepath.Join(staging, "database.dump"), 0600)
}

func (e *Engine) performDatabaseBackup(ctx context.Context, r Request, out io.Writer) (result error) {
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	if r.Action == "restore" {
		raw, readErr := os.ReadFile(filepath.Join(dir, "storage-recovery.json"))
		if readErr == nil {
			var recovery storageRecovery
			if err := json.Unmarshal(raw, &recovery); err != nil {
				return err
			}
			if recovery.DatabaseContainer != "" {
				owner, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, recovery.DatabaseContainer)
				if err != nil || owner != e.composeProject(r.Workload) {
					return errors.New("database recovery container ownership changed")
				}
				if err := e.Run(ctx, e.Root, []string{"docker", "start", recovery.DatabaseContainer}, nil, out); err != nil {
					return err
				}
				defer func() {
					if result != nil {
						cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
						defer cancel()
						result = errors.Join(result, e.Run(cleanup, e.Root, []string{"docker", "stop", "--time", "30", recovery.DatabaseContainer}, nil, out))
					}
				}()
				// The database process may need time to recover its own journal before accepting connections.
				for attempt := 0; attempt < 30; attempt++ {
					probe := &limitedBuffer{Limit: 4096}
					command := []string{"docker", "exec", recovery.DatabaseContainer, "sh", "-ec", `pg_isready -U "${POSTGRES_USER:-postgres}"`}
					if r.Backup.Database.Engine == "mariadb" {
						command = []string{"docker", "exec", recovery.DatabaseContainer, "healthcheck.sh", "--connect", "--innodb_initialized"}
					}
					if e.Run(ctx, e.Root, command, nil, probe) == nil {
						break
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(time.Second):
					}
				}
			}
		} else if !os.IsNotExist(readErr) {
			return readErr
		}
	}
	container, err := e.databaseContainer(ctx, r)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "current.json"))
	if err != nil {
		return err
	}
	var current deployedState
	if err = json.Unmarshal(raw, &current); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(e.Root, "database-backup-")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(staging)) }()
	mounts := []backupMount{{Type: "bind", Source: staging, Destination: "/database", RW: true}}
	if r.Backup.Remote != nil {
		if err = e.remoteBackupHelper(ctx, out); err != nil {
			return err
		}
	} else {
		if err = e.Run(ctx, e.Root, []string{"docker", "pull", "restic/restic:0.19.1"}, nil, out); err != nil {
			return err
		}
	}
	if r.Action == "backup" {
		probe := &limitedBuffer{Limit: 2 << 20}
		if err = e.restic(ctx, r, nil, false, probe, "snapshots", "--json"); err != nil {
			if err = e.restic(ctx, r, nil, false, out, "init"); err != nil {
				return err
			}
		}
		if err = e.dumpDatabase(ctx, r, container, staging); err != nil {
			return err
		}
		_, err = e.makeBackup(ctx, r, mounts, current, out)
		return err
	}
	restore, err := e.loadBackupManifest(ctx, r, out)
	if err != nil {
		return err
	}
	if restore.Record.Workload != r.Workload || restore.Record.Repository != r.Backup.Repository || !reflect.DeepEqual(restore.Record.Database, r.Backup.Database) {
		return errors.New("backup database, engine or repository does not match the current policy")
	}
	if err = e.restic(ctx, r, nil, false, out, "check", "--read-data"); err != nil {
		return err
	}
	// Save a logical safety snapshot before any database mutation.
	safety := r
	safety.ID = r.ID + "-before"
	if err = e.dumpDatabase(ctx, safety, container, staging); err != nil {
		return err
	}
	if _, err = e.makeBackup(ctx, safety, mounts, current, out); err != nil {
		return err
	}
	if err = e.restic(ctx, r, mounts, true, out, "restore", restore.Record.Snapshot, "--target", "/", "--include", "/data", "--delete", "--verify"); err != nil {
		return err
	}
	remote := "/tmp/cove-database-" + r.ID + ".dump"
	if err = e.Run(ctx, e.Root, []string{"docker", "cp", filepath.Join(staging, "database.dump"), container + ":" + remote}, nil, io.Discard); err != nil {
		return errors.New("could not stage database restore")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := e.Run(cleanup, e.Root, []string{"docker", "exec", container, "rm", "-f", remote}, nil, io.Discard); err != nil {
			result = errors.Join(result, errors.New("could not remove temporary restore dump from the container"))
		}
		if result != nil {
			result = errors.Join(result, e.Run(cleanup, e.Root, []string{"docker", "stop", "--time", "30", container}, nil, out))
		}
	}()
	recovery := filepath.Join(dir, "storage-recovery.json")
	if err = writeJSON(recovery, storageRecovery{Restoring: true, DatabaseContainer: container, Helper: "cove-backup-" + r.ID}); err != nil {
		return err
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "exec", container, "sh", "-ec", databaseCommand(r.Backup.Database.Engine, true), "cove", r.Backup.Database.Name, remote}, nil, io.Discard); err != nil {
		fmt.Fprintln(out, "Database restore failed. The pre-restore logical backup is retained. PostgreSQL restores are transactional; MariaDB may require restoring the safety backup.")
		return errors.New("database restore failed; database stopped for recovery")
	}
	return os.Remove(recovery)
}
