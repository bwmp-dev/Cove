package worker

import (
	"bytes"
	"context"
	"cove/internal/outbound"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This requires an exclusively owned daemon because it drives the real fixed-name proxy.
func TestPublicCertificateRetryLateDNSPebble(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("COVE_TEST_CERTIFICATE_RETRY_DOCKER") != "1" {
		t.Skip("owned Linux Docker daemon with cached Pebble/challtestsrv and Traefik required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) string {
		t.Helper()
		value, err := e.output(ctx, args...)
		if err != nil {
			t.Fatal(args, value, err)
		}
		return value
	}
	for _, kind := range []string{"container", "network", "volume"} {
		if _, err := e.output(ctx, "docker", kind, "inspect", "cove-ingress"); err == nil {
			t.Fatal("dedicated empty daemon required")
		}
	}
	owner := filepath.Base(e.Root)
	pebble := "cove-retry-pebble-" + owner
	challenge := "cove-retry-dns-" + owner
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		for _, name := range []string{pebble, challenge} {
			label, err := e.output(cleanup, "docker", "inspect", "--format", `{{index .Config.Labels "cove.certificate-retry"}}`, name)
			if err == nil && label == owner {
				if err = e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "-v", name}, nil, io.Discard); err != nil {
					t.Error(err)
				}
			}
		}
		ids, err := e.output(cleanup, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-retry-app")
		if err == nil {
			for _, id := range strings.Fields(ids) {
				if err = e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "-v", id}, nil, io.Discard); err != nil {
					t.Error(err)
				}
			}
		}
		for _, kind := range []string{"container", "volume", "network"} {
			format := `{{index .Labels "cove.managed"}}`
			if kind == "container" {
				format = `{{index .Config.Labels "cove.managed"}}`
			}
			label, err := e.output(cleanup, "docker", kind, "inspect", "--format", format, "cove-ingress")
			if err == nil && label == "ingress" {
				args := []string{"docker", kind, "rm"}
				if kind == "container" {
					args = append(args, "-f", "-v")
				}
				args = append(args, "cove-ingress")
				if err = e.Run(cleanup, e.Root, args, nil, io.Discard); err != nil {
					t.Error(err)
				}
			}
		}
	})
	certificate, key := selfSignedCertificate(t, "pebble")
	root := filepath.Join(e.Root, "pebble-root.pem")
	if err = os.WriteFile(root, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.Root, "pebble-key.pem"), key, 0600); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"pebble": map[string]any{"listenAddress": ":14000", "managementListenAddress": ":15000", "certificate": "/fixture/pebble-root.pem", "privateKey": "/fixture/pebble-key.pem", "httpPort": 80, "tlsPort": 443, "profiles": map[string]any{"default": map[string]any{"description": "late DNS acceptance", "validityPeriod": 3600}}}}
	if err = writeJSON(filepath.Join(e.Root, "pebble.json"), config); err != nil {
		t.Fatal(err)
	}
	e.ACMEDirectory = "https://pebble:14000/dir"
	e.ACMERoot = root
	if err = e.ensureIngress(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	challengeImage := os.Getenv("COVE_TEST_CERTIFICATE_RETRY_CHALLENGE_IMAGE")
	if challengeImage == "" {
		challengeImage = "ghcr.io/letsencrypt/pebble-challtestsrv@sha256:0152c8cb506e2ecba2900ed47d23932e27e3e6575a7d2c8ef93c7719ee3dea4c"
	}
	pebbleImage := os.Getenv("COVE_TEST_CERTIFICATE_RETRY_PEBBLE_IMAGE")
	if pebbleImage == "" {
		pebbleImage = "ghcr.io/letsencrypt/pebble@sha256:0c81f05db371f436c97fb894bd6d2ab8fc9c8e2ce40f8bef8a8e15941bcd75ef"
	}
	command("docker", "run", "-d", "--pull=never", "--name", challenge, "--label", "cove.certificate-retry="+owner, "--network", "cove-ingress", "--network-alias", "challenge", challengeImage, "-dnsserver=:53", "-defaultIPv4=", "-defaultIPv6=", "-http01=", "-https01=", "-tlsalpn01=", "-doh=")
	command("docker", "run", "-d", "--pull=never", "--name", pebble, "--label", "cove.certificate-retry="+owner, "--network", "cove-ingress", "--network-alias", "pebble", "--mount", "type=bind,source="+e.Root+",target=/fixture,readonly", "-e", "PEBBLE_VA_NOSLEEP=1", "-e", "PEBBLE_AUTHZREUSE=0", pebbleImage, "-config", "/fixture/pebble.json", "-dnsserver", "challenge:53")
	address := command("docker", "inspect", "--format", `{{with index .NetworkSettings.Networks "cove-ingress"}}{{.IPAddress}}{{end}}`, pebble)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certificate)
	management := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "pebble", MinVersion: tls.VersionTLS12}}, Timeout: 2 * time.Second}
	defer management.CloseIdleConnections()
	var issuanceRoot []byte
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		response, err := management.Get("https://" + address + ":15000/roots/0")
		if err == nil {
			issuanceRoot, err = io.ReadAll(io.LimitReader(response.Body, 128<<10))
			response.Body.Close()
			if err == nil && response.StatusCode == 200 {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(issuanceRoot) == 0 {
		t.Fatal("owned validating CA did not start")
	}
	compose := "services:\n  web:\n    image: busybox:1.37\n    pull_policy: never\n    command: [sh, -ec, 'mkdir -p /www; echo retry-marker >/www/index.html; exec httpd -f -p 8080 -h /www']\n    healthcheck:\n      test: [CMD, wget, -qO-, http://127.0.0.1:8080]\n      interval: 1s\n      timeout: 1s\n      retries: 5\n"
	route := Route{Domain: "late.example.test", Service: "web", Port: 8080}
	if _, err = e.Submit(Request{ID: "late-initial", Workload: "retry-app", Action: "deploy", Kind: "service", Compose: compose, Routes: []Route{route}}); err != nil {
		t.Fatal(err)
	}
	var jobs sync.WaitGroup
	if err = e.dispatch(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	jobs.Wait()
	op, err := e.Status("late-initial")
	if err != nil || op.State != "succeeded" {
		logs, _ := e.Logs("late-initial")
		t.Fatal(op, logs, err)
	}
	failed := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		logs := command("docker", "logs", "cove-ingress")
		if strings.Contains(logs, "Unable to obtain ACME certificate") || strings.Contains(logs, "Unable to obtain ACME certificate for domains") {
			failed = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !failed {
		t.Fatal("initial missing-DNS ACME failure was not observed", command("docker", "logs", pebble))
	}
	before := command("docker", "inspect", "--format", "{{.Id}} {{.State.StartedAt}} {{.RestartCount}}", "cove-ingress")
	if err = e.retryPublicCertificates(ctx, time.Now(), io.Discard); err != nil {
		t.Fatal(err)
	}
	status, err := e.CertificateRetries("retry-app")
	if err != nil || len(status) != 1 || status[0].Attempts != 0 {
		t.Fatal(status, err)
	}
	// DNS appears after the failed order, with no new route/deploy/configuration request.
	dnsIP := command("docker", "inspect", "--format", `{{with index .NetworkSettings.Networks "cove-ingress"}}{{.IPAddress}}{{end}}`, challenge)
	proxyIP := command("docker", "inspect", "--format", `{{with index .NetworkSettings.Networks "cove-ingress"}}{{.IPAddress}}{{end}}`, "cove-ingress")
	previousResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, net.JoinHostPort(dnsIP, "53"))
	}}
	t.Cleanup(func() { net.DefaultResolver = previousResolver })
	if err := VerifyManagedRegistry(ctx, &Registry{Server: route.Domain, Username: "fixture", Password: "fixture"}, "", &outbound.Config{}); !errors.Is(err, ErrRegistryDNSPending) {
		t.Fatal("missing registry DNS did not receive its distinct diagnostic", err)
	}
	body, _ := json.Marshal(map[string]any{"host": route.Domain, "addresses": []string{proxyIP}})
	response, err := http.Post("http://"+dnsIP+":8055/add-a", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	loopCtx, stop := context.WithCancel(ctx)
	var loops sync.WaitGroup
	loops.Go(func() { e.certificateRetryLoop(loopCtx) })
	defer func() { stop(); loops.Wait() }()
	var probes, failures atomic.Int64
	probeClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: 2 * time.Second} // Availability only; the final probe below verifies trust and SNI.
	defer probeClient.CloseIdleConnections()
	loops.Go(func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
			}
			r, err := http.NewRequestWithContext(loopCtx, "GET", "https://127.0.0.1/", nil)
			if err != nil {
				return
			}
			r.Host = route.Domain
			response, err := probeClient.Do(r)
			if loopCtx.Err() != nil {
				return
			}
			probes.Add(1)
			if err != nil {
				failures.Add(1)
				continue
			}
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 1024))
			response.Body.Close()
			if readErr != nil || response.StatusCode != 200 || !strings.Contains(string(data), "retry-marker") {
				failures.Add(1)
			}
		}
	})
	trusted := x509.NewCertPool()
	if !trusted.AppendCertsFromPEM(issuanceRoot) {
		t.Fatal("CA root unavailable")
	}
	verified := false
	for deadline := time.Now().Add(100 * time.Second); time.Now().Before(deadline); {
		connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", "127.0.0.1:443", &tls.Config{RootCAs: trusted, ServerName: route.Domain, MinVersion: tls.VersionTLS12})
		if err == nil {
			connection.Close()
			verified = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	stop()
	loops.Wait()
	if !verified {
		t.Fatal("certificate did not recover autonomously after DNS appeared", command("docker", "logs", "cove-ingress"), command("docker", "logs", pebble))
	}
	after := command("docker", "inspect", "--format", "{{.Id}} {{.State.StartedAt}} {{.RestartCount}}", "cove-ingress")
	if before != after || probes.Load() < 10 || failures.Load() != 0 {
		t.Fatal("proxy identity or serving changed during retry", before, after, probes.Load(), failures.Load())
	}
	t.Logf("validating Pebble issuance recovered after late DNS; continuous probes=%d failures=%d; proxy identity/start/restarts unchanged", probes.Load(), failures.Load())
}
