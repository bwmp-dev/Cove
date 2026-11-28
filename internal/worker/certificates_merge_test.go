package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCertificatesMergeTrafficTracksPolicyAndWildcardRouters(t *testing.T) {
	for _, route := range []Route{
		{Domain: "app.example.test", Service: "web", Port: 8080},
		{Domain: "*.example.test", Service: "web", Port: 8080, Policy: &RoutePolicy{CertificateSource: "uploaded", CertificateID: strings.Repeat("a", 32), RequestHeaders: map[string]string{"X-Key": "private"}}},
	} {
		config := newIngressConfig()
		config.addRoute("app", route)
		for name := range config.HTTP.Routers {
			body, err := json.Marshal(map[string]any{"RouterName": name + "@file", "DownstreamStatus": 200, "DownstreamContentSize": 1, "Duration": 1000, "ClientHost": "192.0.2.1"})
			if err != nil {
				t.Fatal(err)
			}
			entries := parseTrafficLines([]byte(time.Now().UTC().Format(time.RFC3339Nano)+" "+string(body)), "proxy", trafficRoutes(map[string][]Route{"app": {route}}), TrafficSettings{})
			if len(entries) != 1 || entries[0].Domain != route.Domain || entries[0].Workload != "app" {
				t.Fatal("actual route identity lost traffic", entries)
			}
			encoded, _ := json.Marshal(entries)
			if strings.Contains(string(encoded), "private") {
				t.Fatal("route header disclosed in traffic")
			}
		}
	}
}

func TestCertificatesMergeExpiryUsesSelectedCertificateAndWildcardSAN(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	certificate, key := selfSignedCertificate(t, "*.example.test")
	material := RouteCertificate{ID: strings.Repeat("a", 32), CertificatePEM: string(certificate), PrivateKeyPEM: string(key)}
	if err = e.saveRouteCertificate("app", material); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(e.Root, "ingress.json"), map[string][]Route{"app": {
		{Domain: "*.example.test", Service: "web", Port: 80, Policy: &RoutePolicy{CertificateSource: "uploaded", CertificateID: material.ID}},
		{Domain: "app.example.test", Service: "web", Port: 80, Policy: &RoutePolicy{CertificateSource: "default"}},
		{Domain: "*.acme.test", Service: "web", Port: 80},
	}}); err != nil {
		t.Fatal(err)
	}
	acme, _ := selfSignedCertificate(t, "*.acme.test")
	defaultCert, _ := selfSignedCertificate(t, "app.example.test")
	data, _ := json.Marshal(map[string]any{"cove": map[string]any{"Certificates": []map[string]string{
		{"certificate": base64.StdEncoding.EncodeToString(acme)},
		{"certificate": base64.StdEncoding.EncodeToString(defaultCert)},
	}}})
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		if len(args) > 1 && args[1] == "ps" {
			return nil
		}
		if len(args) > 1 && args[1] == "inspect" {
			_, err := io.WriteString(out, "ingress")
			return err
		}
		if len(args) > 1 && args[1] == "exec" {
			_, err := out.Write(data)
			return err
		}
		return fmt.Errorf("unexpected observation command: %v", args)
	}
	observed, err := e.ObserveWorkload(t.Context(), "app")
	if err != nil || len(observed.Certificates) != 3 {
		t.Fatal("certificate observations unavailable", err)
	}
	if observed.Certificates[0].ExpiresAt == nil || observed.Certificates[1].ExpiresAt != nil || observed.Certificates[2].ExpiresAt == nil {
		t.Fatal("expiry did not follow selected source", observed.Certificates)
	}
	encoded, _ := json.Marshal(observed)
	if strings.Contains(string(encoded), "PRIVATE KEY") {
		t.Fatal("certificate key disclosed by observations")
	}
}

func TestCertificatesMergeHeaderValuesAreRedactedAcrossLogChunks(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "operation")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	logger := boundedLog{file: file, remaining: 1024, secrets: routeSecrets([]Route{{Policy: &RoutePolicy{RequestHeaders: map[string]string{"X-Key": "header-token"}, ResponseHeaders: map[string]string{"X-Secret": "response-token"}}}})}
	for _, chunk := range []string{"request=header-", "token response=response-", "token"} {
		if _, err = logger.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err = logger.flush(true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil || string(data) != "request=[redacted] response=[redacted]" {
		t.Fatal("header values disclosed in logs", string(data), err)
	}
}
