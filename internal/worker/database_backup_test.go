package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDatabaseBackupValidation(t *testing.T) {
	for _, d := range []DatabaseBackup{{Engine: "postgresql", Service: "db", Name: "app"}, {Engine: "mariadb", Service: "db", Name: "app"}} {
		if err := validateDatabaseBackup(&d); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []DatabaseBackup{{Engine: "postgresql", Service: "db", Name: "app';DROP DATABASE app"}, {Engine: "redis", Service: "db", Name: "app"}, {Engine: "postgresql", Service: "../db", Name: "app"}} {
		if validateDatabaseBackup(&d) == nil {
			t.Fatal("accepted unsafe database config")
		}
	}
}

func TestDatabaseLogicalRoundTrip(t *testing.T) {
	fixture := os.Getenv("COVE_DATABASE_FIXTURE")
	if fixture == "" {
		t.Skip("isolated Docker fixture required")
	}
	for _, image := range []string{"postgres:15-alpine", "postgres:16-alpine", "postgres:17-alpine", "postgres:18-alpine", "mariadb:11.8"} {
		t.Run(image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			name := "cove-logical-" + strings.ReplaceAll(strings.ReplaceAll(image, ":", "-"), ".", "-") + "-" + filepath.Base(fixture)
			root := filepath.Join(fixture, name)
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			e := &Engine{Root: root, Run: RunCommand}
			engine := "postgresql"
			environment := []string{"-e", "POSTGRES_PASSWORD=fixture-password", "-e", "POSTGRES_DB=app"}
			sql := []string{"psql", "-U", "postgres", "-d", "app", "-Atc"}
			if strings.HasPrefix(image, "mariadb") {
				engine = "mariadb"
				environment = []string{"-e", "MARIADB_ROOT_PASSWORD=fixture-password", "-e", "MARIADB_DATABASE=app"}
				sql = []string{"sh", "-ec", `export MYSQL_PWD="$MARIADB_ROOT_PASSWORD"; exec mariadb -h 127.0.0.1 -u root -N -B app -e "$1"`, "cove"}
			}
			args := append([]string{"docker", "run", "-d", "--name", name, "--label", "com.docker.compose.project=cove-" + name, "--label", "com.docker.compose.service=db"}, environment...)
			args = append(args, image)
			if err := e.Run(ctx, root, args, nil, io.Discard); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := e.Run(context.Background(), root, []string{"docker", "rm", "-f", "-v", name}, nil, io.Discard); err != nil {
					t.Error(err)
				}
				if err := e.Run(context.Background(), root, []string{"docker", "volume", "rm", "cove-backups-" + name}, nil, io.Discard); err != nil {
					t.Error(err)
				}
			})
			query := func(q string) (string, error) {
				args := append([]string{"docker", "exec", name}, sql...)
				args = append(args, q)
				return e.output(ctx, args...)
			}
			ready := false
			for i := 0; i < 90; i++ {
				if _, err := query("SELECT 1"); err == nil {
					ready = true
					break
				}
				time.Sleep(time.Second)
			}
			if !ready {
				t.Fatal("database did not start")
			}
			if _, err := query("CREATE TABLE markers (id int primary key, value varchar(64)); INSERT INTO markers VALUES (1,'before')"); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "workloads", name)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			snapshotDir := filepath.Join(dir, "release-states")
			if err := os.MkdirAll(snapshotDir, 0700); err != nil {
				t.Fatal(err)
			}
			digest, err := e.output(ctx, "docker", "inspect", "--format", "{{.Image}}", name)
			if err != nil {
				t.Fatal(err)
			}
			mountJSON, err := e.output(ctx, "docker", "inspect", "--format", "{{json .Mounts}}", name)
			if err != nil {
				t.Fatal(err)
			}
			var originalMounts []backupMount
			if err = json.Unmarshal([]byte(mountJSON), &originalMounts); err != nil {
				t.Fatal(err)
			}
			if len(originalMounts) != 1 {
				t.Fatal("expected database data volume", originalMounts)
			}
			values := map[string]string{}
			for i := 1; i < len(environment); i += 2 {
				key, value, _ := strings.Cut(environment[i], "=")
				values[key] = value
			}
			snapshot := map[string]any{"services": map[string]any{"db": map[string]any{"image": digest, "pull_policy": "never", "environment": values, "volumes": []any{map[string]any{"type": "volume", "source": "data", "target": originalMounts[0].Destination}}}}, "volumes": map[string]any{"data": map[string]any{"name": originalMounts[0].Name}}}
			snapshotPath := filepath.Join(snapshotDir, "release.compose.json")
			if err = writeJSON(snapshotPath, literalCompose(snapshot)); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(filepath.Join(dir, "current.json"), deployedState{ComposePath: snapshotPath, Environment: values}); err != nil {
				t.Fatal(err)
			}
			r := Request{ID: "backup", Workload: name, Action: "backup", Backup: &BackupConfig{Database: &DatabaseBackup{Engine: engine, Service: "db", Name: "app"}, Repository: "local", Password: "fixture-backup-password", KeepLast: 3}}
			before, err := e.output(ctx, "docker", "inspect", "--format", "{{.State.StartedAt}}", name)
			if err != nil {
				t.Fatal(err)
			}
			if err = e.performBackup(ctx, r, io.Discard); err != nil {
				t.Fatal(err)
			}
			if _, err = query("UPDATE markers SET value='after'"); err != nil {
				t.Fatal(err)
			}

			drill := r
			drill.ID = "drill"
			drill.Action = "drill"
			drill.BackupID = "backup"
			if err = e.performDrill(ctx, drill, io.Discard); err != nil {
				t.Fatal("restore drill", err)
			}
			valueAfterDrill, err := query("SELECT value FROM markers WHERE id=1")
			if err != nil || strings.TrimSpace(valueAfterDrill) != "after" {
				t.Fatal("drill changed source database", valueAfterDrill, err)
			}
			startedAfterDrill, err := e.output(ctx, "docker", "inspect", "--format", "{{.State.StartedAt}}", name)
			if err != nil || startedAfterDrill != before {
				t.Fatal("drill restarted source database", err)
			}
			records, err := e.Backups(name)
			if err != nil || len(records) != 1 || records[0].VerifiedAt == nil || records[0].Verification != "database" {
				t.Fatal("drill verification not recorded", records, err)
			}
			copyRequest := r
			copyRequest.ID = "copy"
			copyRequest.Action = "restore_copy"
			copyRequest.BackupID = "backup"
			copyRequest.Workload = name + "-copy"
			copyRequest.RestoreCopy = &RestoreCopy{Source: name}
			plan, err := e.PlanRestoreCopy(ctx, copyRequest, name)
			if err != nil {
				t.Fatal("copy plan", err)
			}
			copyRequest.Compose = plan.Compose
			copyRequest.Environment = plan.Environment
			copyDir := filepath.Join(root, "workloads", copyRequest.Workload)
			if err = os.MkdirAll(copyDir, 0700); err != nil {
				t.Fatal(err)
			}
			var copyLog strings.Builder
			t.Cleanup(func() {
				cleanup := context.Background()
				_ = e.Run(cleanup, root, []string{"docker", "rm", "-f", "-v", "cove-copy-db-copy"}, nil, io.Discard)
				_ = e.Run(cleanup, copyDir, []string{"docker", "compose", "--project-name", "cove-" + copyRequest.Workload, "--file", filepath.Join(copyDir, "restore-copy.compose.json"), "down"}, nil, io.Discard)
				for _, volume := range plan.Volumes {
					if err := e.Run(cleanup, root, []string{"docker", "volume", "rm", volume}, nil, io.Discard); err != nil {
						t.Error(err)
					}
				}
			})
			if err = e.performRestoreCopy(ctx, copyRequest, &copyLog); err != nil {
				t.Fatal("copy restore", err, copyLog.String())
			}
			if _, err = os.Stat(filepath.Join(copyDir, "copy-recovery.json")); !os.IsNotExist(err) {
				t.Fatal("copy remained blocked", err)
			}
			copyArgs := []string{"docker", "compose", "--project-name", "cove-" + copyRequest.Workload, "--file", filepath.Join(copyDir, "restore-copy.compose.json")}
			copyEnv := []string{}
			for key, value := range plan.Environment {
				copyEnv = append(copyEnv, key+"="+value)
			}
			if err = e.Run(ctx, copyDir, append(append([]string{}, copyArgs...), "up", "--detach", "--no-build", "--wait"), copyEnv, &copyLog); err != nil {
				t.Fatal(err, copyLog.String())
			}
			copied := ""
			for attempt := 0; attempt < 60; attempt++ {
				args := append(append([]string{}, copyArgs...), "exec", "-T", "db")
				args = append(args, sql...)
				args = append(args, "SELECT value FROM markers WHERE id=1")
				output := &limitedBuffer{Limit: 4096}
				if e.Run(ctx, copyDir, args, copyEnv, output) == nil {
					copied = strings.TrimSpace(output.data.String())
					break
				}
				time.Sleep(time.Second)
			}
			if copied != "before" {
				t.Fatal("copy data mismatch", copied, copyLog.String())
			}
			sourceValue, err := query("SELECT value FROM markers WHERE id=1")
			if err != nil || strings.TrimSpace(sourceValue) != "after" {
				t.Fatal("copy changed source", sourceValue, err)
			}
			r.ID = "restore"
			r.Action = "restore"
			r.BackupID = "backup"
			if err = e.performBackup(ctx, r, io.Discard); err != nil {
				t.Fatal(err)
			}
			value, err := query("SELECT value FROM markers WHERE id=1")
			if err != nil || strings.TrimSpace(value) != "before" {
				t.Fatalf("restore mismatch: %s %v", value, err)
			}
			after, err := e.output(ctx, "docker", "inspect", "--format", "{{.State.StartedAt}}", name)
			if err != nil || before != after {
				t.Fatal("logical backup or restore restarted database", err)
			}
			if _, err = os.Stat(filepath.Join(dir, "backups", "restore-before.json")); err != nil {
				t.Fatal("missing safety snapshot", err)
			}
			if image == "postgres:15-alpine" {
				r.ID = "failed-restore"
				e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
					if len(args) > 5 && args[0] == "docker" && args[1] == "exec" && strings.Contains(args[5], "exec pg_restore") {
						return errors.New("injected restore interruption")
					}
					return RunCommand(ctx, dir, args, env, out)
				}
				if err = e.performBackup(ctx, r, io.Discard); err == nil {
					t.Fatal("restore interruption ignored")
				}
				status, err := e.output(ctx, "docker", "inspect", "--format", "{{.State.Status}}", name)
				if err != nil || status != "exited" {
					t.Fatal("failed restore did not stop database", status, err)
				}
				if _, err = os.Stat(filepath.Join(dir, "storage-recovery.json")); err != nil {
					t.Fatal("missing restore recovery marker", err)
				}
				e.Run = RunCommand
				r.ID = "retry-restore"
				if err = e.performBackup(ctx, r, io.Discard); err != nil {
					t.Fatal("restore retry failed", err)
				}
				value, err = query("SELECT value FROM markers WHERE id=1")
				if err != nil || strings.TrimSpace(value) != "before" {
					t.Fatal("retry did not restore data", err)
				}
				if _, err = os.Stat(filepath.Join(dir, "storage-recovery.json")); !os.IsNotExist(err) {
					t.Fatal("recovery marker not cleared", err)
				}
			}

		})
	}
}
