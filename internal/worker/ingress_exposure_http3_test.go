package worker

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func verifyIngressExposureHTTP3(t *testing.T, e *Engine, ctx context.Context, desired *IngressExposureSettings, state *ingressExposureState, root string) {
	t.Helper()
	binary := os.Getenv("COVE_TEST_HTTP3_CLIENT")
	if binary == "" {
		t.Fatal("the isolated listener runner must supply its test-only HTTP/3 client")
	}
	desired.Public = &IngressPublicBindings{HTTPS: []IngressBinding{{Address: "127.0.0.1", Port: 14443}, {Address: "::1", Port: 14443}}}
	change := func(enabled bool) {
		t.Helper()
		desired.HTTP3 = enabled
		plan, err := e.PlanIngressExposures(ctx, *desired)
		if err != nil {
			t.Fatal(err)
		}
		before, err := e.output(ctx, "docker", "inspect", "--format", "{{.Id}}|{{.State.StartedAt}}", "cove-ingress")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.ChangeIngressExposures(ctx, IngressExposureRequest{Settings: plan.Settings, Revision: strings.Repeat("0", 64), Digest: plan.Digest, AcceptInterruption: true}); err == nil {
			t.Fatal("stale listener review was accepted")
		}
		unchanged, err := e.output(ctx, "docker", "inspect", "--format", "{{.Id}}|{{.State.StartedAt}}", "cove-ingress")
		if err != nil || unchanged != before {
			t.Fatal("stale listener request changed the proxy", err)
		}
		request := IngressExposureRequest{Settings: plan.Settings, Revision: plan.Revision, Digest: plan.Digest, AcceptInterruption: true}
		status, err := e.ChangeIngressExposures(ctx, request)
		if err != nil || status.Change == nil || status.Change.Phase != "completed" {
			t.Fatal("reviewed HTTP/3 change did not complete", err)
		}
		before, err = e.output(ctx, "docker", "inspect", "--format", "{{.Id}}|{{.State.StartedAt}}", "cove-ingress")
		if err != nil {
			t.Fatal(err)
		}
		repeated, err := e.ChangeIngressExposures(ctx, request)
		if err != nil || repeated.Change == nil || repeated.Change.ID != status.Change.ID {
			t.Fatal("same listener request did not return its durable outcome", err)
		}
		unchanged, err = e.output(ctx, "docker", "inspect", "--format", "{{.Id}}|{{.State.StartedAt}}", "cove-ingress")
		if err != nil || unchanged != before {
			t.Fatal("repeated listener request restarted the proxy", err)
		}
		*state, err = e.loadIngressExposures()
		if err != nil {
			t.Fatal(err)
		}
	}
	probe := func(target string, succeed bool) {
		t.Helper()
		id, err := e.output(ctx, "docker", "create", "--network", "host", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--label", "cove.test-listener=http3-client", "--entrypoint", "/ingress-client", ingressImage, "https://listener.example.test/", target, "/root.pem", "418")
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", id}, nil, io.Discard); err != nil {
				t.Error(err)
			}
		}()
		for _, pair := range [][2]string{{binary, "/ingress-client"}, {root, "/root.pem"}} {
			if err := e.Run(ctx, e.Root, []string{"docker", "cp", pair[0], id + ":" + pair[1]}, nil, io.Discard); err != nil {
				t.Fatal(err)
			}
		}
		output, err := e.output(ctx, "docker", "start", "-a", id)
		if succeed && (err != nil || !strings.Contains(output, "HTTP/3.0 418")) {
			t.Fatal("verified HTTP/3 request through the published UDP port failed", err, output)
		}
		if !succeed && err == nil {
			t.Fatal("disabled HTTP/3 still accepted a QUIC request", output)
		}
	}
	change(true)
	probe("127.0.0.1:14443", true)
	probe("[::1]:14443", true)
	desired.Public.HTTPS = []IngressBinding{{Address: "::1", Port: 14443}}
	change(true)
	probe("[::1]:14443", true)
	probe("127.0.0.1:14443", false)
	change(false)
	probe("[::1]:14443", false)
}
