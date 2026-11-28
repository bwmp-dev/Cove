package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCloudflareOriginHelperProcess(t *testing.T) {
	if os.Getenv("COVE_TEST_TUNNEL_ORIGIN_CHILD") != "1" {
		t.Skip("isolated helper subprocess only")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := RunCloudflareOrigin(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCloudflareOriginClientProcess(t *testing.T) {
	if os.Getenv("COVE_TEST_TUNNEL_ORIGIN_CLIENT") != "1" {
		t.Skip("isolated helper client only")
	}
	data, err := os.ReadFile("/root.pem")
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		t.Fatal("invalid fixture root")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "cove-tunnel-origin.internal", MinVersion: tls.VersionTLS13}}, Timeout: 4 * time.Second}
	defer client.CloseIdleConnections()
	r, err := http.NewRequest("GET", os.Getenv("COVE_TEST_TUNNEL_ORIGIN_URL"), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = os.Getenv("COVE_TEST_TUNNEL_ORIGIN_HOST")
	r.Header.Set("X-Forwarded-For", "172.16.0.10")
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(response.StatusCode) != os.Getenv("COVE_TEST_TUNNEL_ORIGIN_STATUS") || !strings.Contains(string(body), os.Getenv("COVE_TEST_TUNNEL_ORIGIN_MARKER")) {
		t.Fatalf("origin response %d: %s", response.StatusCode, body)
	}
}

func TestCloudflareOriginTLSReloadDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_PROXY_RECOVERY_DOCKER") != "1" {
		t.Skip("owned isolated Docker daemon required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	labels, err := e.output(ctx, "docker", "info", "--format", "{{json .Labels}}")
	if err != nil || !strings.Contains(labels, "cove.test-proxy-recovery=cove-proxy-replay-") {
		t.Fatal("refusing an unowned daemon")
	}
	for _, kind := range []string{"container", "network", "volume"} {
		if _, err := e.output(ctx, "docker", kind, "inspect", "cove-ingress"); err == nil {
			t.Fatal("daemon already contains ingress")
		}
	}
	run := func(args ...string) {
		t.Helper()
		output, err := e.output(ctx, args...)
		if err != nil {
			t.Fatal(args, output, err)
		}
	}
	resources := [][2]string{}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for i := len(resources) - 1; i >= 0; i-- {
			r := resources[i]
			args := []string{"docker", r[0], "rm", r[1]}
			if r[0] == "container" {
				args = []string{"docker", "rm", "-f", r[1]}
			}
			if err := e.Run(cleanup, e.Root, args, nil, io.Discard); err != nil {
				t.Error(err)
			}
		}
	})
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	if err := e.ensureIngress(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	resources = append(resources, [2]string{"volume", "cove-ingress"}, [2]string{"network", "cove-ingress"}, [2]string{"container", "cove-ingress"})
	subnet, err := e.output(ctx, "docker", "network", "inspect", "--format", "{{(index .IPAM.Config 0).Subnet}}", "cove-ingress")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := idleAddress(subnet, 10)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := idleAddress(subnet, 11)
	if err != nil {
		t.Fatal(err)
	}
	host := "app.example.test"
	upstreamCert, upstreamKey := selfSignedCertificate(t, host)
	relayCert, relayKey := selfSignedCertificate(t, "cove-tunnel-origin.internal")
	dynamic := filepath.Join(e.Root, "dynamic")
	if err := os.MkdirAll(dynamic, 0700); err != nil {
		t.Fatal(err)
	}
	run("docker", "cp", dynamic, "cove-ingress:/data/")
	for name, data := range map[string][]byte{"origin.crt": upstreamCert, "origin.key": upstreamKey, "origin.yaml": []byte(`{"tls":{"stores":{"default":{"defaultCertificate":{"certFile":"/data/origin.crt","keyFile":"/data/origin.key"}}}}}`)} {
		path := filepath.Join(e.Root, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		target := "/data/" + name
		if name == "origin.yaml" {
			target = "/data/dynamic/" + name
		}
		run("docker", "cp", path, "cove-ingress:"+target)
	}
	run("docker", "run", "-d", "--name", "tunnel-web", "--label", "com.docker.compose.project=cove-tunnel-app", "--label", "com.docker.compose.service=app", "--entrypoint", "sh", "busybox:1.37", "-ec", "mkdir -p /www/api; echo tunnel-origin-marker >/www/api/index.html; exec httpd -f -p 8080 -h /www")
	resources = append(resources, [2]string{"container", "tunnel-web"})
	if err := e.applyIngress(ctx, "tunnel-app", []Route{{Domain: host, Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "default"}}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	proxy, err := e.output(ctx, "docker", "inspect", "--format", `{{(index .NetworkSettings.Networks "cove-ingress").IPAddress}}`, "cove-ingress")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := cloudflareOriginConfig{Peer: peer, Proxy: proxy, Roots: map[string]string{host: string(upstreamCert)}}
	configDir := filepath.Join(e.Root, "config")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"relay.crt": relayCert, "relay.key": relayKey} {
		if err := os.WriteFile(filepath.Join(configDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeJSON(filepath.Join(configDir, "relay.json"), config); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name, ip string
		args     []string
	}{{"tunnel-origin", origin, []string{"--env", "COVE_TEST_TUNNEL_ORIGIN_CHILD=1", "--entrypoint", "/fixture", "busybox:1.37", "-test.run=^TestCloudflareOriginHelperProcess$"}}, {"tunnel-client", peer, []string{"--entrypoint", "sh", "busybox:1.37", "-ec", "sleep 300"}}} {
		args := []string{"docker", "create", "--name", item.name, "--network", "cove-ingress", "--ip", item.ip}
		run(append(args, item.args...)...)
		resources = append(resources, [2]string{"container", item.name})
		run("docker", "cp", binary, item.name+":/fixture")
		if item.name == "tunnel-origin" {
			run("docker", "cp", configDir, item.name+":/")
		}
		run("docker", "start", item.name)
	}
	rootPath := filepath.Join(e.Root, "root.pem")
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		logs, _ := e.output(cleanup, "docker", "logs", "tunnel-origin")
		state, _ := e.output(cleanup, "docker", "inspect", "--format", "{{json .State}}", "tunnel-origin")
		t.Log("owned origin helper state", state, "logs", logs)
	})
	if err := os.WriteFile(rootPath, relayCert, 0600); err != nil {
		t.Fatal(err)
	}
	run("docker", "cp", rootPath, "tunnel-client:/root.pem")
	run("docker", "cp", rootPath, "cove-ingress:/root.pem")
	run("docker", "cp", binary, "cove-ingress:/fixture")
	probe := func(container, hostname, status, marker string) error {
		output, err := e.output(ctx, "docker", "exec", "-e", "COVE_TEST_TUNNEL_ORIGIN_CLIENT=1", "-e", "COVE_TEST_TUNNEL_ORIGIN_URL=https://"+origin+":8443/api/", "-e", "COVE_TEST_TUNNEL_ORIGIN_HOST="+hostname, "-e", "COVE_TEST_TUNNEL_ORIGIN_STATUS="+status, "-e", "COVE_TEST_TUNNEL_ORIGIN_MARKER="+marker, container, "/fixture", "-test.run=^TestCloudflareOriginClientProcess$")
		if err != nil {
			return fmt.Errorf("%w: %s", err, output)
		}
		return nil
	}
	wait := func(container, hostname, status, marker string) {
		t.Helper()
		for deadline := time.Now().Add(12 * time.Second); ; {
			err := probe(container, hostname, status, marker)
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	wait("tunnel-client", host, "200", "tunnel-origin-marker")
	wait("tunnel-client", "private.example.test", "404", "")
	wait("cove-ingress", host, "403", "")
	publish := func(data []byte) {
		t.Helper()
		file := filepath.Join(e.Root, "relay.next")
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		run("docker", "cp", file, "tunnel-origin:/config/relay.next")
		run("docker", "exec", "tunnel-origin", "mv", "/config/relay.next", "/config/relay.json")
	}
	bad, _ := selfSignedCertificate(t, host)
	config.Roots[host] = string(bad)
	data, _ := json.Marshal(config)
	publish(data)
	wait("tunnel-client", host, "502", "tunnel origin unavailable")
	config.Roots = map[string]string{}
	data, _ = json.Marshal(config)
	publish(data)
	wait("tunnel-client", host, "404", "")
	publish([]byte(`{"peer":`))
	wait("tunnel-client", host, "503", "tunnel origin unavailable")
	config.Roots = map[string]string{host: string(upstreamCert)}
	data, _ = json.Marshal(config)
	publish(data)
	wait("tunnel-client", host, "200", "tunnel-origin-marker")
	run("docker", "restart", "-t", "5", "tunnel-origin")
	wait("tunnel-client", host, "200", "tunnel-origin-marker")
}

func TestCloudflarePublishingKeepsCredentialsOutOfIngressAndRefusesPrivateRoutes(t *testing.T) {
	value := CloudflarePublishing{ID: strings.Repeat("a", 32), TunnelID: "11111111-2222-3333-4444-555555555555", AccountID: strings.Repeat("b", 32), TunnelSecret: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{37}, 32)), Hostnames: []string{"app.example.test"}}
	config, credentials, err := cloudflareConnectorFiles(value, "172.18.0.4")
	if err != nil || bytes.Contains(config, []byte(value.TunnelSecret)) || !bytes.Contains(credentials, []byte(value.TunnelSecret)) || bytes.Contains(config, []byte("noTLSVerify")) || !bytes.Contains(config, []byte("http_status:404")) {
		t.Fatal("unsafe connector files", err)
	}
	route := Route{Domain: "app.example.test", Service: "web", Port: 8080}
	if err := ValidatePublishedRoutes(value.Hostnames, map[string][]Route{"app": {route}}); err != nil {
		t.Fatal(err)
	}
	route.Policy = &RoutePolicy{IPAllowList: []string{"172.16.0.0/12"}}
	if ValidatePublishedRoutes(value.Hostnames, map[string][]Route{"app": {route}}) == nil {
		t.Fatal("tunnel would bypass connecting-address allowlist")
	}
	route.Policy = &RoutePolicy{Visibility: "lan", Listener: "office", CertificateSource: "default"}
	if ValidatePublishedRoutes(value.Hostnames, map[string][]Route{"app": {route}}) == nil {
		t.Fatal("private route published")
	}
	if ValidatePublishedRoutes(value.Hostnames, map[string][]Route{}) == nil {
		t.Fatal("missing route published")
	}
	for _, host := range []string{"*.example.test", "app.example.test/path", "app.example.test\n", "localhost"} {
		value.Hostnames = []string{host}
		if ValidateCloudflarePublishing(value) == nil {
			t.Fatal("unsafe tunnel hostname", host)
		}
	}
	value.Hostnames = []string{"app.example.test"}
	for _, origin := range []string{"8.8.8.8", "127.0.0.1", "[::1]", "cove-ingress", "172.18.0.4:443", "fd00::1%eth0"} {
		if _, _, err := cloudflareConnectorFiles(value, origin); err == nil {
			t.Fatal("unsafe origin", origin)
		}
	}
}

func TestCloudflareConnectorConfigurationDocker(t *testing.T) {
	binary := os.Getenv("COVE_TEST_CLOUDFLARED_BINARY")
	if binary == "" {
		t.Skip("requires pinned local cloudflared artifact and Docker")
	}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	owner := fmt.Sprintf("cove-tunnel-validate-%x", time.Now().UnixNano())
	value := CloudflarePublishing{ID: strings.Repeat("a", 32), TunnelID: "11111111-2222-3333-4444-555555555555", AccountID: strings.Repeat("b", 32), TunnelSecret: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{17}, 32)), Hostnames: []string{"app.example.test"}}
	config, credentials, err := cloudflareConnectorFiles(value, "172.18.0.3")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "create", "--name", owner, "--pull", "never", "--network", "none", "--label", "cove.test-tunnel-validation=" + owner, "--entrypoint", "sh", "busybox:1.37", "-ec", "chmod 0555 /cloudflared; /cloudflared --version; exec /cloudflared tunnel --config /config/config.json ingress validate"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		actual, err := e.output(cleanup, "docker", "inspect", "--format", `{{index .Config.Labels "cove.test-tunnel-validation"}}`, owner)
		if err != nil || actual != owner {
			t.Error("cloudflared validation ownership unavailable", err)
			return
		}
		if err := e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", owner}, nil, io.Discard); err != nil {
			t.Error(err)
		}
	})
	certificate, _ := selfSignedCertificate(t, "cove-tunnel-origin.internal")
	directory := filepath.Join(e.Root, "config")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"config.json": config, "credentials.json": credentials, "origin.pem": certificate} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for source, target := range map[string]string{binary: "/cloudflared", directory: "/"} {
		if output, err := e.output(ctx, "docker", "cp", source, owner+":"+target); err != nil {
			t.Fatal(err, output)
		}
	}
	output, err := e.output(ctx, "docker", "start", "-a", owner)
	if err != nil {
		t.Fatal(err)
	}
	exit, err := e.output(ctx, "docker", "inspect", "--format", "{{.State.ExitCode}}", owner)
	if err != nil || exit != "0" || !strings.Contains(output, "2026.11.1") {
		t.Fatal("pinned cloudflared config validation failed", output, exit, err)
	}
}

