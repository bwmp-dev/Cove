package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func maintenanceFixture() DatabaseMaintenance {
	return DatabaseMaintenance{ID: "maintenance", Kind: "extension", Engine: "postgresql", Service: "postgres", PasswordKey: "POSTGRES_PASSWORD", Extension: "pg_trgm", Request: Request{ID: "maintenance", Workload: "database", Action: "deploy", Kind: "service", Compose: "services:\n  postgres:\n    image: postgres:18.7-alpine\n", Environment: map[string]string{"POSTGRES_PASSWORD": strings.Repeat("a", 64)}}}
}

func TestDatabaseMaintenanceJournalRetriesWithoutRepeatingCompletedChange(t *testing.T) {
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls, fail := 0, true
	engine.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		if args[1] == "ps" {
			_, err := io.WriteString(out, "container")
			return err
		}
		calls++
		if fail {
			return errors.New("deliberate failure")
		}
		return nil
	}
	in := maintenanceFixture()
	if engine.MaintainDatabase(context.Background(), in) == nil {
		t.Fatal("unconfirmed change succeeded")
	}
	fail = false
	if err = engine.MaintainDatabase(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if err = engine.MaintainDatabase(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("completed maintenance was repeated", calls)
	}
	saved, err := os.ReadFile(filepath.Join(engine.Root, "database-maintenance", "maintenance.json"))
	if err != nil || strings.Contains(string(saved), strings.Repeat("a", 64)) {
		t.Fatal("phase journal leaked credentials", err)
	}
	in.Extension = "hstore"
	if engine.MaintainDatabase(context.Background(), in) == nil {
		t.Fatal("accepted changed input for retained ID")
	}
}

func TestDatabaseRotationNeverPlacesGeneratedSecretsInArguments(t *testing.T) {
	for _, engineID := range []string{"postgresql", "mysql", "mariadb", "mongodb", "redis", "valkey", "clickhouse"} {
		t.Run(engineID, func(t *testing.T) {
			engine, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			next, previous := strings.Repeat("b", 64), strings.Repeat("a", 64)
			in := maintenanceFixture()
			in.Engine = engineID
			in.Kind = "rotate"
			in.PreviousPassword = previous
			in.Request.Environment[in.PasswordKey] = next
			engine.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
				for _, arg := range args {
					if strings.Contains(arg, next) || strings.Contains(arg, previous) {
						t.Fatal("secret included in arguments")
					}
				}
				return nil
			}
			if err = engine.rotateDatabase(context.Background(), in, "container"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDatabaseMaintenanceRejectsUnsafeEnvelopeAndExtensions(t *testing.T) {
	for _, extension := range []string{"pg_trgm|hstore", "hstore|uuid-ossp", "pg_stat_statements|vector|postgis"} {
		in := maintenanceFixture()
		in.Extension = extension
		if ValidateDatabaseMaintenance(in) == nil {
			t.Fatal("combined catalogue names accepted as one extension", extension)
		}
	}
	in := maintenanceFixture()
	in.Extension = `pg_trgm";DROP DATABASE app;--`
	if ValidateDatabaseMaintenance(in) == nil {
		t.Fatal("arbitrary extension accepted")
	}
	in = maintenanceFixture()
	outer := in.Request
	outer.DatabaseMaintenance = &in
	outer.Workload = "different"
	if ValidateRequest(outer) == nil {
		t.Fatal("cross-resource envelope accepted")
	}
	in = maintenanceFixture()
	in.Request.DatabaseMaintenance = &in
	if ValidateRequest(in.Request) == nil {
		t.Fatal("nested maintenance envelope accepted")
	}
}

func TestDatabaseRetryAcknowledgesQueuedAndCompletedOperationsIdempotently(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	in := maintenanceFixture()
	request := in.Request
	request.DatabaseMaintenance = &in
	op, err := e.Submit(request)
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.read(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	j.Operation.State = "interrupted"
	if err = writeJSON(e.path(op.ID), j); err != nil {
		t.Fatal(err)
	}
	e.pending = nil
	rpc := RPCRequest{Method: "database_retry", ID: op.ID, Request: &request}
	for range 2 {
		reply := e.RPC(context.Background(), rpc)
		if reply.Error != "" || reply.Operation == nil || reply.Operation.State != "queued" {
			t.Fatal("retry not acknowledged", reply.Error)
		}
	}
	if len(e.pending) != 1 {
		t.Fatal("ambiguous retry duplicated queued work")
	}
	j.Operation.State = "succeeded"
	if err = writeJSON(e.path(op.ID), j); err != nil {
		t.Fatal(err)
	}
	reply := e.RPC(context.Background(), rpc)
	if reply.Error != "" || reply.Operation == nil || reply.Operation.State != "succeeded" || len(e.pending) != 1 {
		t.Fatal("completed retry was restarted")
	}
}

func TestDatabaseMaintenanceFailureHasBoundedOperationLog(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	in := maintenanceFixture()
	in.PreviousPassword = strings.Repeat("b", 64)
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		if args[1] == "ps" {
			_, err := io.WriteString(out, "container")
			return err
		}
		return errors.New(in.PreviousPassword + in.Request.Environment[in.PasswordKey])
	}
	request := in.Request
	request.DatabaseMaintenance = &in
	if e.perform(context.Background(), request) == nil {
		t.Fatal("failed maintenance succeeded")
	}
	log, err := os.ReadFile(filepath.Join(e.Root, "operations", request.ID+".log"))
	if err != nil || !strings.Contains(string(log), "could not change PostgreSQL extension") {
		t.Fatal("maintenance failure has no actionable operation log", err)
	}
	if strings.Contains(string(log), in.PreviousPassword) || strings.Contains(string(log), in.Request.Environment[in.PasswordKey]) {
		t.Fatal("maintenance log exposed credentials")
	}
}
