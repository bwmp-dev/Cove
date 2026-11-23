package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const maxDatabaseDumpBytes int64 = 64 << 30

func extendedDatabase(engine string) bool {
	return engine == "mongodb" || engine == "redis" || engine == "valkey"
}

func runInput(ctx context.Context, dir string, args, env []string, input io.Reader, out io.Writer) error {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	configureCommand(cmd)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = input
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	return cmd.Run()
}

type sizeBoundWriter struct {
	io.Writer
	remaining int64
}

func (w *sizeBoundWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("stream exceeds the configured byte limit")
	}
	n, err := w.Writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func databaseDataSnapshot(m backupManifest) string {
	if m.Record.DataSnapshot != "" {
		return m.Record.DataSnapshot
	}
	return m.Record.Snapshot
}

const mongoCredentials = `const fs=require("fs"); function credential(k){return process.env[k]||(process.env[k+"_FILE"]?fs.readFileSync(process.env[k+"_FILE"],"utf8").trimEnd():"")}; const user=credential("MONGO_INITDB_ROOT_USERNAME"),password=credential("MONGO_INITDB_ROOT_PASSWORD");`

func extendedDumpCommand(engine string) string {
	if engine == "mongodb" {
		return mongoAuth + `mongosh --nodb --quiet --eval '` + mongoCredentials + `const c=new Mongo("mongodb://127.0.0.1:27017"); if(user&&!c.getDB("admin").auth(user,password)) throw Error("authentication failed"); const h=c.getDB("admin").runCommand({hello:1}); if(!h.setName||h.msg==="isdbgrid") throw Error("online consistent MongoDB backup requires a replica set member");' >/dev/null
if [ -n "${MONGO_INITDB_ROOT_USERNAME:-}" ]; then
 mongosh --nodb --quiet --eval '` + mongoCredentials + `print(JSON.stringify({password}));' | mongodump --archive --oplog --authenticationDatabase admin --username "$MONGO_INITDB_ROOT_USERNAME" --config /dev/stdin
else exec mongodump --archive --oplog; fi`
	}
	cli := "redis-cli"
	if engine == "valkey" {
		cli = "valkey-cli"
	}
	return redisAuth + `exec ` + cli + ` --rdb -`
}

func (e *Engine) streamDatabaseSnapshot(ctx context.Context, r Request, container string, out io.Writer) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	reader, writer := io.Pipe()
	produced := make(chan error, 1)
	go func() {
		err := runInput(ctx, e.Root, []string{"docker", "exec", container, "timeout", "-k", "5", "1800", "sh", "-ec", extendedDumpCommand(r.Backup.Database.Engine)}, nil, nil, &sizeBoundWriter{Writer: writer, remaining: maxDatabaseDumpBytes})
		writer.CloseWithError(err)
		produced <- err
	}()
	dataRequest := r
	dataRequest.ID = r.ID + "-data"
	result := &limitedBuffer{Limit: 2 << 20}
	err := e.resticInput(ctx, dataRequest, nil, false, reader, result, "backup", "--stdin", "--stdin-filename", "/data/000/database.dump", "--json", "--tag", "cove-workload:"+r.Workload, "--tag", "cove-operation:"+dataRequest.ID)
	reader.Close()
	if err != nil {
		cancel()
	}
	producerErr := <-produced
	if err != nil || producerErr != nil {
		return "", errors.New("streamed database backup failed; verify engine tools, credentials, topology and size")
	}
	var snapshot string
	for _, line := range strings.Split(result.data.String(), "\n") {
		var item struct {
			Snapshot string `json:"snapshot_id"`
		}
		if json.Unmarshal([]byte(line), &item) == nil && snapshotIdentifier.MatchString(item.Snapshot) {
			snapshot = item.Snapshot
		}
	}
	if snapshot == "" {
		lookup := &limitedBuffer{Limit: 2 << 20}
		if err = e.restic(ctx, dataRequest, nil, false, lookup, "snapshots", "--json", "--tag", "cove-workload:"+r.Workload+",cove-operation:"+dataRequest.ID); err != nil {
			return "", err
		}
		var found []struct{ ID string }
		if json.Unmarshal(lookup.data.Bytes(), &found) != nil || len(found) != 1 || !snapshotIdentifier.MatchString(found[0].ID) {
			return "", errors.New("streamed snapshot identity unavailable")
		}
		snapshot = found[0].ID
	}
	fmt.Fprintln(out, "Database data streamed into the encrypted repository without stopping its container.")
	return snapshot, nil
}

