package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type databaseImportFixture struct {
	engine    *Engine
	request   Request
	container string
	dump      []byte
	set       func(string)
	value     func() string
}

func newDatabaseImportFixture(t *testing.T, fixture, engine string, lifetime ...time.Duration) databaseImportFixture {
	t.Helper()
	timeout := 4 * time.Minute
	if len(lifetime) > 0 {
		timeout = lifetime[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	root := filepath.Join(fixture, "import-"+engine)
	workload := "import-" + engine + "-" + filepath.Base(fixture)
	e := &Engine{Root: root, Run: RunCommand}
	if err := os.MkdirAll(filepath.Join(root, "workloads", workload, "release-states"), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := e.output(ctx, args...)
		if err != nil {
			t.Fatal(args[:min(3, len(args))], err, out)
		}
		return strings.TrimSpace(out)
	}
	image, target, database := "redis:8-alpine", "/data", "all"
	command := []string{"redis-server", "--appendonly", "yes"}
	values := map[string]string{}
	switch engine {
	case "postgresql":
		image, target, database = "postgres:17-alpine", "/var/lib/postgresql/data", "app"
		command = []string{"postgres"}
		values = map[string]string{"POSTGRES_PASSWORD": "synthetic-import-password", "POSTGRES_DB": "app"}
		if os.Getenv("COVE_TEST_POSTGRES_18") == "1" {
			image, target = "postgres:18-alpine", "/var/lib/postgresql"
		}
	case "mariadb":
		image, target, database = "mariadb:11.8", "/var/lib/mysql", "app"
		command = []string{"mariadbd"}
		values = map[string]string{"MARIADB_ROOT_PASSWORD": "synthetic-import-password", "MARIADB_DATABASE": "app", "MYSQL_PWD": "synthetic-import-password"}
	case "mongodb":
		image, target = "mongo:8.0", "/data/db"
		command = []string{"mongod", "--replSet", "rs0", "--bind_ip_all"}
	case "valkey":
		image = "valkey/valkey:8-alpine"
		command[0] = "valkey-server"
	}
	name, volume := "cove-"+workload, "cove-data-"+workload
	run("docker", "volume", "create", "--label", "cove.test=true", volume)
	args := []string{"docker", "run", "-d", "--name", name, "--label", "com.docker.compose.project=cove-" + workload, "--label", "com.docker.compose.service=db", "--volume", volume + ":" + target}
	for key, value := range values {
		args = append(args, "-e", key+"="+value)
	}
	args = append(args, image)
	args = append(args, command...)
	run(args...)
	t.Cleanup(func() {
		e.Run(context.Background(), root, []string{"docker", "rm", "-f", "-v", name}, nil, io.Discard)
		if err := e.Run(context.Background(), root, []string{"docker", "volume", "rm", volume, "cove-backups-" + workload}, nil, io.Discard); err != nil {
			t.Error(err)
		}
	})
	sql := func(statement string) []string {
		if engine == "postgresql" {
			return []string{"docker", "exec", name, "psql", "-U", "postgres", "-d", "app", "-Atc", statement}
		}
		return []string{"docker", "exec", name, "mariadb", "-h", "127.0.0.1", "-u", "root", "-N", "-B", "app", "-e", statement}
	}
	ready := false
	for attempt := 0; attempt < 90; attempt++ {
		probe := []string{"docker", "exec", name, redisCLI(engine), "PING"}
		if engine == "mongodb" {
			probe = []string{"docker", "exec", name, "mongosh", "--quiet", "--eval", "db.adminCommand({ping:1})"}
		} else if engine == "postgresql" || engine == "mariadb" {
			probe = sql("SELECT 1")
		}
		if e.Run(ctx, root, probe, nil, io.Discard) == nil {
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	if !ready {
		t.Fatal("import fixture database unavailable")
	}
	f := databaseImportFixture{engine: e, container: name}
	if engine == "postgresql" || engine == "mariadb" {
		run(sql("CREATE TABLE markers(id integer PRIMARY KEY,value varchar(32));INSERT INTO markers VALUES(1,'before')")...)
		f.set = func(value string) { run(sql("UPDATE markers SET value='" + value + "' WHERE id=1")...) }
		f.value = func() string { return run(sql("SELECT value FROM markers WHERE id=1")...) }
	} else if engine == "mongodb" {
		query := func(statement string) string {
			return run("docker", "exec", name, "mongosh", "--quiet", "--eval", statement)
		}
		query(`rs.initiate({_id:"rs0",members:[{_id:0,host:"127.0.0.1:27017"}]})`)
		for attempt := 0; attempt < 60; attempt++ {
			if query("db.hello().isWritablePrimary") == "true" {
				break
			}
			time.Sleep(time.Second)
		}
		query(`db.getSiblingDB("app").markers.insertOne({_id:1,value:"before"})`)
		f.set = func(value string) {
			query(`db.getSiblingDB("app").markers.updateOne({_id:1},{$set:{value:"` + value + `"}})`)
		}
		f.value = func() string { return query(`db.getSiblingDB("app").markers.findOne({_id:1}).value`) }
	} else {
		run("docker", "exec", name, redisCLI(engine), "SET", "marker", "before")
		blob := make([]byte, 70000)
		if _, err := rand.Read(blob); err != nil {
			t.Fatal(err)
		}
		if err := runInput(ctx, root, []string{"docker", "exec", "-i", name, redisCLI(engine), "-x", "SET", "blob"}, nil, bytes.NewReader(blob), io.Discard); err != nil {
			t.Fatal(err)
		}
		f.set = func(value string) { run("docker", "exec", name, redisCLI(engine), "SET", "marker", value) }
		f.value = func() string { return run("docker", "exec", name, redisCLI(engine), "--raw", "GET", "marker") }
	}
	digest := run("docker", "inspect", "--format", "{{.Image}}", name)
	compose := filepath.Join(root, "workloads", workload, "release-states", "release.compose.json")
	if err := writeJSON(compose, map[string]any{"services": map[string]any{"db": map[string]any{"image": digest, "environment": values, "command": command}}}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(root, "workloads", workload, "current.json"), deployedState{ComposePath: compose, Environment: values}); err != nil {
		t.Fatal(err)
	}
	f.request = Request{ID: "original", Workload: workload, Action: "backup", Backup: &BackupConfig{Repository: "local", Password: "synthetic-import-encryption-password", KeepLast: 3, Database: &DatabaseBackup{Engine: engine, Service: "db", Name: database}}}
	if err := e.performBackup(ctx, f.request, io.Discard); err != nil {
		t.Fatal(err)
	}
	download := f.request
	download.ID, download.Action, download.BackupID = "download", "restore", "original"
	file, _, cleanup, err := e.backupDownload(ctx, download, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	f.dump, err = io.ReadAll(file)
	closeErr, cleanupErr := file.Close(), cleanup()
	if err != nil || closeErr != nil || cleanupErr != nil {
		t.Fatal(err, closeErr, cleanupErr)
	}
	return f
}

func TestDatabaseDumpImportDocker(t *testing.T) {
	fixture := os.Getenv("COVE_DATABASE_FIXTURE")
	if fixture == "" {
		t.Skip("disposable Docker fixture required")
	}
	for _, engine := range []string{"postgresql", "mariadb", "mongodb", "redis", "valkey"} {
		t.Run(engine, func(t *testing.T) {
			f := newDatabaseImportFixture(t, fixture, engine)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			started, err := f.engine.output(ctx, "docker", "inspect", "--format", "{{.State.StartedAt}}", f.container)
			if err != nil {
				t.Fatal(err)
			}
			f.set("after")
			r := f.request
			r.ID = "import"
			stream := StreamRequest{Action: "backup_upload", Request: &r, Limit: 8 << 20, Size: int64(len(f.dump))}
			meta, err := f.engine.receiveDatabaseImport(ctx, r, stream, bytes.NewReader(f.dump))
			if err != nil || meta.BackupID != "import" || meta.Size != int64(len(f.dump)) {
				t.Fatal("valid dump import failed", err, meta)
			}
			if f.value() != "after" {
				t.Fatal("import overwrote source data")
			}
			after, err := f.engine.output(ctx, "docker", "inspect", "--format", "{{.State.StartedAt}}", f.container)
			if err != nil || after != started {
				t.Fatal("import restarted source", err)
			}
			r.ID = "truncated"
			if _, err = f.engine.receiveDatabaseImport(ctx, r, stream, bytes.NewReader(f.dump[:len(f.dump)-1])); err == nil {
				t.Fatal("truncated upload accepted")
			}
			r.ID = "corrupt"
			stream.Size = 4
			if _, err = f.engine.receiveDatabaseImport(ctx, r, stream, strings.NewReader("bad!")); err == nil {
				t.Fatal("corrupt dump accepted")
			}
			records, err := f.engine.Backups(r.Workload)
			if err != nil || len(records) != 2 {
				t.Fatal("invalid import published a backup", len(records), err)
			}
			if _, err = os.Stat(filepath.Join(f.engine.Root, "backup-transfer")); !os.IsNotExist(err) {
				t.Fatal("import retained plaintext staging", err)
			}
			r.ID, r.Action, r.BackupID = "restore-import", "restore", "import"
			if err = f.engine.performBackup(ctx, r, io.Discard); err != nil {
				t.Fatal("imported backup restore failed", err)
			}
			if f.value() != "before" {
				t.Fatal("imported backup restored wrong data")
			}
			if engine == "redis" || engine == "valkey" {
				testRDBFramedImport(t, f)
			}
		})
	}
}
