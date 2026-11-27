package control

import (
	"bytes"
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cove/internal/worker"
)

func TestDispatchContinuesAfterObservedWorkloadDeletion(t *testing.T) {
	for _, outcome := range []string{"success", "rpc_failure", "database_failure"} {
		t.Run(outcome, func(t *testing.T) {
			f := newAccountFixture(t)
			f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,mode) VALUES('periodic-node','Node','localhost',22,'',$1,'ready','local')`, []byte{})
			f.sql(`INSERT INTO projects(id,name,team_id) VALUES('periodic-project','Project','default')`)
			sealed, _ := seal(f.s.Key, []byte(`{}`))
			f.sql(`INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('periodic-resource','periodic-node','Resource',$1,'periodic-project','default')`, sealed)
			var diagnostic bytes.Buffer
			if outcome == "database_failure" {
				f.sql(`CREATE FUNCTION fail_periodic_observation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected-private-detail' USING ERRCODE='23503'; END $$; CREATE TRIGGER fail_periodic_observation BEFORE INSERT ON workload_security_observations FOR EACH ROW EXECUTE FUNCTION fail_periodic_observation()`)
				previous := log.Writer()
				log.SetOutput(&diagnostic)
				defer log.SetOutput(previous)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var observed atomic.Bool
			var probes atomic.Int32
			f.s.workerRPC = func(ctx context.Context, node string, request worker.RPCRequest) (worker.RPCResponse, error) {
				if request.Method == "containers" && request.ID == "periodic-resource" && observed.CompareAndSwap(false, true) {
					query := `UPDATE nodes SET last_checked_at=NULL WHERE id='periodic-node'`
					if outcome != "database_failure" {
						query = `DELETE FROM workloads WHERE id='periodic-resource'; ` + query
					}
					if _, err := f.s.DB.Exec(ctx, query); err != nil {
						return worker.RPCResponse{}, err
					}
					if outcome == "rpc_failure" {
						return worker.RPCResponse{}, errors.New("injected unavailable worker")
					}
					return worker.RPCResponse{}, nil
				}
				if request.Method == "probe" && observed.Load() {
					if probes.Add(1) >= 2 {
						cancel()
					} else {
						if _, err := f.s.DB.Exec(ctx, `UPDATE nodes SET port=port+1 WHERE id='periodic-node'`); err != nil {
							return worker.RPCResponse{}, err
						}
					}
				}
				return worker.RPCResponse{}, errors.New("injected unavailable worker")
			}
			done := make(chan error, 1)
			go func() { done <- f.s.Dispatch(ctx) }()
			select {
			case err := <-done:
				if err != nil || !observed.Load() || probes.Load() < 2 {
					t.Fatalf("dispatcher did not continue: deleted=%v probes=%d err=%v", observed.Load(), probes.Load(), err)
				}
			case <-time.After(15 * time.Second):
				cancel()
				<-done
				t.Fatal("dispatcher did not reach subsequent ticks")
			}
			if outcome == "database_failure" && (!strings.Contains(diagnostic.String(), "refreshNodes") || !strings.Contains(diagnostic.String(), "23503") || strings.Contains(diagnostic.String(), "injected-private-detail")) {
				t.Fatal("housekeeping failure was not safely reported", diagnostic.String())
			}
			var count int
			if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM workload_security_observations WHERE workload_id='periodic-resource'`).Scan(&count); err != nil || count != 0 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestPeriodicObservationMissingParents(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('surviving-node','Node','localhost',22,'',$1)`, []byte{})
	if err := f.s.observePorts(t.Context(), "already-deleted", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.s.recordHealth(t.Context(), "already-deleted-team", "resource", "gone", "unavailable", true); err != nil {
		t.Fatal(err)
	}
	if err := f.s.storeNodeMetric(t.Context(), "already-deleted-node", &worker.NodeMetrics{SampledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.storeWorkloadTelemetry(t.Context(), "already-deleted-node", "gone", &worker.Telemetry{SampledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.storeTraffic(t.Context(), "already-deleted-node", &worker.TrafficReport{SampledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.storeWorkloadTelemetry(t.Context(), "surviving-node", "deleted-workload", &worker.Telemetry{SampledAt: time.Now(), Logs: []worker.LogEntry{{Timestamp: time.Now(), Message: "late log"}}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.linkFailure(t.Context(), &linkNode{id: "already-deleted-node"}, errors.New("unavailable")); err != nil {
		t.Fatal(err)
	}
	if err := f.s.dispatchImageUpdate(t.Context(), "deleted-workload"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.processNodeProvision(t.Context(), "deleted-provision"); err != nil {
		t.Fatal(err)
	}
}

func TestPeriodicAutomaticResultDoesNotResurrectDeletedWorkload(t *testing.T) {
	f, id, _, _ := newAutomaticScanFixture(t)
	enableAutomaticFixture(t, f, id)
	if err := f.s.scheduleAutomaticScans(t.Context()); err != nil {
		t.Fatal(err)
	}
	if automaticLeaseCount(t, f) != 1 {
		t.Fatal("automatic lease not prepared")
	}
	previous := f.s.workerRPC
	deleted := false
	f.s.workerRPC = func(ctx context.Context, node string, request worker.RPCRequest) (worker.RPCResponse, error) {
		if request.Method != "submit_automatic_image_scan" {
			return previous(ctx, node, request)
		}
		f.sql(`DELETE FROM automatic_image_scans WHERE workload_id=$1`, id)
		f.sql(`DELETE FROM operations WHERE workload_id=$1`, id)
		f.sql(`DELETE FROM workloads WHERE id=$1`, id)
		deleted = true
		return worker.RPCResponse{Operation: &worker.Operation{ID: request.Request.ID, Workload: id, Action: "image_scan", State: "failed"}}, nil
	}
	if err := f.s.reconcileAutomaticScans(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !deleted || automaticLeaseCount(t, f) != 0 {
		t.Fatal("late result retained a deleted workload lease")
	}
	var count int
	if err := f.s.DB.QueryRow(t.Context(), `SELECT count(*) FROM operations WHERE workload_id=$1`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("late operation was resurrected", count, err)
	}
}

func TestPeriodicObservationWaitsForConcurrentDelete(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('race-node','Node','localhost',22,'',$1)`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('race-project','Project','default')`)
	sealed, _ := seal(f.s.Key, []byte(`{}`))
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('race-resource','race-node','Resource',$1,'race-project','default')`, sealed)
	tx, err := f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `DELETE FROM workloads WHERE id='race-resource'`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.s.observePorts(t.Context(), "race-resource", nil) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err = f.s.DB.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'WITH live AS (SELECT id FROM workloads%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("observation did not wait for the concurrent delete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("observation remained blocked after delete committed")
	}
}

func TestPeriodicErrorsDoNotLogDatabaseDetails(t *testing.T) {
	var buffer bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buffer)
	defer log.SetOutput(old)
	logPeriodicError(t.Context(), "port exposure", &pgconn.PgError{Code: "23503", Message: "sensitive-message", Detail: "sensitive-detail"})
	text := buffer.String()
	if !strings.Contains(text, "port exposure") || !strings.Contains(text, "23503") || strings.Contains(text, "sensitive") {
		t.Fatal("unsafe or missing periodic diagnostic", text)
	}
}

func TestPeriodicNotificationWaitsForDeletedConnection(t *testing.T) {
	for _, operation := range []bool{false, true} {
		t.Run(map[bool]string{false: "channel_event", true: "operation_event"}[operation], func(t *testing.T) {
			f := newAccountFixture(t)
			f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('notify-node','Node','localhost',22,'',$1)`, []byte{})
			f.sql(`INSERT INTO projects(id,name,team_id) VALUES('notify-project','Project','default')`)
			sealed, _ := seal(f.s.Key, []byte(`{}`))
			f.sql(`INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('notify-resource','notify-node','Resource',$1,'notify-project','default')`, sealed)
			f.sql(`INSERT INTO infrastructure_connections(id,team_id,kind,name,configuration) VALUES('notify-connection','default','notification','Channel',$1)`, sealed)
			f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES('notify-operation','notify-resource','deploy',$1)`, sealed)
			tx, err := f.s.DB.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err = tx.Exec(t.Context(), `DELETE FROM infrastructure_connections WHERE id='notify-connection'`); err != nil {
				t.Fatal(err)
			}
			query := `SELECT cove_queue_channel_event('image.scan_completed','Scan finished','default','notify-resource','test-event','')`
			if operation {
				query = `UPDATE operations SET state='succeeded' WHERE id='notify-operation'`
			}
			done := make(chan error, 1)
			go func() { _, err := f.s.DB.Exec(t.Context(), query); done <- err }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				var waiting bool
				if err = f.s.DB.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query=$1)`, query).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("notification did not wait for connection deletion")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("notification remained blocked after connection deletion")
			}
			var count int
			if err = f.s.DB.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM notification_deliveries)+(SELECT count(*) FROM health_deliveries)`).Scan(&count); err != nil || count != 0 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestDispatchStillStopsWhenOperationRecoveryCannotPersist(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('fatal-node','Node','localhost',22,'',$1)`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('fatal-project','Project','default')`)
	sealed, _ := seal(f.s.Key, []byte(`{}`))
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('fatal-resource','fatal-node','Resource',$1,'fatal-project','default')`, sealed)
	f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES('fatal-operation','fatal-resource','deploy',$1)`, []byte("invalid sealed recovery state"))
	f.sql(`CREATE FUNCTION refuse_operation_outcome() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected outcome persistence failure' USING ERRCODE='XX000'; END $$; CREATE TRIGGER refuse_operation_outcome BEFORE UPDATE ON operations FOR EACH ROW EXECUTE FUNCTION refuse_operation_outcome()`)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := f.s.Dispatch(ctx); err == nil || ctx.Err() != nil {
		t.Fatal("unsafe operation error did not stop dispatcher", err)
	}
}
