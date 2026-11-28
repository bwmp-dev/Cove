package worker

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
)

func jobFixture(workload string, args []string, out io.Writer) (bool, error) {
	var text string
	switch {
	case args[len(args)-1] == "json":
		text = `{"services":{"app":{"image":"busybox:1.37"}}}`
	case len(args) > 1 && args[1] == "ps":
		text = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	case len(args) > 3 && args[1] == "inspect" && args[3] == `{{index .Config.Labels "com.docker.compose.project"}}`:
		text = "cove-" + workload
	case len(args) > 1 && args[1] == "inspect":
		text = `{"running":false,"exit":0,"status":"exited"}`
	default:
		return false, nil
	}
	_, err := io.WriteString(out, text)
	return true, err
}

func TestJobExecutionOptionValidation(t *testing.T) {
	r := Request{ID: "job", Workload: "job", Action: "deploy", Kind: "job", JobService: "app", Compose: "services: {}"}
	for _, seconds := range []int{0, 5, 1800, 86400} {
		r.Job = &JobOptions{TimeoutSeconds: seconds}
		if err := ValidateRequest(r); err != nil {
			t.Fatal(seconds, err)
		}
	}
	for _, seconds := range []int{-1, 4, 86401} {
		r.Job = &JobOptions{TimeoutSeconds: seconds}
		if ValidateRequest(r) == nil {
			t.Fatal("invalid execution timeout accepted", seconds)
		}
	}
	r.Kind, r.Job = "service", &JobOptions{TimeoutSeconds: 30}
	if ValidateRequest(r) == nil {
		t.Fatal("job execution options accepted for a service")
	}
}

func TestInterruptedJobRetainsLockAndMaintenanceCountUntilContainersStop(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "job", Workload: "app", Kind: "job", Action: "deploy", JobService: "app", Compose: "services: {}"}
	if _, err := e.Submit(r); err != nil {
		t.Fatal(err)
	}
	j, _ := e.read(r.ID)
	j.Operation.State = "running"
	if err := writeJSON(e.path(r.ID), j); err != nil {
		t.Fatal(err)
	}
	if err := e.Recover(); err != nil {
		t.Fatal(err)
	}
	state, err := e.Maintenance(true)
	if err != nil || state.Active != 1 {
		t.Fatal("recovered job was reported idle", state, err)
	}
	e.Run = func(context.Context, string, []string, []string, io.Writer) error {
		return errors.New("Docker unavailable")
	}
	previous := routeRecoveryRetry
	routeRecoveryRetry = 0
	defer func() { routeRecoveryRetry = previous }()
	var jobs sync.WaitGroup
	e.dispatch(context.Background(), &jobs)
	jobs.Wait()
	if state, err := e.Maintenance(true); err != nil || state.Active != 1 || !e.busy(r.Workload) {
		t.Fatal("unconfirmed cleanup released the job lock", state, err)
	}
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		_, err := jobFixture(r.Workload, args, out)
		return err
	}
	e.dispatch(context.Background(), &jobs)
	jobs.Wait()
	operation, err := e.Status(r.ID)
	if err != nil || operation.State != "interrupted" || operation.Job == nil || !operation.Job.ContainersStopped || e.busy(r.Workload) {
		t.Fatal(operation, err)
	}
	if state, err := e.Maintenance(true); err != nil || state.Active != 0 {
		t.Fatal("settled job was not released", state, err)
	}
}
