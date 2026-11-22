package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func databaseHelperName(r Request) string {
	sum := sha256.Sum256([]byte(r.Workload + "/" + r.ID))
	return "cove-db-verify-" + hex.EncodeToString(sum[:12])
}

func (e *Engine) cleanupDatabaseVerifier(ctx context.Context, r Request) error {
	owner, err := e.backupHelperOwner(false)
	if err != nil || owner == "" {
		return err
	}
	ids, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "name=^/"+databaseHelperName(r)+"$", "--filter", "label=dev.cove.database-verify="+r.ID, "--filter", "label=dev.cove.backup-owner="+owner)
	if err != nil {
		return err
	}
	if owned := strings.Fields(ids); len(owned) > 0 {
		return e.Run(ctx, e.Root, append([]string{"docker", "rm", "--force", "--volumes"}, owned...), nil, io.Discard)
	}
	return nil
}

func redisCLI(engine string) string {
	if engine == "valkey" {
		return "valkey-cli"
	}
	return "redis-cli"
}

const mongoAuth = `if [ -n "${MONGO_INITDB_ROOT_USERNAME_FILE:-}" ]; then export MONGO_INITDB_ROOT_USERNAME=$(cat "$MONGO_INITDB_ROOT_USERNAME_FILE"); fi; `

const redisAuth = `export REDISCLI_AUTH="${REDIS_PASSWORD:-${VALKEY_PASSWORD:-}}"; if [ -n "${REDIS_PASSWORD_FILE:-}" ]; then REDISCLI_AUTH=$(cat "$REDIS_PASSWORD_FILE"); fi; if [ -n "${VALKEY_PASSWORD_FILE:-}" ]; then REDISCLI_AUTH=$(cat "$VALKEY_PASSWORD_FILE"); fi; if [ -n "$REDISCLI_AUTH" ]; then export REDISCLI_AUTH; export VALKEYCLI_AUTH="$REDISCLI_AUTH"; else unset REDISCLI_AUTH VALKEYCLI_AUTH; fi; `

