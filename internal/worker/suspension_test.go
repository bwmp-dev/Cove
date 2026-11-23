package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSuspensionPreservesStoppedInitAndRestartPolicies(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	states := []string{"running", "exited", "paused"}
	policies := []string{"unless-stopped", "on-failure", "always"}
	dir := filepath.Join(e.Root, "workloads", "app")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "compose.json")
	if err := writeJSON(path, map[string]any{"services": map[string]any{"app": map[string]any{"depends_on": map[string]any{"init": map[string]string{"condition": "service_completed_successfully"}}}, "init": map[string]any{}}}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "current.json"), deployedState{ComposePath: path}); err != nil {
		t.Fatal(err)
	}
	updates := [][]string{}
	unpaused := []string{}
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		switch args[1] {
		case "ps":
			for i, id := range ids {
				if err := json.NewEncoder(out).Encode(map[string]string{"ID": id, "Names": "app", "State": states[i]}); err != nil {
					return err
				}
			}
		case "inspect":
			if strings.Contains(args[3], `"service":`) {
				for i, id := range ids {
					service := "app"
					if i == 1 {
						service = "init"
					}
					if err := json.NewEncoder(out).Encode(map[string]any{"id": id, "service": service, "state": map[string]any{"Status": states[i], "ExitCode": 0}}); err != nil {
						return err
					}
				}
				return nil
			}
			i := slices.Index(ids, args[len(args)-1])
			if i < 0 {
				t.Fatal(args)
			}
			if strings.Contains(args[3], "RestartPolicy") {
				return json.NewEncoder(out).Encode(map[string]any{"id": ids[i], "project": "cove-app", "state": map[string]bool{"running": states[i] == "running" || states[i] == "paused", "paused": states[i] == "paused"}, "restart": map[string]any{"Name": policies[i], "MaximumRetryCount": 3}})
			}
			_, err := io.WriteString(out, "cove-app")
			return err
		case "update":
			updates = append(updates, append([]string(nil), args...))
		case "unpause":
			unpaused = append(unpaused, args[2:]...)
		case "stop":
			for i := range states {
				states[i] = "exited"
			}
		default:
			t.Fatal("suspension started a stopped container", args)
		}
		return nil
	}
	captured, err := e.captureSuspension(context.Background(), "app")
	if err != nil || len(captured) != 3 || !captured[0].WasRunning || captured[1].WasRunning || !captured[1].Completed || !captured[2].WasRunning || !captured[2].Paused {
		t.Fatal(captured, err)
	}
	if err := e.stopSuspensionContainers(context.Background(), "app", captured, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0][2] != "--restart=no" || len(updates[0]) != 6 || !slices.Equal(unpaused, []string{ids[2]}) {
		t.Fatal("suspension did not disable restart for every container", updates, unpaused)
	}
	if err := e.restoreSuspensionPolicies(context.Background(), "app", captured, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 4 || updates[2][2] != "--restart=on-failure:3" {
		t.Fatal("original restart policies were lost", updates)
	}
}

func TestSuspensionRoutesPreserveProtectionWithoutContainers(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		if len(args) == 4 && args[0] == "docker" && args[1] == "cp" && strings.HasPrefix(args[2], "cove-ingress:/data/") {
			fmt.Fprintln(out, "Could not find the file")
			return errors.New("no cached certificate")
		}
		t.Fatal("a paused route inspected live containers")
		return nil
	}
	dir := filepath.Join(e.Root, "workloads", "app")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state := workloadSuspension{Operation: "pause", Options: SuspensionOptions{Reason: "environment", Scope: "development"}, Phase: "paused", Created: time.Now().UTC()}
	if err = writeJSON(filepath.Join(dir, "suspension.json"), state); err != nil {
		t.Fatal(err)
	}
	route := Route{Domain: "app.example.test", Service: "app", Port: 8080, Auth: "reader:$2b$10$" + strings.Repeat("a", 53), Policy: &RoutePolicy{PathPrefix: "/private", IPAllowList: []string{"10.0.0.0/8"}, SecurityHeaders: true, CertificateSource: "default"}}
	generated, err := e.ingressTraffic(context.Background(), map[string][]Route{"app": {route}}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var config dynamicIngress
	if err = json.Unmarshal([]byte(generated), &config); err != nil {
		t.Fatal(err)
	}
	if len(config.HTTP.Routers) != 1 {
		t.Fatal(config)
	}
	for _, router := range config.HTTP.Routers {
		if !strings.Contains(router.Rule, "Host(`app.example.test`)") || !strings.Contains(router.Rule, "PathPrefix(`/private`)") || len(router.Middlewares) != 3 || router.TLS == nil {
			t.Fatal(router)
		}
		backend := config.HTTP.Services[router.Service]
		if backend.LoadBalancer == nil || len(backend.LoadBalancer.Servers) != 0 {
			t.Fatal(backend)
		}
	}
}

func TestSuspensionRecoveryKeepsLockAndMissingIntentFails(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := Request{ID: "resume", Workload: "app", Action: "resume", Suspension: &SuspensionOptions{Reason: "environment", Scope: "development", Start: true}}
	if _, err = e.Submit(r); err != nil {
		t.Fatal(err)
	}
	if _, err = e.beginSuspension(context.Background(), r, false); err == nil {
		t.Fatal("missing snapshot was reported resumed")
	}
	j, err := e.read(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	j.Operation.State = "running"
	j.SuspensionChange = &suspensionChange{Action: "resume", Next: workloadSuspension{Operation: r.ID, Options: *r.Suspension, Phase: "resuming", Created: time.Now().UTC(), Deadline: time.Now().Add(time.Minute)}}
	if err = writeJSON(e.path(r.ID), j); err != nil {
		t.Fatal(err)
	}
	if err = e.Recover(); err != nil {
		t.Fatal(err)
	}
	maintenance, err := e.Maintenance(true)
	if err != nil || maintenance.Active != 1 || !e.busy("app") {
		t.Fatal("recovery released the workload before confirming suspension", maintenance, err)
	}
}
