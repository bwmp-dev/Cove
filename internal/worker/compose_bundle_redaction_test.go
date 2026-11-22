package worker

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestComposeBundleEnvFileValuesAreRedactedDuringDeployAndAfterRestart(t *testing.T) {
	engine, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := Request{ID: "bundle-redaction", Workload: "app", Action: "deploy", Compose: "services:\n  app:\n    image: busybox\n    env_file: private.env\n", ComposeBundle: &ComposeBundle{Assets: map[string]string{"private.env": "TOKEN=bundle-file-private-value\n"}}}
	engine.Run = func(_ context.Context, _ string, args, _ []string, output io.Writer) error {
		joined := strings.Join(args, " ")
		if strings.HasSuffix(joined, "config --format json") {
			_, err := io.WriteString(output, `{"services":{"app":{"image":"busybox","environment":{"TOKEN":"bundle-file-private-value"}}}}`)
			return err
		}
		if handled, err := releaseFixture(args, output); handled {
			return err
		}
		_, err := io.WriteString(output, "TOKEN=bundle-file-private-value\n")
		return err
	}
	if _, err = engine.Submit(request); err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	if err = engine.dispatch(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	operation, err := engine.Status(request.ID)
	if err != nil || operation.State != "succeeded" {
		t.Fatal("fixture deployment failed", err, operation)
	}
	logs, err := engine.Logs(request.ID)
	if err != nil || strings.Contains(logs, "bundle-file-private-value") || !strings.Contains(logs, "[redacted]") {
		t.Fatal("private ancillary environment value exposed in operation logs", err)
	}
	// A new worker loads redaction values from the retained private deployment state.
	restarted, err := NewEngine(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Run = func(_ context.Context, _ string, _, _ []string, output io.Writer) error {
		_, err := io.WriteString(output, "TOKEN=bundle-file-private-value\n")
		return err
	}
	logs, err = restarted.ContainerLogs(context.Background(), "app")
	if err != nil || strings.Contains(logs, "bundle-file-private-value") || !strings.Contains(logs, "[redacted]") {
		t.Fatal("private ancillary environment value exposed after worker restart", err)
	}
}
