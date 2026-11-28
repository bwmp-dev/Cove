package worker

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"
)

const pitrPostgresAuth = `set -eu
unset PGHOST PGHOSTADDR PGSERVICE PGSERVICEFILE PGOPTIONS
export PGUSER="${POSTGRES_USER:-postgres}"
if [ -n "${POSTGRES_USER_FILE:-}" ]; then PGUSER=$(cat "$POSTGRES_USER_FILE"; printf .); PGUSER=${PGUSER%.}; export PGUSER; fi
if [ -n "${POSTGRES_PASSWORD_FILE:-}" ]; then PGPASSWORD=$(cat "$POSTGRES_PASSWORD_FILE"; printf .); PGPASSWORD=${PGPASSWORD%.}; export PGPASSWORD; else export PGPASSWORD="${POSTGRES_PASSWORD:-}"; fi
`

type pitrSource struct {
	Container         string       `json:"container"`
	Image             string       `json:"image"`
	Mount             backupMount  `json:"mount"`
	Data              string       `json:"data"`
	Relative          string       `json:"relative"`
	UID               string       `json:"uid"`
	GID               string       `json:"gid"`
	Database          pitrDatabase `json:"database"`
	OriginalSockets   string       `json:"original_sockets"`
	OriginalSlotLimit string       `json:"original_slot_limit"`
	Bytes             int64        `json:"bytes"`
}

type pitrSourceInfo struct {
	SystemID    string `json:"system_id"`
	Major       int    `json:"major"`
	Data        string `json:"data"`
	Config      string `json:"config"`
	HBA         string `json:"hba"`
	Ident       string `json:"ident"`
	Sockets     string `json:"sockets"`
	SlotLimit   string `json:"slot_limit"`
	ArchiveMode string `json:"archive_mode"`
	WALLevel    string `json:"wal_level"`
	Superuser   bool   `json:"superuser"`
	Recovery    bool   `json:"recovery"`
	Tablespaces int    `json:"tablespaces"`
	Slots       int    `json:"slots"`
	WALSize     int64  `json:"wal_size"`
	Bytes       int64  `json:"bytes"`
}

func (e *Engine) pitrSQL(ctx context.Context, container, sql string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out := &limitedBuffer{Limit: 65536}
	err := runInput(ctx, e.Root, []string{"docker", "exec", "-i", container, "sh", "-ec", pitrPostgresAuth + `exec psql -X -v ON_ERROR_STOP=1 -h /var/run/postgresql -p 5432 -d postgres -At`}, nil, strings.NewReader(sql+"\n"), out)
	if err != nil {
		return nil, errors.New("managed PostgreSQL SQL command failed; verify its local administrator credentials and default socket")
	}
	return out.data.Bytes(), nil
}

func pitrSQLLiteral(value string) string {
	return "E'" + strings.ReplaceAll(strings.ReplaceAll(value, "\\", "\\\\"), "'", "''") + "'"
}

func pitrSocketSetting(original, socket string) (string, error) {
	reader := csv.NewReader(strings.NewReader(original))
	reader.TrimLeadingSpace = true
	values, err := reader.Read()
	if err != nil || len(values) > 16 {
		return "", errors.New("PostgreSQL socket configuration is unsupported")
	}
	result := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value, "\x00\r\n") {
			return "", errors.New("invalid PostgreSQL socket directory")
		}
		if value != socket {
			result = append(result, pitrSQLLiteral(value))
		}
	}
	if socket != "" {
		result = append(result, pitrSQLLiteral(socket))
	}
	return strings.Join(result, ","), nil
}

