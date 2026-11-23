package worker

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestRuntimeUsesTypedDockerHealthAndExitCode(t *testing.T) {
	e, _ := NewEngine(t.TempDir())
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		switch args[1] {
		case "ps":
			_, err := io.WriteString(out, `{"ID":"start","State":"running","Status":"localized"}`+"\n"+`{"ID":"healthy","State":"running"}`+"\n"+`{"ID":"unhealthy","State":"running"}`+"\n"+`{"ID":"job","State":"exited"}`)
			return err
		case "inspect":
			_, err := io.WriteString(out, `{"id":"start-full","service":"app","state":{"status":"running","health":{"status":"starting"}}}`+"\n"+`{"id":"healthy-full","service":"app","state":{"status":"running","health":{"status":"healthy"}}}`+"\n"+`{"id":"unhealthy-full","service":"app","state":{"status":"running","health":{"status":"unhealthy"}}}`+"\n"+`{"id":"job-full","service":"migrate","state":{"status":"exited","exitCode":0}}`)
			return err
		default:
			t.Fatal(strings.Join(args, " "))
			return nil
		}
	}
	containers, err := e.HealthContainers(context.Background(), "app")
	if err != nil || len(containers) != 4 {
		t.Fatal(containers, err)
	}
	for i, want := range []string{"starting", "healthy", "unhealthy", ""} {
		if containers[i].Health != want {
			t.Fatal(containers)
		}
	}
	if containers[0].ExitCode != nil || containers[3].ExitCode == nil || *containers[3].ExitCode != 0 || containers[3].Service != "migrate" {
		t.Fatal(containers)
	}
}

func TestRuntimeRejectsIncompleteInspection(t *testing.T) {
	e, _ := NewEngine(t.TempDir())
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		_, err := io.WriteString(out, `{"id":"other","state":{"status":"running"}}`)
		return err
	}
	if err := e.inspectContainerStates(context.Background(), []Container{{ID: "expected"}}); err == nil {
		t.Fatal("missing container details accepted")
	}
}
