package worker

import (
	"context"
	"io"
	"sync"
	"testing"
)

func TestDrainPreservesQueueAndWaitsForActive(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		select {
		case <-started:
		default:
			close(started)
		}
		<-finish
		return nil
	}
	r := Request{ID: "first", Workload: "app", Action: "deploy", Compose: "services: {}", Kind: "job", JobService: "app"}
	if _, err = e.Submit(r); err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	if err = e.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	<-started
	if state, err := e.Maintenance(true); err != nil || state.Active != 1 {
		t.Fatalf("active operation lost: %+v", state)
	}
	r.ID = "second"
	if _, err = e.Submit(r); err != nil {
		t.Fatal(err)
	}
	close(finish)
	jobs.Wait()
	if err = e.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	if state, err := e.Maintenance(true); err != nil || state.Active != 0 || state.Queued != 1 {
		t.Fatalf("queue not drained safely: %+v", state)
	}
	op, err := e.Status("second")
	if err != nil || op.State != "queued" {
		t.Fatalf("queued operation changed: %+v %v", op, err)
	}
	e, err = NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		if handled, err := releaseFixture(args, out); handled {
			return err
		}
		return nil
	}
	if err = e.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	op, err = e.Status("second")
	if err != nil || op.State != "queued" {
		t.Fatal("restart lost maintenance state")
	}
	if _, err = e.Maintenance(false); err != nil {
		t.Fatal(err)
	}
	if err = e.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	op, err = e.Status("second")
	if err != nil || op.State != "succeeded" {
		t.Fatalf("resume failed: %+v %v", op, err)
	}
}