func (e *Engine) readDatabaseSnapshot(ctx context.Context, r Request, m backupManifest, path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = e.resticInput(ctx, r, nil, false, strings.NewReader(""), &sizeBoundWriter{Writer: file, remaining: maxDatabaseDumpBytes}, "dump", databaseDataSnapshot(m), "/data/000/database.dump")
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.New("could not retrieve bounded database dump")
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return errors.New("database dump is empty")
	}
	return nil
}

func (e *Engine) performExtendedDatabaseBackup(ctx context.Context, r Request, out io.Writer) (result error) {
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	var current deployedState
	data, err := os.ReadFile(filepath.Join(dir, "current.json"))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &current); err != nil {
		return err
	}
	if r.Backup.Remote != nil {
		if err = e.remoteBackupHelper(ctx, out); err != nil {
			return err
		}
	} else if err = e.Run(ctx, e.Root, []string{"docker", "pull", "restic/restic:0.19.1"}, nil, out); err != nil {
		return err
	}
	recovery, err := e.extendedDatabaseRecovery(ctx, r)
	if err != nil {
		return err
	}
	container := ""
	if recovery != nil {
		container = recovery.DatabaseContainer
		if r.Backup.Database.Engine == "mongodb" {
			if err = e.Run(ctx, e.Root, []string{"docker", "start", container}, nil, out); err != nil {
				return err
			}
			defer func() {
				if result != nil {
					c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
					defer cancel()
					result = errors.Join(result, e.Run(c, e.Root, []string{"docker", "stop", "--time", "30", container}, nil, out))
				}
			}()
			if err = e.waitExtendedDatabase(ctx, r.Backup.Database.Engine, container); err != nil {
				return err
			}
		}
	} else {
		container, err = e.databaseContainer(ctx, r)
	}
	if err != nil {
		return err
	}
	stage, err := e.databaseStage(r.Workload, "database-stream")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(stage)) }()
	mounts := []backupMount{{Type: "bind", Source: stage, Destination: "/database", RW: true}}
	if r.Action == "backup" {
		if err = e.restic(ctx, r, nil, false, io.Discard, "snapshots", "--json"); err != nil {
			if err = e.restic(ctx, r, nil, false, out, "init"); err != nil {
				return err
			}
		}
		_, err = e.createStreamedDatabaseBackup(ctx, r, container, mounts, current, out)
		return err
	}
	m, err := e.loadBackupManifest(ctx, r, out)
	if err != nil {
		return err
	}
	if m.Record.Database == nil || *m.Record.Database != *r.Backup.Database || m.Record.Workload != r.Workload || m.Record.Repository != r.Backup.Repository {
		return errors.New("database snapshot does not match this resource and policy")
	}
	if err = e.readDatabaseSnapshot(ctx, r, m, filepath.Join(stage, "database.dump")); err != nil {
		return err
	}
	cleanup, err := e.verifyExtendedDatabase(ctx, r, container, filepath.Join(stage, "database.dump"), false)
	if err != nil {
		return err
	}
	if err = cleanup(); err != nil {
		return err
	}
	if recovery == nil {
		safety := r
		safety.ID = r.ID + "-before"
		if _, err = e.createStreamedDatabaseBackup(ctx, safety, container, mounts, current, out); err != nil {
			return err
		}
	}
	return e.restoreExtendedDatabase(ctx, r, container, filepath.Join(stage, "database.dump"), out)
}

func (e *Engine) databaseStage(workload, name string) (string, error) {
	if !workloadIdentifier.MatchString(workload) {
		return "", errors.New("invalid database staging workload")
	}
	stage := filepath.Join(e.Root, "workloads", workload, name)
	if err := os.RemoveAll(stage); err != nil {
		return "", err
	}
	return stage, os.MkdirAll(stage, 0700)
}
