package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagedCertificateObservations(t *testing.T) {
	certificate, key := selfSignedCertificate(t, "app.example.test")
	data, _ := json.Marshal(map[string]any{"cove": map[string]any{"Account": map[string]string{"PrivateKey": string(key)}, "Certificates": []map[string]string{{"certificate": base64.StdEncoding.EncodeToString(certificate), "key": string(key)}}}})
	routes := []CertificateObservation{{Domain: "app.example.test"}, {Domain: "foreign.example.test"}}
	applyCertificateExpiries(data, routes)
	if routes[0].ExpiresAt == nil || routes[1].ExpiresAt != nil {
		t.Fatal("certificate domains not matched", routes)
	}
	encoded, _ := json.Marshal(routes)
	if strings.Contains(string(encoded), "PRIVATE") || strings.Contains(string(encoded), "certificate\"") {
		t.Fatal("certificate material leaked")
	}
	applyCertificateExpiries([]byte(`{"cove":{"Certificates":[{"certificate":"invalid"}]}}`), routes)
	if routes[0].ExpiresAt == nil {
		t.Fatal("invalid certificate removed valid date")
	}
}

func TestWorkloadObservationsDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_OBSERVATIONS_DOCKER") != "1" {
		t.Skip("COVE_TEST_OBSERVATIONS_DOCKER=1 required; disposable restart-loop container")
	}
	id := fmt.Sprintf("observations-%d", time.Now().UnixNano())
	name := "cove-test-" + id
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	run := func(args ...string) string {
		t.Helper()
		data, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatal(err, string(data))
		}
		return strings.TrimSpace(string(data))
	}
	run("run", "-d", "--name", name, "--label", "com.docker.compose.project=cove-"+id, "--restart", "on-failure:3", "alpine:3.21", "sh", "-c", "exit 7")
	t.Cleanup(func() {
		if data, err := exec.Command("docker", "rm", "-f", name).CombinedOutput(); err != nil {
			t.Error(err, string(data))
		}
	})
	for attempt := 0; attempt < 50; attempt++ {
		if run("inspect", "--format", "{{.RestartCount}} {{.State.Status}}", name) == "3 exited" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "workloads", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(dir, "compose.json")
	if err := writeJSON(compose, map[string]any{"services": map[string]any{"app": map[string]any{}}}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "current.json"), deployedState{ComposePath: compose}); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Root: root, Run: RunCommand}
	result, err := e.ObserveWorkload(ctx, id)
	if err != nil || !result.EventsAvailable || result.RestartEvents != 3 || result.CrashEvents != 4 || len(result.Containers) != 1 || result.Containers[0].ExitCode != 7 {
		t.Fatal(result, err)
	}
}

func TestWorkloadObservationsOwnershipAndBoundaries(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "workloads", "app")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(dir, "compose.json")
	if err := writeJSON(compose, map[string]any{"services": map[string]any{"app": map[string]any{}}}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "current.json"), deployedState{ComposePath: compose}); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Root: root}
	e.Run = func(_ context.Context, _ string, args, env []string, out io.Writer) error {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "ps --all"):
			fmt.Fprintln(out, `{"ID":"aaaaaaaaaaaa","Names":"app","State":"running"}`)
		case args[1] == "inspect":
			fmt.Fprint(out, `{"restart_count":3,"oom_killed":false,"exit_code":0,"exited":false}`)
		case args[1] == "events":
			if !strings.Contains(joined, "--filter container=aaaaaaaaaaaa") || !strings.Contains(joined, "--until") {
				t.Fatal("unbounded or unscoped events", args)
			}
			fmt.Fprintln(out, `{"Action":"die","Actor":{"ID":"aaaaaaaaaaaaa"}}`)
			fmt.Fprintln(out, `{"Action":"start","Actor":{"ID":"aaaaaaaaaaaaa"}}`)
			fmt.Fprintln(out, `{"Action":"oom","Actor":{"ID":"fffffffffffff"}}`)
		default:
			t.Fatal("unexpected command", args)
		}
		return nil
	}
	result, err := e.ObserveWorkload(context.Background(), "app")
	if err != nil || !result.EventsAvailable || result.RestartEvents != 1 || result.OOMEvents != 0 || len(result.Containers) != 1 || result.Containers[0].RestartCount != 3 {
		t.Fatal(result, err)
	}
	if _, err = e.ObserveWorkload(context.Background(), "../other"); err == nil {
		t.Fatal("invalid workload accepted")
	}
}
