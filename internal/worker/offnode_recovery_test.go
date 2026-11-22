package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOffNodeRecovery(t *testing.T) {
	path := os.Getenv("COVE_RECOVERY_FIXTURE")
	if path == "" {
		t.Skip("two-host remote recovery fixture required")
	}
	var fixture struct {
		Workload, Phase string
		Backup          BackupConfig
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if !workloadIdentifier.MatchString(fixture.Workload) || (fixture.Phase != "backup" && fixture.Phase != "restore") {
		t.Fatal("invalid recovery fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	root := filepath.Join(filepath.Dir(path), "engine")
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Root: root, Run: RunCommand}
	db := "cove-" + fixture.Workload + "-database-1"
	app := "cove-" + fixture.Workload + "-app-1"
	dbVolume := "cove-" + fixture.Workload + "-db"
	appVolume := "cove-" + fixture.Workload + "-files"
	t.Cleanup(func() {
		for _, args := range [][]string{{"docker", "rm", "-f", "-v", db, app}, {"docker", "volume", "rm", dbVolume, appVolume}} {
			if err := e.Run(context.Background(), root, args, nil, io.Discard); err != nil {
				t.Error(err)
			}
		}
	})
	for _, spec := range []struct {
		name, service, volume, dest, image string
		extra, command                     []string
	}{
		{db, "database", dbVolume, "/var/lib/postgresql/data", "postgres:17-alpine", []string{"-e", "POSTGRES_PASSWORD=disposable-fixture", "-e", "POSTGRES_DB=app"}, nil},
		{app, "app", appVolume, "/www", "busybox:1.37", nil, []string{"httpd", "-f", "-p", "80", "-h", "/www"}},
	} {
		args := []string{"docker", "run", "-d", "--name", spec.name, "--label", "com.docker.compose.project=cove-" + fixture.Workload, "--label", "com.docker.compose.service=" + spec.service, "-v", spec.volume + ":" + spec.dest}
		args = append(args, spec.extra...)
		args = append(args, spec.image)
		args = append(args, spec.command...)
		if err = e.Run(ctx, root, args, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	query := func(sql string) (string, error) {
		return e.output(ctx, "docker", "exec", db, "psql", "-h", "127.0.0.1", "-U", "postgres", "-d", "app", "-Atc", sql)
	}
	wait := func() {
		t.Helper()
		for attempt := 0; attempt < 60; attempt++ {
			if _, err := query("SELECT 1"); err == nil {
				return
			}
			time.Sleep(time.Second)
		}
		t.Fatal("database readiness timeout")
	}
	wait()
	dir := filepath.Join(root, "workloads", fixture.Workload)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(dir, "current.json"), deployedState{}); err != nil {
		t.Fatal(err)
	}
	request := Request{ID: "offnode-backup", Workload: fixture.Workload, Action: "backup", Backup: &fixture.Backup}
	if fixture.Phase == "backup" {
		if _, err = query("CREATE TABLE recovery_marker(value text); INSERT INTO recovery_marker VALUES('database-recovered')"); err != nil {
			t.Fatal(err)
		}
		if err = e.Run(ctx, root, []string{"docker", "exec", app, "sh", "-c", "printf application-recovered > /www/index.html"}, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
		if err = e.performBackup(ctx, request, io.Discard); err != nil {
			t.Fatal(err)
		}
		t.Log("Encrypted remote backup created; source containers and volumes are removed by cleanup")
		return
	}
	if _, err = os.Stat(filepath.Join(dir, "backups", "offnode-backup.json")); !os.IsNotExist(err) {
		t.Fatal("destination must have no source metadata")
	}
	request.ID = "offnode-restore"
	request.Action = "restore"
	request.BackupID = "offnode-backup"
	if err = e.performBackup(ctx, request, io.Discard); err != nil {
		t.Fatal(err)
	}
	wait()
	value, err := query("SELECT value FROM recovery_marker")
	if err != nil || strings.TrimSpace(value) != "database-recovered" {
		t.Fatal("database data did not recover", err)
	}
	value, err = e.output(ctx, "docker", "exec", app, "wget", "-qO-", "http://127.0.0.1/")
	if err != nil || strings.TrimSpace(value) != "application-recovered" {
		t.Fatal("application HTTP data did not recover", err)
	}
	t.Log("Fresh worker recovered database data and application HTTP response using only encrypted remote storage")
}