func (e *Engine) inspectPITRSource(ctx context.Context, workload, service, slot string) (pitrSource, error) {
	var source pitrSource
	container, err := e.databaseContainer(ctx, Request{Workload: workload, Backup: &BackupConfig{Database: &DatabaseBackup{Service: service}}})
	if err != nil {
		return source, err
	}
	source.Container = container
	raw, err := e.output(ctx, "docker", "inspect", container)
	if err != nil {
		return source, err
	}
	var inspected []struct {
		Image  string
		Mounts []backupMount
	}
	if json.Unmarshal([]byte(raw), &inspected) != nil || len(inspected) != 1 {
		return source, errors.New("PostgreSQL container inspection failed")
	}
	source.Image = inspected[0].Image
	rawInfo, err := e.pitrSQL(ctx, container, `SELECT json_build_object('system_id',(SELECT system_identifier::text FROM pg_control_system()),'major',current_setting('server_version_num')::int/10000,'data',current_setting('data_directory'),'config',current_setting('config_file'),'hba',current_setting('hba_file'),'ident',current_setting('ident_file'),'sockets',current_setting('unix_socket_directories'),'slot_limit',current_setting('max_slot_wal_keep_size'),'archive_mode',current_setting('archive_mode'),'wal_level',current_setting('wal_level'),'superuser',(SELECT rolsuper FROM pg_roles WHERE rolname=current_user),'recovery',pg_is_in_recovery(),'tablespaces',(SELECT count(*) FROM pg_tablespace WHERE spcname NOT IN ('pg_default','pg_global')),'slots',(SELECT count(*) FROM pg_replication_slots WHERE slot_name<>`+pitrSQLLiteral(slot)+`),'wal_size',pg_size_bytes(current_setting('wal_segment_size')),'bytes',(SELECT COALESCE(sum(pg_database_size(oid)),0)::bigint FROM pg_database));`)
	if err != nil {
		return source, err
	}
	var info pitrSourceInfo
	if json.Unmarshal(rawInfo, &info) != nil || !pitrSystemID.MatchString(info.SystemID) || info.Major < 15 || info.Major > 18 || !info.Superuser || info.Recovery || info.Tablespaces != 0 || info.Slots != 0 || info.WALSize != 16<<20 || info.Bytes > MaxTransferBytes || info.ArchiveMode != "off" || info.WALLevel != "replica" && info.WALLevel != "logical" {
		return source, errors.New("PITR requires a dedicated writable PostgreSQL 15–18 primary with administrator access, standard WAL, no other replication slots or external tablespaces, and at most 64 GiB of databases")
	}
	if !path.IsAbs(info.Data) || path.Clean(info.Data) != info.Data || !snapshotRootPath.MatchString(info.Data) {
		return source, errors.New("unsupported PostgreSQL data directory")
	}
	for _, file := range []string{info.Config, info.HBA, info.Ident} {
		if !strings.HasPrefix(path.Clean(file), info.Data+"/") {
			return source, errors.New("PITR requires PostgreSQL configuration inside its persistent data directory")
		}
	}
	for _, mount := range inspected[0].Mounts {
		if mount.Type == "volume" && mount.RW && mount.Name != "" && (info.Data == mount.Destination || strings.HasPrefix(info.Data, mount.Destination+"/")) {
			if source.Mount.Name != "" {
				return source, errors.New("ambiguous PostgreSQL persistent storage")
			}
			source.Mount = mount
		}
	}
	if source.Mount.Name == "" || strings.ContainsAny(source.Mount.Name, ",\r\n") {
		return source, errors.New("PITR requires the PostgreSQL data directory on a persistent named volume")
	}
	source.Data, source.Relative = info.Data, strings.TrimPrefix(strings.TrimPrefix(info.Data, source.Mount.Destination), "/")
	identity, err := e.output(ctx, "docker", "exec", container, "stat", "-c", "%u:%g", info.Data)
	if err != nil {
		return source, err
	}
	source.UID, source.GID, _ = strings.Cut(identity, ":")
	if !pitrSystemID.MatchString(source.UID) || !pitrSystemID.MatchString(source.GID) {
		return source, errors.New("PostgreSQL data ownership could not be verified")
	}
	credentials := &limitedBuffer{Limit: 8192}
	err = runInput(ctx, e.Root, []string{"docker", "exec", container, "sh", "-ec", pitrPostgresAuth + `printf '%s\000%s' "$PGUSER" "$PGPASSWORD"`}, nil, nil, credentials)
	if err != nil {
		return source, errors.New("PostgreSQL credentials unavailable")
	}
	user, password, ok := strings.Cut(credentials.data.String(), "\x00")
	if !ok {
		return source, errors.New("invalid PostgreSQL credentials")
	}
	source.Database = pitrDatabase{Host: path.Join("/source", source.Relative, ".cove-pitr-socket"), Port: 5432, User: user, Password: password, SystemID: info.SystemID, Slot: slot, Major: info.Major}
	source.OriginalSockets, source.OriginalSlotLimit, source.Bytes = info.Sockets, info.SlotLimit, info.Bytes
	return source, nil
}

func (e *Engine) waitPITRSource(ctx context.Context, source pitrSource) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		data, err := e.pitrSQL(ctx, source.Container, "SELECT system_identifier::text FROM pg_control_system();")
		if err == nil && strings.TrimSpace(string(data)) == source.Database.SystemID {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("PostgreSQL did not become ready after its PITR configuration restart")
}

func (e *Engine) createPITRSlot(ctx context.Context, source pitrSource) error {
	data, err := e.pitrSQL(ctx, source.Container, "SELECT COALESCE((SELECT json_build_object('kind',slot_type,'active',active,'state',wal_status) FROM pg_replication_slots WHERE slot_name="+pitrSQLLiteral(source.Database.Slot)+"),'null'::json);")
	if err != nil {
		return err
	}
	var slot *struct {
		Kind   string  `json:"kind"`
		Active bool    `json:"active"`
		State  *string `json:"state"`
	}
	if json.Unmarshal(data, &slot) != nil {
		return errors.New("PITR replication slot inspection failed")
	}
	if slot != nil {
		if slot.Kind != "physical" || slot.Active {
			return errors.New("PITR replication slot changed or remains active outside the owned receiver")
		}
		if slot.State == nil || *slot.State != "reserved" && *slot.State != "extended" {
			if _, err = e.pitrSQL(ctx, source.Container, "SELECT pg_drop_replication_slot("+pitrSQLLiteral(source.Database.Slot)+");"); err != nil {
				return err
			}
		}
	}
	_, err = e.pitrSQL(ctx, source.Container, "SELECT pg_create_physical_replication_slot("+pitrSQLLiteral(source.Database.Slot)+",true) WHERE NOT EXISTS(SELECT 1 FROM pg_replication_slots WHERE slot_name="+pitrSQLLiteral(source.Database.Slot)+");")
	return err
}