func (e *Engine) verifyExtendedDatabase(ctx context.Context, r Request, source, dump string, appendOnly bool) (cleanup func() error, result error) {
	image, err := e.output(ctx, "docker", "inspect", "--format", "{{.Image}}", source)
	if err != nil || !imageDigest.MatchString(image) {
		return nil, errors.New("database image identity unavailable")
	}
	owner, err := e.backupHelperOwner(true)
	if err != nil {
		return nil, err
	}
	name := databaseHelperName(r)
	remove := func() error {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		return e.cleanupDatabaseVerifier(c, r)
	}
	cleanup = remove
	created := false
	defer func() {
		if result != nil && created {
			result = errors.Join(result, remove())
		}
	}()
	args := []string{"docker", "create", "--log-driver", "local", "--log-opt", "max-size=1m", "--log-opt", "max-file=2", "--name", name, "--label", "dev.cove.database-verify=" + r.ID, "--label", "dev.cove.backup-owner=" + owner, "--network", "none", "--restart", "no", "--memory", "2g", "--cpus", "2", "--pids-limit", "512", image}
	engine := r.Backup.Database.Engine
	if engine == "mongodb" {
		args = append(args, "mongod", "--bind_ip", "127.0.0.1")
	} else {
		server := "redis-server"
		if engine == "valkey" {
			server = "valkey-server"
		}
		args = append(args, server, "--dir", "/data", "--dbfilename", "dump.rdb", "--appendonly", "no", "--save", "")
	}
	if err = e.Run(ctx, e.Root, args, nil, io.Discard); err != nil {
		return nil, errors.New("could not create isolated database verifier")
	}
	created = true
	if engine != "mongodb" {
		if err = e.Run(ctx, e.Root, []string{"docker", "cp", dump, name + ":/data/dump.rdb"}, nil, io.Discard); err != nil {
			return nil, err
		}
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "start", name}, nil, io.Discard); err != nil {
		return nil, err
	}
	ready := false
	for attempt := 0; attempt < 90; attempt++ {
		command := []string{"docker", "exec", name, redisCLI(engine), "PING"}
		if engine == "mongodb" {
			command = []string{"docker", "exec", name, "mongosh", "--quiet", "--eval", "if(!db.adminCommand({ping:1}).ok) quit(1)"}
		}
		if e.Run(ctx, e.Root, command, nil, io.Discard) == nil {
			ready = true
			break
		}
		state, inspectErr := e.output(ctx, "docker", "inspect", "--format", "{{.State.Status}}", name)
		if inspectErr != nil || state == "exited" || state == "dead" {
			return nil, errors.New("isolated database rejected the dump or exceeded its resource limit")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if !ready {
		return nil, errors.New("isolated database could not load the dump within its 2 GiB verification limit")
	}
	if engine == "mongodb" {
		if err = e.Run(ctx, e.Root, []string{"docker", "cp", dump, name + ":/tmp/cove-restore.archive"}, nil, io.Discard); err != nil {
			return nil, err
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "exec", name, "mongorestore", "--archive=/tmp/cove-restore.archive", "--drop", "--oplogReplay", "--stopOnError"}, nil, io.Discard); err != nil {
			return nil, errors.New("MongoDB archive failed isolated restore validation")
		}
		raw, err := e.output(ctx, "docker", "exec", source, "mongosh", "--nodb", "--quiet", "--eval", mongoCredentials+`print(JSON.stringify({user,password}));`)
		var credentials struct{ User, Password string }
		if err != nil || json.Unmarshal([]byte(raw), &credentials) != nil {
			return nil, errors.New("MongoDB authentication configuration unavailable")
		}
		if credentials.User != "" {
			if err = e.Run(ctx, e.Root, []string{"docker", "exec", "-e", "COVE_VERIFY_USER", "-e", "COVE_VERIFY_PASSWORD", name, "mongosh", "--quiet", "--eval", `if(!db.getSiblingDB("admin").auth(process.env.COVE_VERIFY_USER,process.env.COVE_VERIFY_PASSWORD)) quit(1)`}, []string{"COVE_VERIFY_USER=" + credentials.User, "COVE_VERIFY_PASSWORD=" + credentials.Password}, io.Discard); err != nil {
				return nil, errors.New("MongoDB archive must retain the currently configured root credentials; verify them before restoring all databases")
			}
		}
	} else if appendOnly {
		if err = e.Run(ctx, e.Root, []string{"docker", "exec", name, redisCLI(engine), "CONFIG", "SET", "appendonly", "yes"}, nil, io.Discard); err != nil {
			return nil, err
		}
		ready = false
		for attempt := 0; attempt < 120; attempt++ {
			persistence, err := e.output(ctx, "docker", "exec", name, redisCLI(engine), "--raw", "INFO", "persistence")
			if err == nil && strings.Contains(persistence, "aof_enabled:1") && strings.Contains(persistence, "aof_rewrite_in_progress:0") && strings.Contains(persistence, "aof_last_bgrewrite_status:ok") {
				if e.Run(ctx, e.Root, []string{"docker", "exec", name, "test", "-f", "/data/appendonlydir/appendonly.aof.manifest"}, nil, io.Discard) == nil {
					ready = true
					break
				}
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
			}
		}
		if !ready {
			return nil, errors.New("could not regenerate append-only persistence from the verified RDB")
		}
	}
	return cleanup, nil
}

