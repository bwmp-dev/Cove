package worker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDatabaseSnapshotRoundTrip(t *testing.T) {
	fixture := os.Getenv("COVE_DATABASE_FIXTURE")
	if fixture == "" {
		t.Skip("isolated Docker fixture required")
	}
	for _, tc := range []struct {
		name, image, mount        string
		env, cli                  []string
		ready, seed, change, read string
	}{
		{"timescale", "timescale/timescaledb:latest-pg16", "/var/lib/postgresql/data", []string{"-e", "POSTGRES_PASSWORD=fixture-password"}, []string{"psql", "-h", "127.0.0.1", "-U", "postgres", "-Atc"}, "SELECT 1", "CREATE EXTENSION IF NOT EXISTS timescaledb; CREATE TABLE markers(time timestamptz NOT NULL,value text); SELECT create_hypertable('markers','time'); INSERT INTO markers VALUES(now(),'before')", "UPDATE markers SET value='after'", "SELECT value FROM markers"},
		{"clickhouse", "clickhouse/clickhouse-server:24.8-alpine", "/var/lib/clickhouse", []string{"-e", "CLICKHOUSE_SKIP_USER_SETUP=1"}, []string{"clickhouse-client", "--multiquery", "--query"}, "SELECT 1", "CREATE TABLE markers(value String) ENGINE=MergeTree ORDER BY tuple(); INSERT INTO markers VALUES('before')", "TRUNCATE TABLE markers; INSERT INTO markers VALUES('after')", "SELECT value FROM markers"},
		{"redis", "redis:7.2", "/data", nil, []string{"redis-cli"}, "PING", "SET marker before", "SET marker after", "GET marker"},
		{"sqlite", "python:3.12-alpine", "/data", nil, []string{"python", "-c"}, "import sqlite3", "import sqlite3; c=sqlite3.connect('/data/app.db'); c.execute('CREATE TABLE markers(value TEXT)'); c.execute(\"INSERT INTO markers VALUES('before')\"); c.commit()", "import sqlite3; c=sqlite3.connect('/data/app.db'); c.execute(\"UPDATE markers SET value='after'\"); c.commit()", "import sqlite3; print(sqlite3.connect('/data/app.db').execute('SELECT value FROM markers').fetchone()[0])"},
		{"clickhouse23", "clickhouse/clickhouse-server:23.8-alpine", "/var/lib/clickhouse", nil, []string{"clickhouse-client", "--multiquery", "--query"}, "SELECT 1", "CREATE TABLE markers(value String) ENGINE=MergeTree ORDER BY tuple(); INSERT INTO markers VALUES('before')", "TRUNCATE TABLE markers; INSERT INTO markers VALUES('after')", "SELECT value FROM markers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			name := "cove-snapshot-" + tc.name + "-" + filepath.Base(fixture)
			volume := name + "-data"
			root := filepath.Join(fixture, name)
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			e := &Engine{Root: root, Run: RunCommand}
			args := []string{"docker", "run", "-d", "--name", name, "--label", "com.docker.compose.project=cove-" + name, "--label", "com.docker.compose.service=db", "-v", volume + ":" + tc.mount}
			args = append(args, tc.env...)
			args = append(args, tc.image)
			if tc.name == "sqlite" {
				args = append(args, "sleep", "600")
			}
			if err := e.Run(ctx, root, args, nil, io.Discard); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				for _, cmd := range [][]string{{"docker", "rm", "-f", "-v", name}, {"docker", "volume", "rm", volume, "cove-backups-" + name}} {
					if err := e.Run(context.Background(), root, cmd, nil, io.Discard); err != nil {
						t.Error(err)
					}
				}
			})
			query := func(q string) (string, error) {
				args := append([]string{"docker", "exec", name}, tc.cli...)
				if tc.name == "redis" {
					args = append(args, strings.Fields(q)...)
				} else {
					args = append(args, q)
				}
				return e.output(ctx, args...)
			}
			wait := func() {
				t.Helper()
				for i := 0; i < 90; i++ {
					if _, err := query(tc.ready); err == nil {
						return
					}
					time.Sleep(time.Second)
				}
				t.Fatal("database did not become ready")
			}
			wait()
			if _, err := query(tc.seed); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "workloads", name)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(filepath.Join(dir, "current.json"), deployedState{}); err != nil {
				t.Fatal(err)
			}
			r := Request{ID: "snapshot", Workload: name, Action: "backup", Backup: &BackupConfig{Repository: "local", Password: "fixture-backup-password", KeepLast: 3}}
			if err := e.performBackup(ctx, r, io.Discard); err != nil {
				t.Fatal(err)
			}
			wait()
			if _, err := query(tc.change); err != nil {
				t.Fatal(err)
			}
			r.ID = "restore"
			r.Action = "restore"
			r.BackupID = "snapshot"
			if err := e.performBackup(ctx, r, io.Discard); err != nil {
				t.Fatal(err)
			}
			wait()
			value, err := query(tc.read)
			if err != nil || strings.TrimSpace(value) != "before" {
				t.Fatalf("restored data mismatch %s %v", value, err)
			}
		})
	}
}
