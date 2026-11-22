package worker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHealthIgnoresOnlySuccessfulInitServices(t *testing.T) {
	e, _ := NewEngine(t.TempDir())
	dir := filepath.Join(e.Root, "workloads", "app")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "release.compose.json")
	if err := os.WriteFile(path, []byte(`{"services":{"web":{"depends_on":{"migrate":{"condition":"service_completed_successfully"}}},"migrate":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "current.json"), deployedState{ComposePath: path}); err != nil {
		t.Fatal(err)
	}
	exit := "0"
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		if args[1] == "ps" {
			_, err := io.WriteString(out, `{"ID":"init","State":"exited"}`+"\n"+`{"ID":"web","State":"running"}`)
			return err
		}
		if args[1] == "inspect" {
			_, err := io.WriteString(out, "migrate "+exit)
			return err
		}
		t.Fatal(strings.Join(args, " "))
		return nil
	}
	for _, code := range []string{"0", "1"} {
		exit = code
		containers, err := e.HealthContainers(context.Background(), "app")
		if err != nil || len(containers) != 2 || containers[0].ExpectedCompleted != (code == "0") {
			t.Fatal(containers, err)
		}
	}
}

func TestSharedHostInspectionDoesNotClaimVerifiedSettings(t *testing.T) {
	run := func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		_, err := io.WriteString(out, "container-id")
		return err
	}
	result := inspectSharedHost(context.Background(), run, t.TempDir())
	if result == nil || !result.CoolifyDetected || result.CleanupVerified {
		t.Fatal(result)
	}
}