func TestCloudflareOriginRefusesUnownedPeersAndUnpublishedHosts(t *testing.T) {
	config := cloudflareOriginConfig{Peer: "172.18.0.2", Proxy: "172.18.0.3", Roots: map[string]string{"app.example.test": ""}}
	handler, closeIdle, err := newCloudflareOriginHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeIdle()
	for _, test := range []struct {
		peer, host string
		code       int
	}{{"172.18.0.4:4567", "app.example.test", 403}, {"172.18.0.2:4567", "private.example.test", 404}, {"172.18.0.2:4567", "app.example.test.evil.test", 404}, {"172.18.0.2:4567", "app.example.test:bad:host", 404}} {
		request := httptest.NewRequest("GET", "/", nil)
		request.RemoteAddr = test.peer
		request.Host = test.host
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.code {
			t.Fatal(test, response.Code)
		}
	}
	config.Roots["app.example.test"] = "not a certificate"
	if _, _, err := newCloudflareOriginHandler(config); err == nil {
		t.Fatal("invalid roots accepted")
	}
}

func TestCloudflareOriginRootsRequireSelectedCurrentlyValidIdentity(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := "app.example.test"
	route := Route{Domain: host, Service: "app", Port: 8080}
	routes := map[string][]Route{"app": {route}}
	roots, err := e.cloudflareOriginRoots([]string{host}, routes)
	if err != nil || len(roots) != 1 || roots[host] != "" {
		t.Fatal("public ACME trust", err)
	}
	route.Policy = &RoutePolicy{CertificateSource: "default"}
	routes["app"] = []Route{route}
	if _, err := e.cloudflareOriginRoots([]string{host}, routes); err == nil {
		t.Fatal("unverified proxy default accepted")
	}
	public, private := selfSignedCertificate(t, host)
	certificate := RouteCertificate{ID: strings.Repeat("a", 32), CertificatePEM: string(public), PrivateKeyPEM: string(private)}
	if err := e.saveRouteCertificate("app", certificate); err != nil {
		t.Fatal(err)
	}
	route.Policy = &RoutePolicy{CertificateSource: "uploaded", CertificateID: certificate.ID}
	routes["app"] = []Route{route}
	roots, err = e.cloudflareOriginRoots([]string{host}, routes)
	if err != nil || roots[host] != string(public) || strings.Contains(roots[host], "PRIVATE KEY") {
		t.Fatal("selected public identity", err)
	}
	route.Policy.CertificateID = strings.Repeat("b", 32)
	routes["app"] = []Route{route}
	if _, err := e.cloudflareOriginRoots([]string{host}, routes); err == nil {
		t.Fatal("missing certificate accepted")
	}
	route.Policy.CertificateID = certificate.ID
	route.Policy.IPAllowList = []string{"10.0.0.0/8"}
	routes["app"] = []Route{route}
	if _, err := e.cloudflareOriginRoots([]string{host}, routes); err == nil {
		t.Fatal("trust preparation bypassed source policy")
	}
}

func TestCloudflareOriginIdentityPersistsAndRefusesCorruption(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.ensureCloudflareOriginIdentity()
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := restarted.ensureCloudflareOriginIdentity()
	if err != nil || first != second {
		t.Fatal("origin trust changed after restart", err)
	}
	pair, err := tls.X509KeyPair([]byte(first.CertificatePEM), []byte(first.PrivateKeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.VerifyHostname("cove-tunnel-origin.internal") != nil {
		t.Fatal("wrong origin identity", err)
	}
	path := filepath.Join(e.Root, "cloudflare-publishing", "identity.json")
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.ensureCloudflareOriginIdentity(); err == nil {
		t.Fatal("corrupt identity silently replaced")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{broken" {
		t.Fatal("corrupt identity was overwritten", err)
	}
}
