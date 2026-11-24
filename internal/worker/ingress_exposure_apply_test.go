package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func verifyIngressExposureApply(t *testing.T, e *Engine, ctx context.Context, state *ingressExposureState) {
	t.Helper()
	client := e.ingressDockerClient()
	defer client.CloseIdleConnections()
	if err := e.recoverIngressExposure(ctx, client, state); err != nil {
		t.Fatal(err)
	}
	certificate, key := selfSignedCertificate(t, "listener.example.test")
	bundle := RouteCertificate{ID: strings.Repeat("a", 32), CertificatePEM: string(certificate), PrivateKeyPEM: string(key)}
	if err := e.saveRouteCertificate("listener-app", bundle); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(e.Root, "ingress.json"), map[string][]Route{"listener-app": {{Domain: "listener.example.test", Service: "web", Port: 8080, Policy: &RoutePolicy{CertificateSource: "uploaded", CertificateID: bundle.ID}}}}); err != nil {
		t.Fatal(err)
	}
	cachedCertificate, cachedKey := selfSignedCertificate(t, "cached.example.test")
	cache, err := json.Marshal(map[string]any{"cove": map[string]any{"Certificates": []any{map[string]any{"domain": map[string]string{"main": "cached.example.test"}, "certificate": base64.StdEncoding.EncodeToString(cachedCertificate), "key": base64.StdEncoding.EncodeToString(cachedKey), "Store": "default"}}}})
	if err != nil {
		t.Fatal(err)
	}
	dynamic := []byte(`{"http":{"routers":{"listener-test":{"rule":"Host(\u0060listener.example.test\u0060)","entryPoints":["websecure"],"service":"noop@internal","tls":{}}}},"tls":{"certificates":[{"certFile":"/data/listener-test.crt","keyFile":"/data/listener-test.key"}]}}`)
	for name, data := range map[string][]byte{"listener-test.crt": certificate, "listener-test.key": key, "traefik-acme.json": cache, "dynamic/cove-listener-test.yaml": dynamic} {
		path := filepath.Join(e.Root, strings.ReplaceAll(name, "/", "-"))
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if name == "dynamic/cove-listener-test.yaml" {
			// Docker archive extraction creates the directory for the never-started proxy.
			dir := filepath.Join(e.Root, "dynamic")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "cove-listener-test.yaml"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := e.Run(ctx, e.Root, []string{"docker", "cp", dir, "cove-ingress:/data/dynamic"}, nil, io.Discard); err != nil {
				t.Fatal(err)
			}
		} else if err := e.Run(ctx, e.Root, []string{"docker", "cp", path, "cove-ingress:/data/" + name}, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	holder, err := e.output(ctx, "docker", "run", "-d", "--name", "cove-listener-port-holder", "--label", "cove.test-listener=port-holder", "-p", "127.0.0.1:15434:9999", "--entrypoint", "sh", ingressImage, "-ec", "sleep 120")
	if err != nil {
		t.Fatal("could not occupy the isolated fixture port", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", holder}, nil, io.Discard); err != nil {
			t.Error(err)
		}
	})
	desired := IngressExposureSettings{Listeners: []IngressListener{{ID: "e2e-local", Protocol: "tcp", Visibility: "public", Bindings: []IngressBinding{{Address: "127.0.0.1", Port: 15432}}}, {ID: "datagrams", Protocol: "udp", Visibility: "public", Bindings: []IngressBinding{{Address: "127.0.0.1", Port: 15353}}}}}
	prepared, err := e.prepareIngressExposure(ctx, client, desired, *state)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Probes = append(prepared.Probes, routeCertificateProbe{Hostname: "cached.example.test", CertificatePEM: string(cachedCertificate), VerifiedAt: time.Now().UTC()})
	if err := e.applyIngressExposure(ctx, client, prepared, state); err != nil {
		t.Fatal("stopped listener replacement failed", err)
	}
	stopped, err := captureIngressProxy(ctx, client)
	if err != nil || stopped.ID == prepared.Snapshot.ID || stopped.Running || state.Change.Phase != "completed" {
		t.Fatal("stopped replacement intent was not preserved", err)
	}
	desired.Listeners[0].Bindings[0].Port = 15434
	blocked, err := e.prepareIngressExposure(ctx, client, desired, *state)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.applyIngressExposure(ctx, client, blocked, state); err == nil {
		t.Fatal("stopped proxy accepted an occupied host binding")
	}
	kept, err := captureIngressProxy(ctx, client)
	if err != nil || kept.ID != stopped.ID || kept.Running || state.Change.Committed {
		t.Fatal("stopped bind failure replaced or started the original", err)
	}
	desired.Listeners[0].Bindings[0].Port = 15432
	if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+stopped.ID+"/start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.confirmIngressExposure(ctx, prepared, nil); err != nil {
		t.Fatal("replacement listeners did not start", err)
	}
	if _, err := e.inspectProxy(ctx); err != nil {
		t.Fatal("replacement is incompatible with custom ingress", err)
	}
	response, _ := e.output(ctx, "docker", "exec", "cove-ingress", "wget", "-S", "-O", "/dev/null", "--no-check-certificate", "--header=Host: listener.example.test", "https://127.0.0.1/")
	if !strings.Contains(response, "418") {
		t.Fatal("fixture router did not receive an HTTP request")
	}
	desired.Listeners[0].Bindings[0].Port = 15433
	next, err := e.prepareIngressExposure(ctx, client, desired, *state)
	if err != nil {
		t.Fatal("replacement cannot be planned again", err)
	}
	if err := e.applyIngressExposure(ctx, client, next, state); err != nil {
		t.Fatal("running listener replacement failed", err)
	}
	liveProxy, err := ingressProxyInspect(ctx, client, "cove-ingress")
	if err != nil || liveProxy == nil {
		t.Fatal(err)
	}
	if err := e.probeIngressExposureTLS(ctx, client, liveProxy.ID, next.Plan.ChangeID, prepared.Probes); err != nil {
		t.Fatal("stored and cached certificates changed after replacement", err)
	}
	var savedCache bytes.Buffer
	if err := copyIngressProxyFile(ctx, client, liveProxy.ID, "/data/traefik-acme.json", &savedCache, maxACMEStorageBytes); err != nil || !bytes.Equal(savedCache.Bytes(), cache) {
		t.Fatal("replacement changed the original ACME cache", err)
	}
	live, err := captureIngressProxy(ctx, client)
	if err != nil || live.ID == stopped.ID || !live.Running || state.Change.Phase != "completed" {
		t.Fatal("running replacement was not committed", err)
	}
	verifyIngressExposureHTTP3(t, e, ctx, &desired, state, filepath.Join(e.Root, "listener-test.crt"))
	live, err = captureIngressProxy(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{stopped.ID, prepared.Snapshot.ID} {
		container, err := ingressProxyInspect(ctx, client, old)
		if err != nil || container != nil {
			t.Fatal("committed original remains", err)
		}
	}
	if err := e.recoverIngressExposure(ctx, client, state); err != nil {
		t.Fatal("settled recovery failed", err)
	}
	desired.Listeners[0].Bindings[0].Port = 15434
	conflicting, err := e.prepareIngressExposure(ctx, client, desired, *state)
	if err != nil {
		t.Fatal("port-race plan failed before testing rollback", err)
	}
	if err := e.applyIngressExposure(ctx, client, conflicting, state); err == nil {
		t.Fatal("occupied publication unexpectedly succeeded")
	}
	restored, err := captureIngressProxy(ctx, client)
	if err != nil || restored.ID != live.ID || !restored.Running || state.Change.Phase != "interrupted" || state.Change.Committed {
		t.Fatal("publication failure did not automatically restore the original", err)
	}
	if err := e.probeIngressExposureTLS(ctx, client, restored.ID, conflicting.Plan.ChangeID, prepared.Probes); err != nil {
		t.Fatal("rollback lost the original certificate selection", err)
	}
	if err := ingressDockerJSON(ctx, client, "DELETE", "/containers/"+restored.ID+"?force=1", nil, nil); err != nil {
		t.Fatal(err)
	}
	reinitialized, err := e.InitializeIngressExposures(ctx)
	if err != nil || !reinitialized.ProxyPresent || reinitialized.Running {
		t.Fatal("missing proxy was not recreated with stopped intent", err)
	}
	desired.Listeners[0].Bindings[0].Port = 15433
	reviewed, err := e.PlanIngressExposures(ctx, desired)
	if err != nil || reviewed.RequiresRestart {
		t.Fatal("initialization lost the saved listeners or publications", err)
	}
	created, err := captureIngressProxy(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+created.ID+"/start", nil, nil); err != nil {
		t.Fatal("recreated proxy cannot start", err)
	}
	if err := e.probeIngressExposureTLS(ctx, client, created.ID, reviewed.ChangeID, prepared.Probes); err != nil {
		t.Fatal("recreated proxy lost certificate selection", err)
	}

	if err := ingressDockerJSON(ctx, client, "DELETE", "/containers/"+created.ID+"?force=1", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.ensureIngress(ctx, io.Discard); err != nil {
		t.Fatal("ordinary ingress reconciliation did not restore saved launch", err)
	}
	reviewed, err = e.PlanIngressExposures(ctx, desired)
	if err != nil || reviewed.RequiresRestart {
		t.Fatal("ordinary ingress reconciliation lost saved listeners", err)
	}

}
