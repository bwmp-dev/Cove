package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRouteCertificatesDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_ROUTE_CERTIFICATES_DOCKER") != "1" || runtime.GOOS != "linux" {
		t.Skip("isolated Docker/Traefik certificate fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	command := func(args ...string) string {
		t.Helper()
		text, err := e.output(ctx, args...)
		if err != nil {
			t.Fatal(args, text, err)
		}
		return text
	}
	for _, kind := range []string{"container", "network", "volume"} {
		if _, err := e.output(ctx, "docker", kind, "inspect", "cove-ingress"); err == nil {
			t.Fatal("dedicated empty daemon required")
		}
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		for _, workload := range []string{"cert-app", "cert-victim"} {
			ids, _ := e.output(cleanup, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+workload)
			for _, id := range strings.Fields(ids) {
				e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "-v", id}, nil, io.Discard)
			}
		}
		e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "cove-ingress"}, nil, io.Discard)
	})
	if err := e.ensureIngress(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	oldCert, oldKey := selfSignedCertificate(t, "app.example.test")
	oldInfo, err := ParseRouteCertificate(RouteCertificate{ID: strings.Repeat("a", 32), CertificatePEM: string(oldCert), PrivateKeyPEM: string(oldKey)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	oldVictim, oldVictimKey := selfSignedCertificate(t, "victim.example.test")
	cached := map[string]any{"cove": map[string]any{"Certificates": []any{map[string]any{"domain": map[string]string{"main": "app.example.test"}, "certificate": base64.StdEncoding.EncodeToString(oldCert), "key": base64.StdEncoding.EncodeToString(oldKey), "Store": "default"}, map[string]any{"domain": map[string]string{"main": "victim.example.test"}, "certificate": base64.StdEncoding.EncodeToString(oldVictim), "key": base64.StdEncoding.EncodeToString(oldVictimKey), "Store": "default"}}}}
	cachePath := filepath.Join(e.Root, "seed-acme.json")
	if err := writeJSON(cachePath, cached); err != nil {
		t.Fatal(err)
	}
	command("docker", "stop", "cove-ingress")
	command("docker", "cp", cachePath, "cove-ingress:/data/traefik-acme.json")
	command("docker", "start", "cove-ingress")
	compose := `services:
  app:
    image: busybox:1.37
    pull_policy: never
    command: [sh, -ec, "mkdir -p /www; echo certificate-marker >/www/index.html; exec httpd -f -p 8080 -h /www"]
    healthcheck:
      test: [CMD, wget, -qO-, http://127.0.0.1:8080]
      interval: 1s
      timeout: 1s
      retries: 3
`
	run := func(r Request, expected string) {
		t.Helper()
		if _, err := e.Submit(r); err != nil {
			t.Fatal(err)
		}
		var jobs sync.WaitGroup
		if err := e.dispatch(ctx, &jobs); err != nil {
			t.Fatal(err)
		}
		jobs.Wait()
		op, err := e.Status(r.ID)
		logs, _ := e.Logs(r.ID)
		if err != nil || op.State != expected {
			t.Fatal(op, err, logs)
		}
	}
	route := Route{Domain: "app.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "acme"}}
	run(Request{ID: "cert-initial", Workload: "cert-app", Action: "deploy", Kind: "service", Compose: compose, Routes: []Route{route}}, "succeeded")
	if err := CheckRouteCertificateTLS(ctx, "127.0.0.1:443", route.Domain, string(oldCert)); err != nil {
		t.Fatal("cached ACME certificate not served before migration", oldInfo.Fingerprint, err)
	}

	oldVictimRoute := Route{Domain: "victim.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "acme"}}
	run(Request{ID: "victim-cached-initial", Workload: "cert-victim", Action: "deploy", Kind: "service", Compose: compose, Routes: []Route{oldVictimRoute}}, "succeeded")
	if err := CheckRouteCertificateTLS(ctx, "127.0.0.1:443", oldVictimRoute.Domain, string(oldVictim)); err != nil {
		t.Fatal("unrelated cached certificate was not loaded", err)
	}
	before := command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-cert-app")
	certificate, key := selfSignedCertificate(t, route.Domain)
	first := RouteCertificate{ID: strings.Repeat("b", 32), CertificatePEM: string(certificate), PrivateKeyPEM: string(key)}
	baseRunner := e.Run
	probeReported := false
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		if len(args) > 0 && args[len(args)-1] == "certificate-probe" {
			// The packaged worker owns private files as UID 1000; the probe must use that same unprivileged identity.
			args = append([]string{}, args...)
			for i, value := range args {
				if value == "--user" {
					args[i+1] = "1000:1000"
				}
			}
			if err := os.Chmod(e.Root, 0755); err != nil {
				return err
			}
			if err := os.Chown(filepath.Join(e.Root, "certificate-probes.json"), 1000, 1000); err != nil {
				return err
			}
			var diagnostic bytes.Buffer
			err := baseRunner(ctx, dir, args, env, &diagnostic)
			if err != nil && !probeReported {
				probeReported = true
				t.Log("TLS probe diagnostic:", diagnostic.String())
			}
			return err
		}
		if len(args) > 3 && args[0] == "docker" && args[1] == "cp" && strings.HasPrefix(args[2], e.Root) {
			if err := os.Chown(args[2], 1000, 1000); err != nil {
				return err
			}
		}
		return baseRunner(ctx, dir, args, env, out)
	}
	route.Policy.CertificateSource = "uploaded"
	route.Policy.CertificateID = first.ID
	run(Request{ID: "cert-upload", Workload: "cert-app", Action: "routes", Kind: "service", Routes: []Route{route}, Certificates: []RouteCertificate{first}}, "succeeded")
	if err := CheckRouteCertificateTLS(ctx, "127.0.0.1:443", route.Domain, first.CertificatePEM); err != nil {
		t.Fatal("cached ACME certificate overrode selected upload", err)
	}
	if command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-cert-app") != before {
		t.Fatal("certificate change replaced application")
	}

	if err := CheckRouteCertificateTLS(ctx, "127.0.0.1:443", oldVictimRoute.Domain, string(oldVictim)); err != nil {
		t.Fatal("upload changed another resource's cached ACME certificate", err)
	}
	victimCert, victimKey := selfSignedCertificate(t, "victim.example.test")
	victim := RouteCertificate{ID: strings.Repeat("c", 32), CertificatePEM: string(victimCert), PrivateKeyPEM: string(victimKey)}
	victimRoute := Route{Domain: "victim.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "uploaded", CertificateID: victim.ID}}
	run(Request{ID: "victim-upload", Workload: "cert-victim", Action: "routes", Kind: "service", Routes: []Route{victimRoute}, Certificates: []RouteCertificate{victim}}, "succeeded")
	if err := CheckRouteCertificateTLS(ctx, "127.0.0.1:443", victimRoute.Domain, victim.CertificatePEM); err != nil {
		t.Fatal(err)
	}
	replacement, replacementKey := selfSignedCertificate(t, route.Domain)
	next := RouteCertificate{ID: strings.Repeat("d", 32), CertificatePEM: string(replacement), PrivateKeyPEM: string(replacementKey)}
	original := e.Run
	injected := false
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		if !injected && len(args) > 3 && args[0] == "docker" && args[1] == "exec" && strings.Contains(strings.Join(args, " "), "mv /data/routes.next") {
			injected = true
			return errors.New("injected proxy handoff failure")
		}
		return original(ctx, dir, args, env, out)
	}
	route.Policy.CertificateID = next.ID
	run(Request{ID: "cert-rollback", Workload: "cert-app", Action: "routes", Kind: "service", Routes: []Route{route}, Certificates: []RouteCertificate{next}}, "failed")
	e.Run = original
	if !injected {
		t.Fatal("failure was not exercised")
	}
	if err := CheckRouteCertificateTLS(ctx, "127.0.0.1:443", route.Domain, first.CertificatePEM); err != nil {
		t.Fatal("previous certificate was not restored", err)
	}
	run(Request{ID: "cert-rotate", Workload: "cert-app", Action: "routes", Kind: "service", Routes: []Route{route}, Certificates: []RouteCertificate{next}}, "succeeded")
	if err := CheckRouteCertificateTLS(ctx, "127.0.0.1:443", route.Domain, next.CertificatePEM); err != nil {
		t.Fatal(err)
	}
	e, err = NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	command("docker", "restart", "cove-ingress")
	for deadline := time.Now().Add(15 * time.Second); ; {
		if err := CheckRouteCertificateTLS(ctx, "127.0.0.1:443", route.Domain, next.CertificatePEM); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("proxy restart lost certificate")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := CheckRouteCertificateTLS(ctx, "127.0.0.1:443", victimRoute.Domain, victim.CertificatePEM); err != nil {
		t.Fatal("other resource lost certificate", err)
	}
	leaf, _ := pemCertificateDigest(next.CertificatePEM)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: routeTestPool(next.CertificatePEM), ServerName: route.Domain, MinVersion: tls.VersionTLS12}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request, _ := http.NewRequestWithContext(ctx, "GET", "https://127.0.0.1/", nil)
	request.Host = route.Domain
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.TLS == nil || sha256.Sum256(response.TLS.PeerCertificates[0].Raw) != leaf {
		t.Fatal("HTTP TLS fingerprint changed")
	}
	route.Policy.CertificateID = ""
	route.Policy.CertificateSource = "acme"
	run(Request{ID: "cert-return-acme", Workload: "cert-app", Action: "routes", Kind: "service", Routes: []Route{route}}, "succeeded")
	if err := CheckRouteCertificateTLS(ctx, "127.0.0.1:443", route.Domain, string(oldCert)); err != nil {
		t.Fatal("switching back lost cached ACME certificate", err)
	}
	marker := filepath.Join(e.Root, "certificate-acme-reload.json")
	if err := writeJSON(marker, map[string]int{"version": 1}); err != nil {
		t.Fatal(err)
	}
	command("docker", "stop", "cove-ingress")
	e, err = NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	owned, err := e.loadIngress()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.syncIngress(ctx, owned, io.Discard); err != nil {
		t.Fatal("worker restart did not recover pending proxy reload", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("reload intent was not settled", err)
	}
	if err := CheckRouteCertificateTLS(ctx, "127.0.0.1:443", route.Domain, string(oldCert)); err != nil {
		t.Fatal(err)
	}
	if err := CheckRouteCertificateTLS(ctx, "127.0.0.1:443", victimRoute.Domain, victim.CertificatePEM); err != nil {
		t.Fatal("reload recovery changed unrelated TLS", err)
	}
	defaultCertificate, defaultKey := selfSignedCertificate(t, route.Domain)
	for name, value := range map[string][]byte{"fixture-default.crt": defaultCertificate, "fixture-default.key": defaultKey} {
		local := filepath.Join(e.Root, name)
		if err := os.WriteFile(local, value, 0600); err != nil {
			t.Fatal(err)
		}
		command("docker", "cp", local, "cove-ingress:/data/"+name)
	}
	defaultConfig := filepath.Join(e.Root, "fixture-default.yaml")
	if err := writeJSON(defaultConfig, map[string]any{"tls": map[string]any{"stores": map[string]any{"default": map[string]any{"defaultCertificate": map[string]string{"certFile": "/data/fixture-default.crt", "keyFile": "/data/fixture-default.key"}}}}}); err != nil {
		t.Fatal(err)
	}
	command("docker", "cp", defaultConfig, "cove-ingress:/data/dynamic/fixture-default.yaml")
	route.Policy.CertificateSource = "default"
	run(Request{ID: "cert-disable-acme", Workload: "cert-app", Action: "routes", Kind: "service", Routes: []Route{route}}, "succeeded")
	if err := CheckRouteCertificateTLS(ctx, "127.0.0.1:443", route.Domain, string(defaultCertificate)); err != nil {
		t.Fatal("disabling ACME did not remove the cached provider certificate", err)
	}
	cache, err := e.readACMEFile(ctx, "traefik-acme.json")
	if err != nil || bytes.Contains(cache, []byte(base64.StdEncoding.EncodeToString(oldCert))) {
		t.Fatal("disabled certificate remains eligible for cached ACME renewal", err)
	}
	defaultTransport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: routeTestPool(string(defaultCertificate)), ServerName: route.Domain, MinVersion: tls.VersionTLS12}, DisableKeepAlives: true}
	defer defaultTransport.CloseIdleConnections()
	defaultClient := &http.Client{Transport: defaultTransport, Timeout: 5 * time.Second}
	legacyPriority := filepath.Join(e.Root, "fixture-priority.yaml")
	if err := writeJSON(legacyPriority, map[string]any{"http": map[string]any{"routers": map[string]any{"fixture-priority": map[string]any{"rule": "HostRegexp(`^" + regexp.QuoteMeta(route.Domain) + "$`)", "priority": 1000, "entryPoints": []string{"websecure"}, "service": "noop@internal", "tls": map[string]any{}}}}}); err != nil {
		t.Fatal(err)
	}
	command("docker", "cp", legacyPriority, "cove-ingress:/data/dynamic/fixture-priority.yaml")
	waitStatus := func(expected int) {
		t.Helper()
		lastStatus := 0
		var lastError error
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
			request, _ := http.NewRequestWithContext(ctx, "GET", "https://127.0.0.1/index.html", nil)
			request.Host = route.Domain
			response, err := defaultClient.Do(request)
			lastError = err
			if err == nil {
				response.Body.Close()
				lastStatus = response.StatusCode
				if response.StatusCode == expected {
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("existing custom router ordering was not preserved: expected status %d, received %d: %v", expected, lastStatus, lastError)
	}
	// Existing positive-priority custom rules keep their precedence over an exact-host route.
	waitStatus(http.StatusTeapot)
	command("docker", "exec", "cove-ingress", "rm", "/data/dynamic/fixture-priority.yaml")
	waitStatus(http.StatusOK)
	wildcardPEM, wildcardKey := selfSignedCertificate(t, "*.wild.example.test")
	wildcardCertificate := RouteCertificate{ID: strings.Repeat("e", 32), CertificatePEM: string(wildcardPEM), PrivateKeyPEM: string(wildcardKey)}
	wildcardRoute := Route{Domain: "*.wild.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "uploaded", CertificateID: wildcardCertificate.ID, PathPrefix: "/index", ResponseHeaders: map[string]string{"X-Route": "wildcard"}}}
	exactRoute := Route{Domain: "a.wild.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "uploaded", CertificateID: wildcardCertificate.ID, ResponseHeaders: map[string]string{"X-Route": "exact"}}}
	run(Request{ID: "cert-wildcard", Workload: "cert-app", Action: "routes", Kind: "service", Routes: []Route{route, wildcardRoute, exactRoute}, Certificates: []RouteCertificate{wildcardCertificate}}, "succeeded")
	if err := CheckRouteCertificateTLS(ctx, "127.0.0.1:443", "b.wild.example.test", string(wildcardPEM)); err != nil {
		t.Fatal("wildcard SNI certificate was not served", err)
	}
	for _, check := range []struct {
		host, header string
		status       int
	}{{"a.wild.example.test", "exact", 200}, {"b.wild.example.test", "wildcard", 200}, {"B.WILD.EXAMPLE.TEST", "wildcard", 200}, {"wild.example.test", "", 404}, {"a.b.wild.example.test", "", 404}} {
		request, _ := http.NewRequestWithContext(ctx, "GET", "https://127.0.0.1/index.html", nil)
		request.Host = check.host
		response, err := defaultClient.Do(request)
		if err != nil {
			t.Fatal(check.host, err)
		}
		response.Body.Close()
		if response.StatusCode != check.status || response.Header.Get("X-Route") != check.header {
			t.Fatal("wildcard scope/priority", check.host, response.StatusCode, response.Header.Get("X-Route"))
		}
	}

	if bytes, err := json.Marshal(Operation{}); err != nil || strings.Contains(string(bytes), "private_key_pem") {
		t.Fatal("public operation contains private material")
	}
}

func pemCertificateDigest(value string) ([32]byte, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil {
		return [32]byte{}, errors.New("certificate unavailable")
	}
	return sha256.Sum256(block.Bytes), nil
}
func routeTestPool(value string) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(value))
	return pool
}
