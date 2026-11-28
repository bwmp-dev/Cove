package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestIngressProxyRecoveryDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_PROXY_RECOVERY_DOCKER") != "1" {
		t.Skip("requires an owned isolated Docker daemon")
	}
	guard := &Engine{Root: t.TempDir(), Run: RunCommand}
	labels, err := guard.output(context.Background(), "docker", "info", "--format", "{{json .Labels}}")
	if err != nil || !strings.Contains(labels, "cove.test-proxy-recovery=cove-proxy-replay-") {
		t.Fatal("proxy recovery acceptance refuses a daemon without its owned isolation label")
	}
	for _, test := range []struct {
		name      string
		stage     int
		stopped   bool
		committed bool
	}{
		{"before mutation", 0, false, false},
		{"restart disabled", 1, false, false},
		{"original stopped", 2, false, false},
		{"original renamed", 3, false, false},
		{"peers detached", 4, false, false},
		{"candidate response lost", 5, false, false},
		{"candidate running", 6, false, false},
		{"intentional stop", 5, true, false},
		{"committed cleanup", 6, false, true},
		{"committed intentional stop", 5, true, true},
		{"foreign canonical", 5, false, false},
		{"missing original", 6, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			run := func(args ...string) {
				t.Helper()
				if err := e.Run(ctx, e.Root, args, nil, io.Discard); err != nil {
					t.Fatal(err)
				}
			}
			for _, kind := range []string{"container", "network", "volume"} {
				if _, err := e.output(ctx, "docker", kind, "inspect", "cove-ingress"); err == nil {
					t.Fatal("isolated replay daemon already contains an ingress resource")
				}
			}
			ids := []string{}
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
				defer stop()
				for _, id := range ids {
					exists, err := e.output(cleanup, "docker", "ps", "-aq", "--filter", "id="+id)
					if err != nil {
						t.Error(err)
						continue
					}
					if exists != "" {
						if err := e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", id}, nil, io.Discard); err != nil {
							t.Error(err)
						}
					}
				}
				for _, args := range [][]string{{"docker", "network", "rm", "cove-ingress"}, {"docker", "network", "rm", "cove-recovery-helper"}, {"docker", "volume", "rm", "cove-ingress"}} {
					if err := e.Run(cleanup, e.Root, args, nil, io.Discard); err != nil {
						t.Error(err)
					}
				}
			})
			if err := writeJSON(filepath.Join(e.Root, "ingress-mode.json"), migrationIngressMode{Staged: true, TLSChallenge: true}); err != nil {
				t.Fatal(err)
			}
			if err := e.ensureIngress(ctx, io.Discard); err != nil {
				t.Fatal(err)
			}
			client := e.ingressDockerClient()
			defer client.CloseIdleConnections()
			var raw json.RawMessage
			if err := ingressDockerJSON(ctx, client, "GET", "/containers/cove-ingress/json", nil, &raw); err != nil {
				t.Fatal(err)
			}
			var identity struct {
				ID string `json:"Id"`
			}
			if json.Unmarshal(raw, &identity) != nil || len(identity.ID) != 64 {
				t.Fatal("original identity missing")
			}
			ids = append(ids, identity.ID)
			run("docker", "network", "create", "--internal", "cove-recovery-helper")
			run("docker", "network", "connect", "--alias", "trusted-proxy", "cove-recovery-helper", identity.ID)
			if !test.stopped {
				run("docker", "start", identity.ID)
			}
			if err := ingressDockerJSON(ctx, client, "GET", "/containers/cove-ingress/json", nil, &raw); err != nil {
				t.Fatal(err)
			}
			snapshot, err := captureIngressProxy(ctx, client)
			if err != nil {
				t.Fatal(err)
			}
			before, err := e.output(ctx, "docker", "inspect", "--format", "{{.State.StartedAt}}", identity.ID)
			if err != nil {
				t.Fatal(err)
			}
			change := &ingressProxyChange{ID: strings.Repeat("e", 32), Phase: "applying", Previous: &snapshot, StartedAt: time.Now().UTC()}
			state := ingressExposureState{Change: change}
			if err := e.saveIngressExposures(state); err != nil {
				t.Fatal(err)
			}
			if test.stage >= 1 {
				run("docker", "update", "--restart=no", identity.ID)
			}
			if test.stage >= 2 && !test.stopped {
				run("docker", "stop", "--timeout", "1", identity.ID)
			}
			if test.stage >= 3 {
				run("docker", "rename", identity.ID, "cove-ingress-backup-"+change.ID)
			}
			if test.stage >= 4 {
				for name := range snapshot.Networks {
					run("docker", "network", "disconnect", name, identity.ID)
				}
			}
			if test.stage >= 5 {
				body, err := ingressProxyCreateBody(snapshot, change.ID)
				if err != nil {
					t.Fatal(err)
				}
				if test.name == "foreign canonical" {
					var labels map[string]string
					json.Unmarshal(body["Labels"], &labels)
					labels["cove.managed"] = "foreign"
					body["Labels"], _ = json.Marshal(labels)
				}
				var created struct {
					ID string `json:"Id"`
				}
				if err := ingressDockerJSON(ctx, client, "POST", "/containers/create?name=cove-ingress", body, &created); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, created.ID)
				if test.stage >= 6 {
					run("docker", "start", created.ID)
				}
			}
			restarted, err := NewEngine(e.Root)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := restarted.loadIngressExposures()
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "foreign canonical" || test.name == "missing original" {
				current, err := ingressProxyInspect(ctx, client, "cove-ingress")
				if err != nil || current == nil {
					t.Fatal("test replacement missing", err)
				}
				if test.name == "missing original" {
					checkpoint.Change.Previous.ID = strings.Repeat("f", 64)
				}
				if err := restoreIngressProxy(ctx, client, checkpoint.Change); err == nil {
					t.Fatal("unsafe restoration was accepted")
				}
				after, err := ingressProxyInspect(ctx, client, current.ID)
				if err != nil || after == nil || after.Name != "/cove-ingress" || after.State.Running != current.State.Running {
					t.Fatal("held recovery changed the existing replacement", err)
				}
				original, err := ingressProxyInspect(ctx, client, identity.ID)
				if err != nil || original == nil || original.Name != "/cove-ingress-backup-"+change.ID || original.State.Running {
					t.Fatal("held recovery changed the retained original", err)
				}
				return
			}
			if test.committed {
				checkpoint.Change.Committed = true
				if err := restarted.saveIngressExposures(checkpoint); err != nil {
					t.Fatal(err)
				}
				checkpoint, err = restarted.loadIngressExposures()
				if err != nil {
					t.Fatal(err)
				}
				if err := restoreIngressProxy(ctx, client, checkpoint.Change); err == nil {
					t.Fatal("committed replacement was rolled back")
				}
				current, err := ingressProxyInspect(ctx, client, "cove-ingress")
				if err != nil || current == nil {
					t.Fatal("candidate missing before committed cleanup", err)
				}
				started, err := e.output(ctx, "docker", "inspect", "--format", "{{.State.StartedAt}}", current.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := restarted.Recover(); err != nil {
					t.Fatal(err)
				}
				restarted.runProxyRecovery(ctx)
				if restarted.proxyUnsettled {
					t.Fatal("worker startup left committed recovery unsettled")
				}
				settled, err := restarted.loadIngressExposures()
				if err != nil || settled.Change.Phase != "completed" {
					t.Fatal("worker startup did not persist committed cleanup", err)
				}
				if err := finishIngressProxy(ctx, client, checkpoint.Change); err != nil {
					t.Fatal("repeated committed cleanup failed", err)
				}
				original, err := ingressProxyInspect(ctx, client, identity.ID)
				if err != nil || original != nil {
					t.Fatal("retained original survived committed cleanup", err)
				}
				after, err := e.output(ctx, "docker", "inspect", "--format", "{{.State.StartedAt}}", current.ID)
				if err != nil || started != after {
					t.Fatal("committed cleanup restarted its replacement", err)
				}
				return
			}
			if err := restarted.Recover(); err != nil {
				t.Fatal(err)
			}
			restarted.runProxyRecovery(ctx)
			if restarted.proxyUnsettled {
				t.Fatal("worker startup left rollback unsettled")
			}
			settled, err := restarted.loadIngressExposures()
			if err != nil || settled.Change.Phase != "interrupted" {
				t.Fatal("worker startup did not persist rollback", err)
			}
			after, err := e.output(ctx, "docker", "inspect", "--format", "{{.State.StartedAt}}", identity.ID)
			if err != nil || test.stage <= 1 && before != after {
				t.Fatal("recovery restarted an unchanged running proxy", err)
			}
			if err := restoreIngressProxy(ctx, client, checkpoint.Change); err != nil {
				t.Fatal("repeated recovery failed", err)
			}
			again, err := e.output(ctx, "docker", "inspect", "--format", "{{.State.StartedAt}}", identity.ID)
			if err != nil || again != after {
				t.Fatal("repeated recovery restarted the restored proxy", err)
			}
			var restored struct {
				HostConfig struct{ RestartPolicy struct{ Name string } }
				State      struct{ Running bool }
			}
			if err := ingressDockerJSON(ctx, client, "GET", "/containers/"+identity.ID+"/json", nil, &restored); err != nil {
				t.Fatal(err)
			}
			if restored.HostConfig.RestartPolicy.Name != "unless-stopped" || restored.State.Running == test.stopped {
				t.Fatal("original running/restart intent changed")
			}
			remaining, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "label=cove.managed=ingress")
			if err != nil || !slices.Equal(strings.Fields(remaining), []string{identity.ID[:12]}) {
				t.Fatal("replacement remained after rollback", remaining, err)
			}
		})
	}
}
