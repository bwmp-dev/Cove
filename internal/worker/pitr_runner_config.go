package worker

import (
	"context"
	"encoding/hex"
	"errors"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type pitrDatabase struct {
	Host     string `json:"host"`
	Port     uint16 `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	SystemID string `json:"system_id"`
	Slot     string `json:"slot"`
	Major    int    `json:"major"`
}

type pitrRunnerConfig struct {
	RequireArchive bool             `json:"require_archive"`
	Workload       string           `json:"workload"`
	Owner          string           `json:"owner"`
	Policy         PITRPolicy       `json:"policy"`
	Database       pitrDatabase     `json:"database"`
	Storage        *StorageLocation `json:"storage,omitempty"`
}

func validatePITRRunner(config pitrRunnerConfig) error {
	if err := ValidatePITR(config.Policy.Settings); err != nil {
		return err
	}
	key, err := hex.DecodeString(config.Policy.Key)
	if err != nil || len(key) != 32 || !snapshotOwnerID.MatchString(config.Policy.Archive) || !snapshotOwnerID.MatchString(config.Owner) || !workloadIdentifier.MatchString(config.Workload) {
		return errors.New("invalid PITR archive identity or encryption key")
	}
	database := config.Database
	if !strings.HasPrefix(database.Host, "/source/") || path.Clean(database.Host) != database.Host || !snapshotRootPath.MatchString(database.Host) || database.Port != 5432 || len(database.User) == 0 || len(database.User) > 256 || len(database.Password) > 4096 || !utf8.ValidString(database.User+database.Password) || strings.ContainsRune(database.User+database.Password, 0) || !pitrSystemID.MatchString(database.SystemID) || !strings.HasPrefix(database.Slot, "cove_") || !workloadIdentifier.MatchString(database.Slot) || len(database.Slot) > 63 {
		return errors.New("invalid managed PostgreSQL connection")
	}
	if database.Major < 15 || database.Major > 18 {
		return errors.New("managed PostgreSQL PITR supports PostgreSQL 15–18")
	}
	if config.Storage != nil {
		return ValidateStorage(config.Storage)
	}
	return nil
}

func pitrConnection(ctx context.Context, database pitrDatabase) (*pgx.Conn, error) {
	if !strings.HasPrefix(database.Host, "/source/") || path.Clean(database.Host) != database.Host {
		return nil, errors.New("PITR connections require the private mounted Unix socket")
	}
	config, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	config.Host, config.Port, config.User, config.Password, config.Database = database.Host, database.Port, database.User, database.Password, "postgres"
	config.ConnectTimeout = 3 * time.Second
	config.TLSConfig = nil
	config.Fallbacks = nil
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, errors.New("managed PostgreSQL connection is unavailable")
	}
	var identity string
	if err := connection.QueryRow(ctx, "SELECT system_identifier::text FROM pg_control_system()").Scan(&identity); err != nil || identity != database.SystemID {
		connection.Close(ctx)
		return nil, errors.New("PostgreSQL cluster identity changed; begin a new PITR archive")
	}
	return connection, nil
}

func pitrWALGConfig(config pitrRunnerConfig, port int, access, secret string) map[string]string {
	return map[string]string{
		"PGHOST": config.Database.Host, "PGPORT": strconv.Itoa(int(config.Database.Port)), "PGUSER": config.Database.User, "PGPASSWORD": config.Database.Password,
		"WALG_SLOTNAME": config.Database.Slot, "WALG_S3_PREFIX": "s3://" + config.Policy.Archive,
		"AWS_ENDPOINT": "http://127.0.0.1:" + strconv.Itoa(port), "AWS_ACCESS_KEY_ID": access, "AWS_SECRET_ACCESS_KEY": secret,
		"AWS_REGION": "us-east-1", "AWS_S3_FORCE_PATH_STYLE": "true", "AWS_EC2_METADATA_DISABLED": "true", "S3_ENABLE_VERSIONING": "disabled",
		"WALG_LIBSODIUM_KEY": config.Policy.Key, "WALG_LIBSODIUM_KEY_TRANSFORM": "hex", "WALG_COMPRESSION_METHOD": "lz4",
		"WALG_UPLOAD_CONCURRENCY": "1", "WALG_DOWNLOAD_CONCURRENCY": "1", "WALG_NETWORK_RATE_LIMIT": "67108864",
		"WALG_TAR_SIZE_THRESHOLD": "16777216", "WALG_PREVENT_WAL_OVERWRITE": "true", "WALG_DOWNLOAD_FILE_RETRIES": "2", "WALG_S3_MAX_RETRIES": "2",
	}
}

func pitrStorageEnvironment(config pitrRunnerConfig) (string, []string, error) {
	if config.Storage == nil {
		return "/archive", nil, nil
	}
	environment, err := storageEnvironment(config.Storage)
	if err != nil {
		return "", nil, err
	}
	for i, value := range environment {
		environment[i] = strings.ReplaceAll(strings.ReplaceAll(value, "/storage-ca.pem", "/state/ca.pem"), "/storage-known-hosts", "/state/known_hosts")
	}
	return "cove:" + path.Join(config.Storage.Path, config.Workload, "pitr"), environment, nil
}
