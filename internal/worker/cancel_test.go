package worker

import (
	"context"
	"io"
	"sync"
	"testing"
)

func TestCancellationBeforeSubmitSurvivesReplay(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "cancel-before-submit", Workload: "app", Action: "stop"}
	op, err := e.Cancel(r)
	if err != nil || op.State != "cancelled" {
		t.Fatalf("%+v %v", op, err)
	}
	e, err = NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	op, err = e.Submit(r)
	if err != nil || op.State != "cancelled" {
		t.Fatalf("replay: %+v %v", op, err)
	}
}

func TestCancelRunningOperation(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	e.Run = func(ctx context.Context, _ string, _, _ []string, _ io.Writer) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	r := Request{ID: "running", Workload: "app", Action: "deploy", Compose: "services: {}"}
	if _, err = e.Submit(r); err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	if err = e.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err = e.Cancel(r); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	op, err := e.Status(r.ID)
	if err != nil || op.State != "cancelled" {
		t.Fatalf("%+v %v", op, err)
	}
	op, err = e.Cancel(r)
	if err != nil || op.State != "cancelled" {
		t.Fatalf("repeat: %+v %v", op, err)
	}
}