func (e *Engine) extendedDatabaseRecovery(ctx context.Context, r Request) (*storageRecovery, error) {
	data, err := os.ReadFile(filepath.Join(e.Root, "workloads", r.Workload, "storage-recovery.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recovery storageRecovery
	if len(data) > 65536 || json.Unmarshal(data, &recovery) != nil || !recovery.Restoring || recovery.DatabaseEngine != r.Backup.Database.Engine || recovery.DatabaseContainer == "" || r.Action != "restore" {
		return nil, errors.New("finish the interrupted restore before starting another database operation")
	}
	if recovery.DatabaseUser != "" {
		operation := strings.TrimSuffix(recovery.DatabaseVerify, "-apply")
		want := "cove_restore_" + strings.TrimPrefix(databaseHelperName(Request{Workload: r.Workload, ID: operation}), "cove-db-verify-")
		if recovery.DatabaseUser != want {
			return nil, errors.New("temporary database recovery account ownership changed")
		}
	}
	ids := append([]string{recovery.DatabaseContainer}, recovery.Containers...)
	for _, id := range ids {
		owner, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, id)
		if err != nil || owner != "cove-"+r.Workload {
			return nil, errors.New("database recovery container ownership changed")
		}
	}
	if recovery.DatabaseVerify != "" {
		if !identifier.MatchString(recovery.DatabaseVerify) {
			return nil, errors.New("invalid database recovery identity")
		}
		if err = e.cleanupDatabaseVerifier(ctx, Request{Workload: r.Workload, ID: recovery.DatabaseVerify}); err != nil {
			return nil, err
		}
	}
	return &recovery, nil
}

func (e *Engine) cleanupDatabaseRestoreUser(ctx context.Context, container, user string) error {
	if len(user) != len("cove_restore_")+24 || !strings.HasPrefix(user, "cove_restore_") {
		return errors.New("invalid temporary database account")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(user, "cove_restore_")); err != nil {
		return errors.New("invalid temporary database account")
	}
	command := `mongosh --nodb --quiet --eval '` + mongoCredentials + `const c=new Mongo("mongodb://127.0.0.1:27017"),a=c.getDB("admin");if(user&&!a.auth(user,password))quit(1);if(a.getUser(process.env.COVE_RESTORE_USER)&&!a.dropUser(process.env.COVE_RESTORE_USER))quit(1);'`
	return e.Run(ctx, e.Root, []string{"docker", "exec", "-e", "COVE_RESTORE_USER", container, "sh", "-ec", command}, []string{"COVE_RESTORE_USER=" + user}, io.Discard)
}

func (e *Engine) waitExtendedDatabase(ctx context.Context, engine, container string) error {
	for attempt := 0; attempt < 90; attempt++ {
		command := redisAuth + redisCLI(engine) + " PING"
		if engine == "mongodb" {
			command = `mongosh --nodb --quiet --eval '` + mongoCredentials + `const c=new Mongo("mongodb://127.0.0.1:27017"); if(user&&!c.getDB("admin").auth(user,password)) quit(1); const a=c.getDB("admin"),h=a.runCommand({hello:1});if((h.setName||h.isreplicaset)&&!h.isWritablePrimary)quit(1);if(!a.runCommand({ping:1}).ok) quit(1);'`
		}
		if e.Run(ctx, e.Root, []string{"docker", "exec", container, "sh", "-ec", command}, nil, io.Discard) == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("database did not become ready")
}

func (e *Engine) restoreExtendedDatabase(ctx context.Context, r Request, container, dump string, out io.Writer) (result error) {
	engine := r.Backup.Database.Engine
	journal := filepath.Join(e.Root, "workloads", r.Workload, "storage-recovery.json")
	mounts, running, err := e.backupMounts(ctx, r.Workload)
	if err != nil {
		return err
	}
	previous, err := e.extendedDatabaseRecovery(ctx, r)
	if err != nil {
		return err
	}
	appendOnly := false
	if previous != nil {
		running = previous.Containers
		appendOnly = previous.DatabaseAppendOnly
	}
	var target backupMount
	if engine != "mongodb" {
		if previous == nil {
			for _, setting := range []struct{ key, want string }{{"dir", "/data"}, {"dbfilename", "dump.rdb"}, {"appendfilename", "appendonly.aof"}, {"appenddirname", "appendonlydir"}} {
				raw, err := e.output(ctx, "docker", "exec", container, "sh", "-ec", redisAuth+redisCLI(engine)+` --raw CONFIG GET "$1"`, "cove", setting.key)
				if err != nil || strings.TrimSpace(raw) != setting.key+"\n"+setting.want {
					return fmt.Errorf("RDB restore requires the standard %s setting %s", setting.key, setting.want)
				}
			}
			raw, err := e.output(ctx, "docker", "exec", container, "sh", "-ec", redisAuth+redisCLI(engine)+" --raw CONFIG GET appendonly")
			if err != nil {
				return err
			}
			appendOnly = strings.TrimSpace(raw) == "appendonly\nyes"
		}
		rawMounts, err := e.output(ctx, "docker", "inspect", "--format", "{{json .Mounts}}", container)
		var databaseMounts []backupMount
		if err != nil || json.Unmarshal([]byte(rawMounts), &databaseMounts) != nil {
			return errors.New("database persistence mounts unavailable")
		}
		for _, mount := range databaseMounts {
			if mount.Destination != "/data" || !mount.RW {
				continue
			}
			for _, allowed := range mounts {
				if mount.Type == allowed.Type && mount.Source == allowed.Source && mount.Name == allowed.Name {
					target = mount
				}
			}
		}
		if target.Type == "" {
			return errors.New("RDB restore requires persistent storage mounted at /data")
		}
	}
	if engine == "mongodb" {
		command := `mongosh --nodb --quiet --eval '` + mongoCredentials + `const c=new Mongo("mongodb://127.0.0.1:27017"),a=c.getDB("admin"); if(user&&!a.auth(user,password))quit(1);const h=a.runCommand({hello:1});if(h.isreplicaset&&!h.setName)quit(1);if(h.setName){const conf=a.runCommand({replSetGetConfig:1});if(!h.isWritablePrimary||!conf.ok||conf.config.members.length!==1)quit(1)}'`
		if err = e.Run(ctx, e.Root, []string{"docker", "exec", container, "sh", "-ec", command}, nil, io.Discard); err != nil {
			return errors.New("in-place MongoDB restore requires a standalone server or a writable single-member replica set; restore multi-member backups as a separate resource")
		}
	}
	if engine == "mongodb" && previous != nil && previous.DatabaseUser != "" {
		if err = e.cleanupDatabaseRestoreUser(ctx, container, previous.DatabaseUser); err != nil {
			return errors.New("temporary database account cleanup requires repair")
		}
	}
	verifyRequest := r
	verifyRequest.ID += "-apply"
	cleanup, err := e.verifyExtendedDatabase(ctx, verifyRequest, container, dump, appendOnly)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, cleanup()) }()
	recovery := storageRecovery{Containers: running, Restoring: true, DatabaseContainer: container, Helper: "cove-backup-" + r.ID, DatabaseVerify: verifyRequest.ID, DatabaseEngine: engine, DatabaseAppendOnly: appendOnly}
	restoreEnv := []string{}
	if engine == "mongodb" {
		auth, err := e.output(ctx, "docker", "exec", container, "mongosh", "--nodb", "--quiet", "--eval", mongoCredentials+`print(user?"true":"false");`)
		if err != nil {
			return errors.New("database authentication configuration unavailable")
		}
		if strings.TrimSpace(auth) == "true" {
			recovery.DatabaseUser = "cove_restore_" + strings.TrimPrefix(databaseHelperName(r), "cove-db-verify-")
			restoreEnv = []string{"COVE_RESTORE_USER=" + recovery.DatabaseUser, "COVE_RESTORE_PASSWORD=" + rand.Text()}
		}
	}
	if err = writeJSON(journal, recovery); err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			if len(running) > 0 {
				result = errors.Join(result, e.Run(c, e.Root, append([]string{"docker", "stop", "--time", "30"}, running...), nil, out))
			}
		}
	}()
	if engine == "mongodb" {
		identity, err := e.output(ctx, "docker", "inspect", "--format", "{{.Id}}", container)
		if err != nil || !fileContainerID.MatchString(identity) {
			return errors.New("database container identity unavailable")
		}
		others := []string{}
		for _, id := range running {
			if id != identity && !strings.HasPrefix(identity, id) {
				others = append(others, id)
			}
		}
		if len(others) > 0 {
			if err = e.Run(ctx, e.Root, append([]string{"docker", "stop", "--time", "30"}, others...), nil, out); err != nil {
				return err
			}
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "cp", dump, container + ":/tmp/cove-database-restore.archive"}, nil, io.Discard); err != nil {
			return err
		}
		accountCleaned := recovery.DatabaseUser == ""
		if !accountCleaned {
			defer func() {
				if !accountCleaned {
					c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
					defer cancel()
					result = errors.Join(result, e.cleanupDatabaseRestoreUser(c, container, recovery.DatabaseUser))
				}
			}()
		}
		command := mongoAuth + `if [ -n "${COVE_RESTORE_USER:-}" ]; then
 mongosh --nodb --quiet --eval '` + mongoCredentials + `const c=new Mongo("mongodb://127.0.0.1:27017"),a=c.getDB("admin");if(!a.auth(user,password))quit(1);a.createUser({user:process.env.COVE_RESTORE_USER,pwd:process.env.COVE_RESTORE_PASSWORD,roles:[{role:"root",db:"admin"}]});' >/dev/null
fi
mongosh --nodb --quiet --eval '` + mongoCredentials + `const c=new Mongo("mongodb://127.0.0.1:27017"),a=c.getDB("admin");if(user&&!a.auth(user,password))quit(1);const list=a.runCommand({listDatabases:1,nameOnly:true});if(!list.ok)quit(1);for(const d of list.databases){if(!["admin","config","local"].includes(d.name)&&!c.getDB(d.name).dropDatabase().ok)quit(1)}' >/dev/null
if [ -n "${COVE_RESTORE_USER:-}" ]; then mongosh --nodb --quiet --eval 'print(JSON.stringify({password:process.env.COVE_RESTORE_PASSWORD}));' | mongorestore --username "$COVE_RESTORE_USER" --authenticationDatabase admin --config /dev/stdin --archive=/tmp/cove-database-restore.archive --oplogReplay --stopOnError; else mongorestore --archive=/tmp/cove-database-restore.archive --oplogReplay --stopOnError; fi`
		if err = e.Run(ctx, e.Root, []string{"docker", "exec", "-e", "COVE_RESTORE_USER", "-e", "COVE_RESTORE_PASSWORD", container, "sh", "-ec", command}, restoreEnv, io.Discard); err != nil {
			return errors.New("MongoDB restore failed; containers remain stopped and the safety snapshot is retained")
		}
		if !accountCleaned {
			if err = e.cleanupDatabaseRestoreUser(ctx, container, recovery.DatabaseUser); err != nil {
				return errors.New("temporary database account cleanup requires repair")
			}
			accountCleaned = true
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "exec", container, "rm", "-f", "/tmp/cove-database-restore.archive"}, nil, io.Discard); err != nil {
			return err
		}
		if len(others) > 0 {
			if err = e.startBackupContainers(ctx, others, out); err != nil {
				return err
			}
		}
	} else {
		if len(running) > 0 {
			if err = e.Run(ctx, e.Root, append([]string{"docker", "stop", "--time", "30"}, running...), nil, out); err != nil {
				return err
			}
		}
		source := target.Source
		if target.Type == "volume" {
			source = target.Name
		}
		mount := fmt.Sprintf("type=%s,source=%s,target=/target", target.Type, source)
		if err = e.Run(ctx, e.Root, []string{"docker", "run", "--rm", "--name", recovery.Helper, "--network", "none", "--read-only", "--mount", mount, "--entrypoint", "sh", "restic/restic:0.19.1", "-ec", "rm -f -- /target/dump.rdb /target/appendonly.aof; rm -rf -- /target/appendonlydir"}, nil, io.Discard); err != nil {
			return err
		}
		reader, writer := io.Pipe()
		copied := make(chan error, 1)
		go func() {
			err := runInput(ctx, e.Root, []string{"docker", "cp", databaseHelperName(verifyRequest) + ":/data/.", "-"}, nil, nil, &sizeBoundWriter{Writer: writer, remaining: maxDatabaseDumpBytes*2 + (16 << 20)})
			writer.CloseWithError(err)
			copied <- err
		}()
		err = runInput(ctx, e.Root, []string{"docker", "cp", "-", container + ":/data"}, nil, reader, io.Discard)
		reader.Close()
		producerErr := <-copied
		if err != nil || producerErr != nil {
			return errors.New("RDB restore copy failed; safety snapshot retained and containers remain stopped")
		}
		if err = e.startBackupContainers(ctx, running, out); err != nil {
			return err
		}
		if err = e.waitExtendedDatabase(ctx, engine, container); err != nil {
			return errors.New("restored RDB database failed readiness; safety snapshot retained")
		}
	}
	success = true
	return os.Remove(journal)
}
