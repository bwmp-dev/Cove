package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtendedDatabaseRoundTrip(t *testing.T) {
	fixture := os.Getenv("COVE_DATABASE_FIXTURE")
	if fixture == "" {
		t.Skip("disposable Docker database fixture required")
	}
	for _, engineCase := range []string{"redis", "valkey", "mongodb", "valkey-auth", "mongodb-auth"} {
		t.Run(engineCase, func(t *testing.T) {
			engine := strings.TrimSuffix(engineCase, "-auth")
			authenticated := strings.HasSuffix(engineCase, "-auth")
			values := map[string]string{}
			if authenticated {
				if engine == "mongodb" {
					values["MONGO_INITDB_ROOT_USERNAME"] = "fixture"
					values["MONGO_INITDB_ROOT_PASSWORD"] = "synthetic-mongo-password"
				} else {
					values["VALKEY_PASSWORD"] = "synthetic-valkey-password"
				}
			}
			mongoQuery := func(value string) string {
				return mongoCredentials + `if(user&&!db.getSiblingDB("admin").auth(user,password))quit(1);` + value
			}
			mongoStart := `if [ ! -f /tmp/cove-fixture.key ]; then printf %s SyntheticMongoReplicationKeyForLocalFixtureOnly0123456789 > /tmp/cove-fixture.key; chmod 600 /tmp/cove-fixture.key; chown mongodb:mongodb /tmp/cove-fixture.key; fi; exec docker-entrypoint.sh mongod --replSet rs0 --bind_ip_all --keyFile /tmp/cove-fixture.key`
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			workload := "extended-" + engineCase + "-" + filepath.Base(fixture)
			root := filepath.Join(fixture, engineCase)
			if err := os.MkdirAll(filepath.Join(root, "workloads", workload), 0700); err != nil {
				t.Fatal(err)
			}
			e := &Engine{Root: root, Run: RunCommand}
			run := func(args ...string) string {
				t.Helper()
				out, err := e.output(ctx, args...)
				if err != nil {
					t.Fatal(args[:min(3, len(args))], err, out)
				}
				return strings.TrimSpace(out)
			}
			name := "cove-" + workload
			image := engine + ":8-alpine"
			if engine == "valkey" {
				image = "valkey/valkey:8-alpine"
			}
			if engine == "mongodb" {
				image = "mongo:8.0"
			}
			run("docker", "pull", image)
			mountTarget := "/data"
			if engine == "mongodb" {
				mountTarget = "/data/db"
			}
			args := []string{"docker", "run", "-d", "--name", name, "--label", "com.docker.compose.project=cove-" + workload, "--label", "com.docker.compose.service=db", "--volume", name + "-data:" + mountTarget}
			for key, value := range values {
				args = append(args, "-e", key+"="+value)
			}
			args = append(args, image)
			if engine == "mongodb" {
				if authenticated {
					args = append(args, "sh", "-ec", mongoStart)
				} else {
					args = append(args, "--replSet", "rs0", "--bind_ip_all")
				}
			} else {
				args = append(args, "--appendonly", "yes")
				if authenticated {
					args = append(args, "--requirepass", values["VALKEY_PASSWORD"])
				}
			}
			run(args...)
			t.Cleanup(func() {
				if err := e.Run(context.Background(), root, []string{"docker", "rm", "-f", "-v", name}, nil, io.Discard); err != nil {
					t.Error(err)
				}
				e.Run(context.Background(), root, []string{"docker", "volume", "rm", "cove-backups-" + workload, name + "-data"}, nil, io.Discard)
			})
			ready := false
			for attempt := 0; attempt < 90; attempt++ {
				cmd := []string{"docker", "exec", name, "sh", "-ec", redisAuth + redisCLI(engine) + " PING"}
				if engine == "mongodb" {
					cmd = []string{"docker", "exec", name, "mongosh", "--quiet", "--eval", mongoQuery(`const opts=db.adminCommand({getCmdLineOpts:1}).parsed;if(!opts.replication)quit(1);db.adminCommand({ping:1});`)}
				}
				if e.Run(ctx, root, cmd, nil, io.Discard) == nil {
					ready = true
					break
				}
				time.Sleep(time.Second)
			}
			if !ready {
				t.Fatal("source database not ready")
			}
			query := func(value string) string {
				if engine == "mongodb" {
					return run("docker", "exec", name, "mongosh", "--quiet", "--eval", mongoQuery(value))
				}
				return run("docker", "exec", name, "sh", "-ec", redisAuth+redisCLI(engine)+` --raw GET "$1"`, "cove", value)
			}
			if engine == "mongodb" {
				query(`rs.initiate({_id:"rs0",members:[{_id:0,host:"127.0.0.1:27017"}]})`)
				for attempt := 0; attempt < 60; attempt++ {
					if strings.Contains(query(`db.hello().isWritablePrimary`), "true") {
						break
					}
					time.Sleep(time.Second)
				}
				query(`db.getSiblingDB("app").markers.insertOne({_id:1,value:"before"})`)
			} else {
				run("docker", "exec", name, "sh", "-ec", redisAuth+redisCLI(engine)+" SET marker before")
			}
			releaseDir := filepath.Join(root, "workloads", workload, "release-states")
			if err := os.MkdirAll(releaseDir, 0700); err != nil {
				t.Fatal(err)
			}
			compose := filepath.Join(releaseDir, "release.compose.json")
			digest := run("docker", "inspect", "--format", "{{.Image}}", name)
			service := map[string]any{"image": digest, "environment": values, "pull_policy": "never", "volumes": []any{map[string]any{"type": "volume", "source": "data", "target": mountTarget}}}
			if engine == "mongodb" {
				if authenticated {
					service["command"] = []string{"sh", "-ec", mongoStart}
				} else {
					service["command"] = []string{"mongod", "--replSet", "rs0", "--bind_ip_all"}
				}
			} else {
				command := []string{engine + "-server", "--appendonly", "yes"}
				if authenticated {
					command = append(command, "--requirepass", values["VALKEY_PASSWORD"])
				}
				service["command"] = command
			}
			if err := writeJSON(compose, map[string]any{"services": map[string]any{"db": service}, "volumes": map[string]any{"data": map[string]any{"name": name + "-data"}}}); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(filepath.Join(root, "workloads", workload, "current.json"), deployedState{ComposePath: compose, Environment: values}); err != nil {
				t.Fatal(err)
			}
			r := Request{ID: "backup", Workload: workload, Action: "backup", Backup: &BackupConfig{Database: &DatabaseBackup{Engine: engine, Service: "db", Name: "all"}, Repository: "local", Password: "fixture-repository-password", KeepLast: 3}}
			before := run("docker", "inspect", "--format", "{{.State.StartedAt}}", name)
			var log strings.Builder
			if err := e.performBackup(ctx, r, &log); err != nil {
				t.Fatal(err, log.String())
			}
			if after := run("docker", "inspect", "--format", "{{.State.StartedAt}}", name); after != before {
				t.Fatal("backup stopped the source database")
			}
			records, err := e.Backups(workload)
			if err != nil || len(records) != 1 || records[0].DataSnapshot == "" || records[0].VerifiedAt == nil {
				t.Fatal(records, err)
			}
			backupID := "backup"
			if engine == "redis" || engine == "valkey" {
				backupID = legacyRDBBackupForTest(t, ctx, e, r, name)
			}
			if engine == "mongodb" {
				query(`db.getSiblingDB("app").markers.updateOne({_id:1},{$set:{value:"after"}})`)
			} else {
				run("docker", "exec", name, "sh", "-ec", redisAuth+redisCLI(engine)+" SET marker after")
			}
			drill := r
			drill.ID, drill.Action, drill.BackupID = "drill", "drill", backupID
			if err = e.performDrill(ctx, drill, &log); err != nil {
				t.Fatal("restore drill", err, log.String())
			}
			if run("docker", "inspect", "--format", "{{.State.StartedAt}}", name) != before {
				t.Fatal("drill stopped source")
			}
			copyRequest := r
			copyRequest.ID, copyRequest.Action, copyRequest.BackupID, copyRequest.Workload = "copy", "restore_copy", backupID, workload+"-copy"
			copyRequest.RestoreCopy = &RestoreCopy{Source: workload}
			plan, err := e.PlanRestoreCopy(ctx, copyRequest, workload)
			if err != nil {
				t.Fatal("restore copy plan", err)
			}
			copyRequest.Compose, copyRequest.Environment = plan.Compose, plan.Environment
			copyDir := filepath.Join(root, "workloads", copyRequest.Workload)
			if err = os.MkdirAll(copyDir, 0700); err != nil {
				t.Fatal(err)
			}
			copyArgs := []string{"docker", "compose", "--project-name", "cove-" + copyRequest.Workload, "--file", filepath.Join(copyDir, "restore-copy.compose.json")}
			t.Cleanup(func() {
				e.Run(context.Background(), root, []string{"docker", "rm", "-f", "-v", "cove-copy-db-copy"}, nil, io.Discard)
				e.Run(context.Background(), root, append(append([]string{}, copyArgs...), "down"), nil, io.Discard)
				for _, v := range plan.Volumes {
					if err := e.Run(context.Background(), root, []string{"docker", "volume", "rm", v}, nil, io.Discard); err != nil {
						t.Error(err)
					}
				}
			})
			e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
				if strings.Contains(strings.Join(args, " "), "--archive=/tmp/cove-database-restore.archive") {
					out = &log
				}
				return RunCommand(ctx, dir, args, env, out)
			}
			if err = e.performRestoreCopy(ctx, copyRequest, &log); err != nil {
				t.Fatal("restore copy", err, log.String())
			}
			e.Run = RunCommand
			copyEnv := []string{}
			for key, value := range plan.Environment {
				copyEnv = append(copyEnv, key+"="+value)
			}
			if err = e.Run(ctx, root, append(append([]string{}, copyArgs...), "up", "--detach", "--no-build"), copyEnv, &log); err != nil {
				t.Fatal(err, log.String())
			}
			copyContainer := run("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+copyRequest.Workload, "--filter", "label=com.docker.compose.service=db")
			if err = e.waitExtendedDatabase(ctx, engine, copyContainer); err != nil {
				t.Fatal(err)
			}
			valueCommand := []string{"docker", "exec", copyContainer, "sh", "-ec", redisAuth + redisCLI(engine) + " --raw GET marker"}
			if engine == "mongodb" {
				valueCommand = []string{"docker", "exec", copyContainer, "mongosh", "--quiet", "--eval", mongoQuery(`db.getSiblingDB("app").markers.findOne({_id:1}).value`)}
			}
			if copied := run(valueCommand...); copied != "before" {
				t.Fatal("restore copy lost data", copied)
			}
			if run("docker", "inspect", "--format", "{{.State.StartedAt}}", name) != before {
				t.Fatal("restore copy stopped source")
			}
			r.ID = "restore-failed"
			r.Action = "restore"
			r.BackupID = backupID
			injected := false
			e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
				joined := strings.Join(args, " ")
				if !injected && ((engine != "mongodb" && strings.Contains(joined, "rm -f -- /target/dump.rdb")) || (engine == "mongodb" && strings.Contains(joined, "--archive=/tmp/cove-database-restore.archive"))) {
					injected = true
					return errors.New("injected restore failure")
				}
				return RunCommand(ctx, dir, args, env, out)
			}
			if err = e.performBackup(ctx, r, &log); err == nil || !injected {
				t.Fatal("restore failure was not injected", err, log.String())
			}
			if run("docker", "inspect", "--format", "{{.State.Running}}", name) != "false" {
				t.Fatal("failed restore left database running")
			}
			e.Run = RunCommand
			r.ID = "restore"
			if err = e.performBackup(ctx, r, &log); err != nil {
				t.Fatal(err, log.String())
			}
			value := ""
			if engine == "mongodb" {
				value = query(`db.getSiblingDB("app").markers.findOne({_id:1}).value`)
			} else {
				value = query("marker")
			}
			if value != "before" {
				t.Fatal("database restore lost data", value)
			}
			if engine == "mongodb" && query(`db.getSiblingDB("admin").getUsers().users.filter(u=>u.user.startsWith("cove_restore_")).length`) != "0" {
				t.Fatal("temporary restore account retained")
			}
			if _, err = os.Stat(filepath.Join(root, "workloads", workload, "backups", "restore-failed-before.json")); err != nil {
				t.Fatal("safety snapshot missing", err)
			}
		})
	}
}
