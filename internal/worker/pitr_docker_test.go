package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPITRCaptureDocker(t *testing.T) {
	testPITRCaptureDocker(t, false)
}

func TestPITRS3CaptureDocker(t *testing.T) {
	testPITRCaptureDocker(t, true)
}

func TestPITRPostgres18Docker(t *testing.T) {
	t.Setenv("COVE_TEST_POSTGRES_18", "1")
	testPITRCaptureDocker(t, false)
}

func testPITRCaptureDocker(t *testing.T, remote bool) {
	fixture := os.Getenv("COVE_DATABASE_FIXTURE")
	binary := os.Getenv("COVE_TEST_WORKER_BINARY")
	if fixture == "" || binary == "" {
		t.Skip("disposable Docker fixture and Linux worker binary required")
	}
	scope := "local"
	if remote {
		scope = "remote"
	}
	f := newDatabaseImportFixture(t, filepath.Join(fixture, filepath.Base(fixture)+"-"+scope), "postgresql", 12*time.Minute)
	e := f.engine
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		if len(args) > 2 && args[0] == "docker" && args[1] == "run" {
			replay := false
			for i, arg := range args {
				if arg == "--name" && i+1 < len(args) && strings.HasPrefix(args[i+1], "cove-pitr-replay-") {
					replay = true
				}
			}
			if replay {
				args = append([]string{}, args...)
				for i, arg := range args {
					if arg == "--log-driver" {
						args[i+1] = "local"
						break
					}
				}
				args = append(args[:2], append([]string{"--log-opt", "max-size=64k", "--log-opt", "max-file=2"}, args[2:]...)...)
			}
		}
		captured := &bytes.Buffer{}
		err := RunCommand(ctx, dir, args, env, io.MultiWriter(out, captured))
		if err != nil {
			t.Logf("command %v: %s", args[:min(3, len(args))], captured.String())
		}
		if len(args) > 4 && args[1] == "inspect" && strings.Contains(strings.Join(args, " "), ".State.Status") && strings.TrimSpace(captured.String()) == "exited" {
			RunCommand(ctx, dir, []string{"docker", "logs", "--tail", "25", args[len(args)-1]}, nil, os.Stdout)
		}
		return err
	}
	e.hostHelperBinary = binary
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	r := f.request
	if remote {
		r.Backup.Remote = pitrS3Fixture(t, fixture, e)
		r.Backup.Repository = "rclone:cove:" + r.Backup.Remote.Path + "/" + r.Workload
	}
	r.ID = "pitr-configure"
	r.Action = "pitr_configure"
	r.PITR = &PITRPolicy{Key: strings.Repeat("17", 32), Archive: strings.Repeat("18", 16), Settings: PITRSettings{Enabled: true, Service: "db", IntervalHours: 24, KeepLast: 2, RetentionDays: 7, MaxBytes: 1 << 30}}
	t.Cleanup(func() {
		if state, err := e.loadPITR(r.Workload); err == nil {
			e.stopPITR(context.Background(), r.Workload)
			name := "cove-pitr-image-" + r.Workload
			if owner, err := e.output(context.Background(), "docker", "inspect", "--format", `{{index .Config.Labels "dev.cove.pitr.owner"}}`, name); err == nil && owner == state.Config.Owner {
				e.Run(context.Background(), e.Root, []string{"docker", "rm", "--volumes", name}, nil, io.Discard)
			}
			if !remote {
				volume := "cove-pitr-archive-" + r.Workload
				if owner, err := e.output(context.Background(), "docker", "volume", "inspect", "--format", `{{index .Labels "dev.cove.pitr.owner"}}`, volume); err == nil && owner == state.Config.Owner {
					if err = e.Run(context.Background(), e.Root, []string{"docker", "volume", "rm", volume}, nil, io.Discard); err != nil {
						t.Error(err)
					}
				}
			}
		}
	})
	if err := e.configurePITR(ctx, r); err != nil {
		if info, inspectErr := os.Stat(filepath.Join(e.Root, "host-snapshot-helper")); inspectErr == nil {
			t.Log("host helper", info.Mode(), info.Size())
		}
		e.Run(ctx, e.Root, []string{"docker", "run", "--rm", "--network", "none", "--volume", e.Root + ":/check:ro", "busybox:1.37", "ls", "-l", "/check/host-snapshot-helper"}, nil, os.Stdout)
		t.Fatal(err)
	}
	state, err := e.loadPITR(r.Workload)
	if err != nil {
		t.Fatal(err)
	}
	started, err := e.output(ctx, "docker", "inspect", "--format", "{{.State.StartedAt}}", f.container)
	if err != nil {
		t.Fatal(err)
	}
	r.ID, r.Action = "pitr-base", "pitr_backup"
	if err = e.backupPITR(ctx, r); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(40 * time.Second)
	var status PITRStatus
	for time.Now().Before(deadline) {
		status, err = e.PITRStatus(ctx, r.Workload)
		if err == nil && len(status.Bases) == 1 && status.LastArchiveAt != nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil || len(status.Bases) != 1 || !status.Receiver || status.State != "capturing" || status.Bytes == 0 || status.LastError != "" {
		t.Fatalf("capture status: %+v, %v", status, err)
	}
	if after, err := e.output(ctx, "docker", "inspect", "--format", "{{.State.StartedAt}}", f.container); err != nil || after != started {
		t.Fatal("base backup restarted source", err)
	}
	if err = e.stopPITR(ctx, r.Workload); err != nil {
		t.Fatal(err)
	}
	if err = e.startPITR(ctx, state); err != nil {
		t.Fatal(err)
	}
	if f.value() != "before" {
		t.Fatal("capture changed source")
	}
	f.set("wanted")
	stamp, err := e.pitrSQL(ctx, f.container, "SELECT to_json(clock_timestamp());")
	var targetTime time.Time
	if err != nil || json.Unmarshal(stamp, &targetTime) != nil {
		t.Fatal("target timestamp", err)
	}
	f.set("after")
	if _, err = e.pitrSQL(ctx, f.container, "SELECT pg_switch_wal();"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		status, err = e.PITRStatus(ctx, r.Workload)
		if err == nil && status.LastArchiveAt != nil && status.LastArchiveAt.After(targetTime) && len(status.Bases) > 0 && status.Bases[0].AvailableTo != nil && !status.Bases[0].AvailableTo.Before(targetTime) {
			break
		}
		time.Sleep(time.Second)
	}
	copyID := "pitr-copy-" + filepath.Base(fixture) + "-" + scope
	r.PITR.Settings.Enabled = false
	r.ID, r.Action = "pitr-disable-before-copy", "pitr_configure"
	if err = e.configurePITR(ctx, r); err != nil {
		t.Fatal(err)
	}
	if disabled, err := e.PITRStatus(ctx, r.Workload); err != nil || len(disabled.Bases) != 1 || disabled.Bases[0].AvailableTo == nil || disabled.Bases[0].AvailableTo.Before(targetTime) {
		t.Fatal("disabled history lost its verified recovery window", disabled, err)
	}
	if err = os.MkdirAll(filepath.Join(e.Root, "workloads", copyID), 0700); err != nil {
		t.Fatal(err)
	}
	copyRequest := Request{ID: copyID, Workload: copyID, Action: "pitr_restore", PITRRestore: &PITRRestore{Source: r.Workload, Base: status.Bases[0].ID, Timestamp: targetTime}, Environment: map[string]string{"COVE_PITR_PASSWORD": strings.Repeat("synthetic-copy-password", 3)}}
	plan, err := e.PlanPITRCopy(ctx, copyRequest)
	if err != nil {
		t.Fatal(err)
	}
	copyRequest.Compose, copyRequest.Environment = plan.Compose, plan.Environment
	t.Cleanup(func() {
		e.Run(context.Background(), e.Root, []string{"docker", "rm", "-f", "--volumes", "cove-" + copyID, "cove-pitr-image-" + copyID}, nil, io.Discard)
		e.Run(context.Background(), e.Root, []string{"docker", "volume", "rm", plan.Volumes[0]}, nil, io.Discard)
	})
	if err = e.performPITRCopy(ctx, copyRequest, os.Stdout); err != nil {
		t.Fatal(err)
	}
	dataDirectory := pitrCopyDataDirectory(state.Config.Database.Major)
	args := []string{"docker", "run", "-d", "--name", "cove-" + copyID, "--network", "none", "-e", "PGDATA=" + dataDirectory, "--volume", plan.Volumes[0] + ":" + dataDirectory, state.Source.Image}
	if err = e.Run(ctx, e.Root, args, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if e.Run(ctx, e.Root, []string{"docker", "exec", "cove-" + copyID, "pg_isready", "-U", "postgres"}, nil, io.Discard) == nil {
			break
		}
		time.Sleep(time.Second)
	}
	restored, err := e.output(ctx, "docker", "exec", "cove-"+copyID, "psql", "-U", "postgres", "-d", "app", "-Atc", "SELECT value FROM markers")
	if err != nil || restored != "wanted" || f.value() != "after" {
		t.Fatal("timestamp recovery or source isolation", restored, err)
	}
	r.PITR.Settings.Enabled = false
	r.ID, r.Action = "pitr-disable", "pitr_configure"
	if err = e.configurePITR(ctx, r); err != nil {
		t.Fatal(err)
	}
	data, err := e.pitrSQL(ctx, f.container, "SELECT count(*) FROM pg_replication_slots WHERE slot_name="+pitrSQLLiteral(state.Source.Database.Slot)+";")
	if err != nil || strings.TrimSpace(string(data)) != "0" {
		t.Fatal("owned slot retained after disable", err)
	}
	data, err = e.pitrSQL(ctx, f.container, "SHOW max_slot_wal_keep_size;")
	if err != nil || strings.TrimSpace(string(data)) != state.Source.OriginalSlotLimit {
		t.Fatal("source WAL retention not restored", err)
	}
	for range 10 {
		f.set("after")
		if _, err = e.pitrSQL(ctx, f.container, "SELECT pg_switch_wal();"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = e.pitrSQL(ctx, f.container, "CHECKPOINT;"); err != nil {
		t.Fatal(err)
	}
	r.PITR.Settings.Enabled = true
	r.ID, r.Action = "pitr-resume", "pitr_configure"
	if err = e.configurePITR(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.ID, r.Action = "pitr-new-base", "pitr_backup"
	if err = e.backupPITR(ctx, r); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		status, err = e.PITRStatus(ctx, r.Workload)
		if err == nil && len(status.Bases) == 2 && status.Bases[0].AvailableTo != nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil || len(status.Bases) != 2 || status.Bases[0].AvailableTo == nil {
		t.Fatal("new recovery span unavailable after resumed capture", err, status)
	}
	quiet := Request{ID: copyID + "-quiet", Workload: copyID + "-quiet", Action: "pitr_restore", PITRRestore: &PITRRestore{Source: r.Workload, Base: status.Bases[0].ID, Timestamp: *status.Bases[0].AvailableTo}, Environment: map[string]string{"COVE_PITR_PASSWORD": strings.Repeat("synthetic-quiet-copy", 3)}}
	if err = os.MkdirAll(filepath.Join(e.Root, "workloads", quiet.Workload), 0700); err != nil {
		t.Fatal(err)
	}
	quietPlan, err := e.PlanPITRCopy(ctx, quiet)
	if err != nil {
		t.Fatal(err)
	}
	quiet.Compose, quiet.Environment = quietPlan.Compose, quietPlan.Environment
	t.Cleanup(func() {
		e.Run(context.Background(), e.Root, []string{"docker", "rm", "-f", "--volumes", "cove-pitr-image-" + quiet.Workload}, nil, io.Discard)
		e.Run(context.Background(), e.Root, []string{"docker", "volume", "rm", quietPlan.Volumes[0]}, nil, io.Discard)
	})
	if err = e.performPITRCopy(ctx, quiet, os.Stdout); err != nil {
		t.Fatal("quiet-source heartbeat timestamp recovery", err)
	}
	r.PITR.Settings.Enabled = false
	r.ID, r.Action = "pitr-disable-final", "pitr_configure"
	if err = e.configurePITR(ctx, r); err != nil {
		t.Fatal(err)
	}
	if disabled, err := e.PITRStatus(ctx, r.Workload); err != nil || disabled.State != "disabled" || disabled.Receiver || disabled.Enabled {
		t.Fatal("capture disable accepted a stale helper heartbeat", disabled, err)
	} else if len(disabled.Bases) != 2 || disabled.Bases[0].AvailableTo == nil || disabled.Bases[1].AvailableTo == nil {
		t.Fatal("disabled history lost a retained recovery span", disabled)
	}
	r.ID, r.Action = "pitr-forget", "pitr_forget"
	if err = e.forgetPITR(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(e.pitrDirectory(r.Workload), "state.json")); !os.IsNotExist(err) {
		t.Fatal("private PITR state retained", err)
	}
	if f.value() != "after" {
		t.Fatal("history deletion changed source")
	}
}
