package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIngressExposurePlanDockerIsReadOnlyAndRevisionBound(t *testing.T) {
	if os.Getenv("COVE_TEST_PROXY_RECOVERY_DOCKER") != "1" {
		t.Skip("requires owned isolated Docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	labels, err := e.output(ctx, "docker", "info", "--format", "{{json .Labels}}")
	if err != nil || !strings.Contains(labels, "cove.test-proxy-recovery=cove-proxy-replay-") {
		t.Fatal("listener planning refuses a daemon without its isolation label")
	}
	for _, kind := range []string{"container", "network", "volume"} {
		if _, err := e.output(ctx, "docker", kind, "inspect", "cove-ingress"); err == nil {
			t.Fatal("isolated daemon already has ingress state")
		}
	}
	created := [][2]string{}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for i := len(created) - 1; i >= 0; i-- {
			item := created[i]
			args := []string{"docker", item[0], "rm", "cove-ingress"}
			if item[0] == "container" {
				args = []string{"docker", "rm", "-f", "cove-ingress"}
			}
			if err := e.Run(cleanup, e.Root, args, nil, io.Discard); err != nil {
				t.Error(err)
			}
		}
	})
	for _, kind := range []string{"network", "volume"} {
		if err := e.Run(ctx, e.Root, []string{"docker", kind, "create", "--label", "cove.managed=ingress", "cove-ingress"}, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
		created = append(created, [2]string{kind, "cove-ingress"})
	}
	flags, err := e.ingressArgs()
	if err != nil {
		t.Fatal(err)
	}
	command := []string{"docker", "create", "--name", "cove-ingress", "--label", "cove.managed=ingress", "--network", "cove-ingress", "--restart", "unless-stopped", "--security-opt", "no-new-privileges:true", "-e", "PLANNER_SECRET=planner-private-fixture", "-v", "cove-ingress:/data", "--entrypoint", "sh", ingressImage, "-ec", `mkdir -p /data/dynamic; exec traefik "$@"`, "traefik"}
	if err := e.Run(ctx, e.Root, append(command, flags...), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	created = append(created, [2]string{"container", "cove-ingress"})
	client := e.ingressDockerClient()
	defer client.CloseIdleConnections()
	before, err := captureIngressProxy(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	desired := exposureConfigFixture()
	first, err := e.prepareIngressExposure(ctx, client, desired, ingressExposureState{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.prepareIngressExposure(ctx, client, desired, ingressExposureState{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Plan.Digest != second.Plan.Digest || first.Plan.Revision != second.Plan.Revision || !identifier.MatchString(first.Plan.ChangeID) || !first.Plan.RequiresRestart {
		t.Fatal("listener plan is not stable or reviewable")
	}
	public, _ := json.Marshal(first.Plan)
	for _, secret := range []string{"PLANNER_SECRET", "planner-private-fixture", "HostConfig", "Entrypoint"} {
		if strings.Contains(string(public), secret) {
			t.Fatal("public plan leaked private launch configuration")
		}
	}
	if err := writeJSON(filepath.Join(e.Root, "ingress.json"), map[string][]Route{"app": {{Domain: "plan.example.test", Service: "web", Port: 80}}}); err != nil {
		t.Fatal(err)
	}
	changed, err := e.prepareIngressExposure(ctx, client, desired, ingressExposureState{})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Plan.Revision == first.Plan.Revision || len(changed.Plan.AffectedWorkloads) != 1 || changed.Plan.AffectedWorkloads[0] != "app" {
		t.Fatal("new route inventory did not invalidate the reviewed plan")
	}
	after, err := captureIngressProxy(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if before.ID != after.ID || after.Running || string(before.Config) != string(after.Config) || string(before.HostConfig) != string(after.HostConfig) {
		t.Fatal("planning mutated or started the original proxy")
	}
	state := ingressExposureState{Change: &ingressProxyChange{ID: first.Plan.ChangeID, Phase: "validating", Previous: &first.Snapshot, Desired: desired, DesiredPorts: first.Plan.Ports, StartedAt: time.Now().UTC()}}
	if err := e.validateIngressExposure(ctx, client, first, &state); err != nil {
		t.Fatal("isolated pre-stop validation failed", err)
	}
	if state.Change.Validator != "" || state.Change.Staging != "" {
		t.Fatal("private validation resources remained in the checkpoint")
	}
	staging, err := filepath.Glob(filepath.Join(e.Root, ".ingress-listener-check-*"))
	if err != nil || len(staging) != 0 {
		t.Fatal("private validation staging was not removed", err)
	}
	remaining, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "label=cove.managed=ingress-check")
	if err != nil || remaining != "" {
		t.Fatal("validation proxy was not removed", err)
	}
	after, err = captureIngressProxy(ctx, client)
	if err != nil || after.ID != before.ID || after.Running {
		t.Fatal("pre-stop validation changed the original proxy", err)
	}
	verifyIngressExposureApply(t, e, ctx, &state)
}

func TestIngressExposurePlanningRefusesUnsettledWorkBeforeContactingDocker(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, *Engine, *ingressExposureState){
		"maintenance": func(_ *testing.T, e *Engine, _ *ingressExposureState) { e.draining = true },
		"route recovery": func(_ *testing.T, e *Engine, _ *ingressExposureState) {
			e.recovering = map[string]routeRecovery{"app": {}}
		},
		"custom recovery": func(_ *testing.T, e *Engine, _ *ingressExposureState) { e.customUnsettled = true },
		"listener recovery": func(_ *testing.T, _ *Engine, state *ingressExposureState) {
			state.Change = &ingressProxyChange{Phase: "applying"}
		},
		"migration": func(t *testing.T, e *Engine, _ *ingressExposureState) {
			if err := writeJSON(filepath.Join(e.Root, "ingress-mode.json"), migrationIngressMode{Staged: true, TLSChallenge: true}); err != nil {
				t.Fatal(err)
			}
		},
		"certificate reload": func(t *testing.T, e *Engine, _ *ingressExposureState) {
			if err := writeJSON(filepath.Join(e.Root, "certificate-acme-reload.json"), map[string]int{"version": 1}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, err := NewEngine(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var state ingressExposureState
			setup(t, e, &state)
			if _, err := e.prepareIngressExposure(context.Background(), nil, IngressExposureSettings{}, state); err == nil {
				t.Fatal("unsettled listener plan was accepted")
			}
		})
	}
}

func TestIngressExposureEquivalentBindingsDoNotRequireAnotherReplacement(t *testing.T) {
	first := IngressExposureSettings{Public: &IngressPublicBindings{HTTPS: []IngressBinding{{Address: "::1", Port: 443}, {Address: "127.0.0.1", Port: 443}}}}
	second := IngressExposureSettings{Public: &IngressPublicBindings{HTTP: []IngressBinding{}, HTTPS: []IngressBinding{{Address: "127.0.0.1", Port: 443}, {Address: "0:0:0:0:0:0:0:1", Port: 443}}}, Listeners: []IngressListener{}}
	if canonicalIngressExposures(first) != canonicalIngressExposures(second) {
		t.Fatal("equivalent binding spelling/order would restart the proxy")
	}
	if first.Public.HTTPS[0].Address != "::1" || second.Public.HTTPS[1].Address != "0:0:0:0:0:0:0:1" {
		t.Fatal("comparison changed the caller's settings")
	}
}
