package worker

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIngressExposureComposePlanAndValidationDocker(t *testing.T) {
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
		t.Fatal("Compose proxy validation refuses an unowned daemon")
	}
	for _, kind := range []string{"container", "network", "volume"} {
		if _, err := e.output(ctx, "docker", kind, "inspect", "cove-ingress"); err == nil {
			t.Fatal("isolated daemon already has ingress state")
		}
	}
	resources := [][2]string{}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for i := len(resources) - 1; i >= 0; i-- {
			resource := resources[i]
			args := []string{"docker", resource[0], "rm", resource[1]}
			if resource[0] == "container" {
				args = []string{"docker", "rm", "-f", resource[1]}
			}
			if err := e.Run(cleanup, e.Root, args, nil, io.Discard); err != nil {
				t.Error(err)
			}
		}
	})
	run := func(args ...string) {
		t.Helper()
		var output bytes.Buffer
		if err := e.Run(ctx, e.Root, args, nil, &output); err != nil {
			t.Fatalf("%v: %v\n%s", args[:2], err, output.String())
		}
	}
	for _, resource := range [][2]string{{"network", "cove-ingress"}, {"volume", "cove-ingress"}, {"volume", "cove-static-fixture"}} {
		run("docker", resource[0], "create", "--label", "cove.managed=ingress", resource[1])
		resources = append(resources, resource)
	}
	run("docker", "create", "--name", "cove-ingress", "--label", "cove.managed=ingress", "--label", "cove.panel.domain=panel.example.test", "--network", "cove-ingress", "--restart", "unless-stopped", "--security-opt", "no-new-privileges:true", "-e", "COVE_DOMAIN=panel.example.test", "-e", "PLANNER_SECRET=compose-private-fixture", "-v", "cove-ingress:/data", "-v", "cove-static-fixture:/etc/traefik:ro", "--entrypoint", "sh", ingressImage, "-ec", "mkdir -p /data/dynamic\ncp /etc/traefik/panel.yaml /data/dynamic/panel.yaml\nexec traefik --configFile=/etc/traefik/traefik.yaml\n")
	resources = append(resources, [2]string{"container", "cove-ingress"})
	source := []byte("entryPoints:\n  web: {address: ':80'}\n  websecure: {address: ':443'}\n  traefik: {address: '127.0.0.1:8080'}\napi: {insecure: true, dashboard: false}\nping: {}\nproviders: {file: {directory: /data/dynamic, watch: true}}\ncertificatesResolvers: {cove: {acme: {storage: /data/traefik-acme.json, httpChallenge: {entryPoint: web}}}}\n")
	panel := []byte("http:\n  routers:\n    panel:\n      rule: Host(`{{env \"COVE_DOMAIN\"}}`)\n      entryPoints: [websecure]\n      service: panel\n      tls: {certResolver: cove}\n  services:\n    panel:\n      loadBalancer:\n        servers: [{url: 'http://127.0.0.1:9'}]\n")
	run("docker", "create", "--name", "cove-static-seed", "--label", "cove.managed=ingress", "-v", "cove-static-fixture:/etc/traefik", "--entrypoint", "sh", ingressImage, "-c", "true")
	resources = append(resources, [2]string{"container", "cove-static-seed"})
	for name, data := range map[string][]byte{"traefik.yaml": source, "panel.yaml": panel} {
		file := filepath.Join(e.Root, name)
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		run("docker", "cp", file, "cove-static-seed:/etc/traefik/"+name)
	}
	client := e.ingressDockerClient()
	defer client.CloseIdleConnections()
	snapshot, err := captureIngressProxy(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	var copied bytes.Buffer
	if err := copyIngressProxyFile(ctx, client, snapshot.ID, "/etc/traefik/traefik.yaml", &copied, 1<<20); err != nil || !bytes.Equal(copied.Bytes(), source) {
		t.Fatal("stopped Compose static file was not read exactly", err)
	}
	var bounded bytes.Buffer
	if err := copyIngressProxyFile(ctx, client, snapshot.ID, "/etc/traefik/traefik.yaml", &bounded, 16); err == nil || bounded.Len() != 0 {
		t.Fatal("oversized private file was copied")
	}
	if err := copyIngressProxyFile(ctx, client, snapshot.ID, "/etc/traefik", &bounded, 1<<20); err == nil {
		t.Fatal("a directory was accepted as private static input")
	}
	prepared, err := e.prepareIngressExposure(ctx, client, exposureConfigFixture(), ingressExposureState{})
	if err != nil {
		t.Fatal(err)
	}
	state := ingressExposureState{Change: &ingressProxyChange{ID: prepared.Plan.ChangeID, Phase: "validating", Previous: &prepared.Snapshot, Desired: prepared.Plan.Settings, DesiredPorts: prepared.Plan.Ports, StartedAt: time.Now().UTC()}}
	if err := e.validateIngressExposure(ctx, client, prepared, &state); err != nil {
		t.Fatal("Compose pre-stop validation failed", err)
	}
	after, err := captureIngressProxy(ctx, client)
	if err != nil || after.ID != snapshot.ID || after.Running {
		t.Fatal("validation started or replaced the stopped Compose proxy", err)
	}
	var original bytes.Buffer
	if err := copyIngressProxyFile(ctx, client, snapshot.ID, "/etc/traefik/traefik.yaml", &original, 1<<20); err != nil || !bytes.Equal(original.Bytes(), source) {
		t.Fatal("validation changed the mounted original static file", err)
	}
	if state.Change.Validator != "" || state.Change.Staging != "" {
		t.Fatal("Compose validation resources remained in the checkpoint")
	}
	verifyIngressExposureApply(t, e, ctx, &state)
}
