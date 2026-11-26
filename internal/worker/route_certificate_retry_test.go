package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPublicCertificateRetryBudgetAndRecovery(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string][]Route{"app": {{Domain: "late.example.test", Service: "web", Port: 80}, {Domain: "private.example.test", Service: "web", Port: 80, Policy: &RoutePolicy{Visibility: "lan", CertificateSource: "default"}}, {Domain: "stored.example.test", Service: "web", Port: 80, Policy: &RoutePolicy{CertificateSource: "uploaded", CertificateID: "stored"}}, {Protocol: "tcp", Port: 80}}}
	if err = e.writeManagedIngress(routes); err != nil {
		t.Fatal(err)
	}
	config := newIngressConfig()
	config.HTTP.Routers["cove-route-app"] = ingressRouter{Rule: "Host(`late.example.test`)", Service: "app"}
	config.TCP = &ingressTransportConfig{Routers: map[string]ingressTransportRouter{"raw-tcp": {EntryPoints: []string{"tcp"}, Service: "raw-tcp"}}, Services: map[string]ingressTransportService{"raw-tcp": {LoadBalancer: &ingressTransportBalancer{Servers: []map[string]string{{"address": "app:5432"}}}}}}
	config.UDP = &ingressTransportConfig{Routers: map[string]ingressTransportRouter{"raw-udp": {EntryPoints: []string{"udp"}, Service: "raw-udp"}}, Services: map[string]ingressTransportService{"raw-udp": {LoadBalancer: &ingressTransportBalancer{Servers: []map[string]string{{"address": "app:5353"}}}}}}
	config.TLS = &ingressTLS{Certificates: []map[string]string{{"certFile": "/data/selected.pem", "keyFile": "/data/selected.key"}}}
	if err = writeJSON(filepath.Join(e.Root, "ingress.traefik.yaml"), config); err != nil {
		t.Fatal(err)
	}
	certificate, _ := selfSignedCertificate(t, "late.example.test")
	issued := false
	delivered := 0
	var revisions []string
	run := func(ctx context.Context, dir string, args, environment []string, out io.Writer) error {
		switch {
		case len(args) > 1 && args[1] == "inspect":
			io.WriteString(out, `{"labels":{"cove.managed":"ingress"},"running":true,"image":"traefik:v3.7.19"}`)
		case len(args) == 4 && args[1] == "cp" && args[2] == "cove-ingress:/data/traefik-acme.json":
			store := map[string]any{"cove": map[string]any{"Certificates": []any{}}}
			if issued {
				store["cove"] = map[string]any{"Certificates": []any{map[string]any{"certificate": certificate}}}
			}
			return writeJSON(args[3], store)
		case len(args) == 4 && args[1] == "cp" && args[3] == "cove-ingress:/data/routes.next":
			var changed dynamicIngress
			data, _ := os.ReadFile(args[2])
			if err := json.Unmarshal(data, &changed); err != nil {
				return err
			}
			if !reflect.DeepEqual(changed.HTTP.Routers, config.HTTP.Routers) || len(changed.HTTP.Middlewares) != 1 {
				t.Fatal("retry changed routing", string(data))
			}
			if !reflect.DeepEqual(changed.TCP, config.TCP) || !reflect.DeepEqual(changed.UDP, config.UDP) || !reflect.DeepEqual(changed.TLS, config.TLS) {
				t.Fatal("retry changed raw routes or stored TLS configuration", string(data))
			}
			revision := changed.HTTP.Middlewares["cove-certificate-retry"].Headers["customRequestHeaders"].(map[string]any)["X-Cove-Unused-Retry"].(string)
			revisions = append(revisions, revision)
		case len(args) == 6 && args[1] == "exec":
			delivered++
			if delivered == 2 {
				issued = true
			} // Fake CA fails its first retried validation.
		default:
			t.Fatalf("unexpected command (must never start/stop/replace the proxy): %v", args)
		}
		return nil
	}
	e.Run = run
	now := time.Now().UTC()
	retry := func(at time.Time) {
		t.Helper()
		if err := e.retryPublicCertificates(context.Background(), at, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	retry(now)
	retry(now.Add(59 * time.Second))
	if delivered != 0 {
		t.Fatal("retried before budget")
	}
	status, err := e.CertificateRetries("app")
	if err != nil || len(status) != 1 || status[0].Hostname != "late.example.test" || status[0].Attempts != 0 {
		t.Fatal(status, err)
	}
	retry(now.Add(time.Minute))
	if delivered != 1 {
		t.Fatal("first retry missing")
	}
	// A worker restart must keep the spent budget rather than immediately retry.
	e, err = NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	e.Run = run
	retry(now.Add(2 * time.Minute))
	if delivered != 1 {
		t.Fatal("restart reset retry budget")
	}
	retry(now.Add(61 * time.Minute))
	checkpoint, err := e.certificateRetryState()
	if err != nil || !checkpoint.Next.Equal(now.Add(421*time.Minute)) {
		t.Fatal("repeated failure did not slow to six hours", checkpoint, err)
	}
	if delivered != 2 || revisions[0] == revisions[1] {
		t.Fatal("retry did not change provider snapshot", delivered, revisions)
	}
	retry(now.Add(62 * time.Minute))
	status, err = e.CertificateRetries("app")
	if err != nil || len(status) != 0 {
		t.Fatal("issued certificate still pending", status, err)
	}
	if _, err = os.Stat(filepath.Join(e.Root, "certificate-retry.json")); !os.IsNotExist(err) {
		t.Fatal("retry checkpoint not retired", err)
	}
}

func TestPublicCertificateRetryFencesAndLongBackoff(t *testing.T) {
	for _, fence := range []string{"active", "move", "host", "proxy", "stopped", "foreign", "certificates", "udp"} {
		t.Run(fence, func(t *testing.T) {
			e, _ := NewEngine(t.TempDir())
			e.writeManagedIngress(map[string][]Route{"app": {{Domain: "never.example.test", Service: "web", Port: 80}}})
			e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
				if (fence == "stopped" || fence == "foreign") && len(args) > 1 && args[1] == "inspect" {
					label := "ingress"
					if fence == "foreign" {
						label = "foreign"
					}
					fmt.Fprintf(out, `{"labels":{"cove.managed":%q},"running":false,"image":"traefik:v3.7.19"}`, label)
					return nil
				}
				t.Fatal("fenced retry reached Docker", args)
				return errors.New("unexpected")
			}
			switch fence {
			case "active":
				e.active["app"] = true
			case "move":
				e.draining = true
			case "host":
				e.hostChanging = true
			case "proxy":
				e.proxyUnsettled = true
			case "certificates":
				if err := writeJSON(filepath.Join(e.Root, "certificate-acme-reload.json"), map[string]int{"version": 1}); err != nil {
					t.Fatal(err)
				}
			case "udp":
				if err := writeJSON(filepath.Join(e.Root, "ingress.route-transition.json"), newIngressConfig()); err != nil {
					t.Fatal(err)
				}
			}
			err := e.retryPublicCertificates(context.Background(), time.Now(), io.Discard)
			if fence == "foreign" {
				if err == nil || !strings.Contains(err.Error(), "not managed") {
					t.Fatal("foreign proxy was not refused", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	data := []byte(`{"cove":{"Certificates":[]}}`)
	missing, err := missingACMECertificates(data, []string{"never.example.test"})
	if err != nil || strings.Join(missing, ",") != "never.example.test" {
		t.Fatal(missing, err)
	}
}
