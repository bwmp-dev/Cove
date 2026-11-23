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
	manualName := name + "-manual"
	manualID := run("run", "-d", "--name", manualName, "--label", "com.docker.compose.project=cove-"+id, "alpine:3.21", "sh", "-c", "trap 'exit 0' TERM; while :; do sleep 0.1; done")
	t.Cleanup(func() {
		if output, err := exec.Command("docker", "rm", "-f", manualName).CombinedOutput(); err != nil {
			t.Error(err, string(output))
		}
	})
	run("stop", "-t", "2", manualName)
	result, err = e.ObserveWorkload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	manualFound := false
	for _, container := range result.Containers {
		if strings.HasPrefix(manualID, container.ID) {
			manualFound = container.ManualStop && !container.ExitUnknown
		}
	}
	if !manualFound || result.CrashEvents != 4 {
		t.Fatal("Docker manual stop counted as crash", result)
	}
	oomName := name + "-oom"
	oomID := run("run", "-d", "--name", oomName, "--memory", "16m", "--memory-swap", "16m", "--label", "com.docker.compose.project=cove-"+id, "alpine:3.21", "awk", `BEGIN { x = "0123456789"; for (;;) x = x x }`)
	t.Cleanup(func() {
		if output, err := exec.Command("docker", "rm", "-f", oomName).CombinedOutput(); err != nil {
			t.Error(err, string(output))
		}
	})
	for attempt := 0; attempt < 50; attempt++ {
		if run("inspect", "--format", "{{.State.OOMKilled}}", oomName) == "true" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	result, err = e.ObserveWorkload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	oomFound := false
	for _, container := range result.Containers {
		if strings.HasPrefix(oomID, container.ID) {
			oomFound = container.OOMKilled && !container.ManualStop
		}
	}
	if !oomFound || result.OOMEvents < 1 {
		t.Fatal("real OOM event unavailable", result)
	}
}

func TestManualExitEventsAndBoundedMemory(t *testing.T) {
	var events []observationEvent
	for _, action := range []string{"die", "start", "kill", "die", "stop", "start", "restart", "die", "start", "kill", "die", "stop"} {
		events = append(events, observationEvent{container: "app", action: action, timestamp: time.Now()})
	}
	classified, manual := classifyObservationEvents(events, map[string]int{"app": 3})
	crashes, restarts := 0, 0
	for _, event := range classified {
		if event.Action == "die" {
			crashes++
		}
		if event.AutomaticRestart {
			restarts++
		}
	}
	if crashes != 2 || restarts != 2 || !manual["app"] {
		t.Fatal(crashes, restarts, manual)
	}
	e := &Engine{Root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(e.Root, "workloads", "app"), 0700); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	sample := &WorkloadObservations{SampledAt: time.Now(), Containers: []ContainerObservation{{ID: "app", Exited: true}}}
	if err := e.rememberManualExits("app", sample, map[string]string{"app": stamp}, manual); err != nil {
		t.Fatal(err)
	}
	sample.Containers[0].ManualStop = false
	sample.SampledAt = sample.SampledAt.Add(11 * time.Minute)
	if err := e.rememberManualExits("app", sample, map[string]string{"app": stamp}, nil); err != nil || !sample.Containers[0].ManualStop {
		t.Fatal("old manual stop forgotten", sample, err)
	}
	stamp = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
	if err := e.rememberManualExits("app", sample, map[string]string{"app": stamp}, nil); err != nil || !sample.Containers[0].ExitUnknown {
		t.Fatal("old stamp classified a later exit", sample, err)
	}
	if err := e.rememberManualExits("app", sample, map[string]string{"app": stamp}, map[string]bool{"app": false}); err != nil || sample.Containers[0].ExitUnknown || sample.Containers[0].ManualStop {
		t.Fatal("unexpected exit not recorded", sample, err)
	}
	if err := e.rememberManualExits("app", sample, map[string]string{"app": stamp}, nil); err != nil || sample.Containers[0].ExitUnknown {
		t.Fatal("known crash forgotten", sample, err)
	}
	sample.Containers[0].Exited = false
	if err := e.rememberManualExits("app", sample, nil, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(e.Root, "workloads", "app", "observed-manual-exits.json"))
	if err != nil || len(data) > 16384 || strings.Contains(string(data), stamp) {
		t.Fatal("obsolete exit retained", string(data), err)
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
			if strings.Contains(joined, `"state":`) {
				fmt.Fprintln(out, `{"id":"aaaaaaaaaaaa","state":{"Status":"running","ExitCode":0},"service":"app"}`)
			} else {
				fmt.Fprint(out, `{"restart_count":3,"oom_killed":false,"exit_code":0,"exited":false}`)
			}
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
	if err = writeJSON(filepath.Join(root, "ingress.json"), map[string][]Route{"app": {{Domain: "app.example.test", Service: "app", Port: 80}}}); err != nil {
		t.Fatal(err)
	}
	result, err = e.ObserveWorkload(context.Background(), "app")
	if err != nil || len(result.Certificates) != 1 || result.Certificates[0].ExpiresAt != nil {
		t.Fatal("foreign proxy certificate ownership was not respected", result, err)
	}
}
