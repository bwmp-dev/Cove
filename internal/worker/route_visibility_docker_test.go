package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIngressExposurePrivateRoutingDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_PROXY_RECOVERY_DOCKER") != "1" {
		t.Skip("requires owned isolated Docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	labels, err := e.output(ctx, "docker", "info", "--format", "{{json .Labels}}")
	if err != nil || !strings.Contains(labels, "cove.test-proxy-recovery=cove-proxy-replay-") {
		t.Fatal("private route test refuses an unowned daemon")
	}
	for _, kind := range []string{"container", "network", "volume"} {
		if _, err := e.output(ctx, "docker", kind, "inspect", "cove-ingress"); err == nil {
			t.Fatal("isolated daemon already contains ingress")
		}
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	if err := e.ensureIngress(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, args := range [][]string{{"docker", "rm", "-f", "private-web"}, {"docker", "rm", "-f", "cove-ingress"}, {"docker", "network", "rm", "cove-ingress"}, {"docker", "volume", "rm", "cove-ingress"}} {
			if err := e.Run(cleanup, e.Root, args, nil, io.Discard); err != nil {
				t.Error(err)
			}
		}
	})
	addresses, err := net.LookupHost("daemon")
	if err != nil {
		t.Fatal(err)
	}
	address := ""
	for _, value := range addresses {
		if ip := net.ParseIP(value); ip != nil && ip.To4() != nil && ip.IsPrivate() {
			address = value
			break
		}
	}
	if address == "" {
		t.Fatal("isolated daemon has no private IPv4 address")
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "network", "create", "--subnet", "100.64.51.0/24", "--gateway", "100.64.51.1", "--opt", "com.docker.network.bridge.enable_ip_masquerade=false", "fixture-tailnet"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if err := e.Run(cleanup, e.Root, []string{"docker", "network", "rm", "fixture-tailnet"}, nil, io.Discard); err != nil {
			t.Error(err)
		}
	})
	settings := IngressExposureSettings{Listeners: []IngressListener{{ID: "office", Protocol: "https", Visibility: "lan", Bindings: []IngressBinding{{Address: address, Port: 18443}}}, {ID: "tailnet", Protocol: "https", Visibility: "tailscale", Bindings: []IngressBinding{{Address: "100.64.51.1", Port: 18444}}}}}
	plan, err := e.PlanIngressExposures(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ChangeIngressExposures(ctx, IngressExposureRequest{Settings: settings, Revision: plan.Revision, Digest: plan.Digest, AcceptInterruption: true}); err != nil {
		t.Fatal(err)
	}
	dynamic := filepath.Join(e.Root, "dynamic")
	if err := os.MkdirAll(dynamic, 0700); err != nil {
		t.Fatal(err)
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "cp", dynamic, "cove-ingress:/data/"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	certificate, key := selfSignedCertificate(t, "office.example.test")
	for name, data := range map[string][]byte{"private.crt": certificate, "private.key": key, "default.yaml": []byte(`{"tls":{"stores":{"default":{"defaultCertificate":{"certFile":"/data/private.crt","keyFile":"/data/private.key"}}}}}`)} {
		path := filepath.Join(e.Root, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		target := "/data/" + name
		if name == "default.yaml" {
			target = "/data/dynamic/default.yaml"
		}
		if err := e.Run(ctx, e.Root, []string{"docker", "cp", path, "cove-ingress:" + target}, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "run", "-d", "--name", "private-web", "--label", "com.docker.compose.project=cove-private-app", "--label", "com.docker.compose.service=web", "--entrypoint", "sh", "busybox:1.37", "-ec", "mkdir -p /www/api; echo private-route-marker >/www/api/index.html; echo public-fallback-marker >/www/index.html; exec httpd -f -p 8080 -h /www"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	route := Route{Domain: "office.example.test", Service: "web", Port: 8080, Policy: &RoutePolicy{Visibility: "lan", Listener: "office", PathPrefix: "/api", CertificateSource: "default"}}
	if err := e.applyIngress(ctx, "private-app", []Route{route}, io.Discard); err != nil {
		t.Fatal(err)
	}
	fallback := fmt.Sprintf(`{"http":{"routers":{"fallback":{"rule":"PathPrefix(`+"`/`"+`)","entryPoints":["websecure"],"service":"fallback","priority":100,"tls":{}}},"services":{"fallback":{"loadBalancer":{"servers":[{"url":"http://%s:8080"}]}}}}}`, routeAlias("private-app", "web"))
	fallbackPath := filepath.Join(e.Root, "fallback.yaml")
	if err := os.WriteFile(fallbackPath, []byte(fallback), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "cp", fallbackPath, "cove-ingress:/data/dynamic/fallback.yaml"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certificate)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: route.Domain, MinVersion: tls.VersionTLS12}, DisableKeepAlives: true}, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	probe := func(port int, path string) (int, string, error) {
		request, _ := http.NewRequestWithContext(ctx, "GET", "https://"+net.JoinHostPort(address, fmt.Sprint(port))+path, nil)
		request.Host = route.Domain
		response, err := client.Do(request)
		if err != nil {
			return 0, "", err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		return response.StatusCode, string(body), err
	}
	wait := func(port int, path string, status int, marker string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			code, body, err := probe(port, path)
			if err == nil && code == status && (marker == "" || strings.Contains(body, marker)) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("private probe %d%s: status=%d body=%q error=%v", port, path, code, body, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	wait(18443, "/api/", 200, "private-route-marker")
	wait(443, "/api/", 418, "")
	wait(443, "/other", 418, "")
	state, err := e.loadIngressExposures()
	if err != nil {
		t.Fatal(err)
	}
	privatePort := state.Ports["office"]
	denied, _ := e.output(ctx, "docker", "exec", "cove-ingress", "wget", "-S", "-O", "/dev/null", "--no-check-certificate", "--header=Host: "+route.Domain, fmt.Sprintf("https://127.0.0.1:%d/api/", privatePort))
	if !strings.Contains(denied, "403") {
		t.Fatal("non-private source reached private listener", denied)
	}
	if _, err := e.PlanIngressExposures(ctx, IngressExposureSettings{}); err == nil {
		t.Fatal("referenced private listener could be removed")
	}
	blocked := `tcp: {routers: {intercept: {rule: 'HostSNI(` + "`*`" + `)', entryPoints: [websecure], service: app}}}`
	result := e.ChangeIngressConfig(ctx, IngressConfigRequest{Action: "validate", Name: "intercept.yaml", Content: &blocked})
	if result.Code != "invalid" {
		t.Fatal("shared TCP bypass was not refused", result)
	}
	for _, entries := range []string{"", ", entryPoints: [cove-listener-office]"} {
		content := "http: {routers: {intercept: {rule: 'PathPrefix(`/`)', service: app" + entries + "}}}"
		result := e.ChangeIngressConfig(ctx, IngressConfigRequest{Action: "validate", Name: "intercept.yaml", Content: &content})
		if result.Code != "invalid" {
			t.Fatal("custom HTTP private-listener bypass was not refused", result)
		}
	}
	before, err := e.output(ctx, "docker", "inspect", "--format", "{{.Id}}|{{.State.StartedAt}}", "private-web")
	if err != nil {
		t.Fatal(err)
	}
	e, err = NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := e.loadIngress()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.syncIngress(ctx, saved, io.Discard); err != nil {
		t.Fatal("private routes did not survive worker restart", err)
	}
	wait(18443, "/api/", 200, "private-route-marker")
	wait(443, "/other", 418, "")
	invalid := route
	policy := *route.Policy
	policy.Listener = "missing"
	invalid.Policy = &policy
	if err := e.applyIngress(ctx, "private-app", []Route{invalid}, io.Discard); err == nil {
		t.Fatal("missing listener silently fell back to public")
	}
	wait(18443, "/api/", 200, "private-route-marker")
	wait(443, "/api/", 418, "")
	after, err := e.output(ctx, "docker", "inspect", "--format", "{{.Id}}|{{.State.StartedAt}}", "private-web")
	if err != nil || before != after {
		t.Fatal("route update replaced application", err)
	}
	restored, err := e.loadIngress()
	if err != nil || !RoutesEqual(restored["private-app"], []Route{route}) {
		t.Fatal("failed route did not restore private state", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "cp", executable, "cove-ingress:/tmp/private-probe"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	tail := route
	tailPolicy := *route.Policy
	tailPolicy.Visibility, tailPolicy.Listener = "tailscale", "tailnet"
	tail.Policy = &tailPolicy
	if err := e.applyIngress(ctx, "private-app", []Route{tail}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "create", "--name", "tailnet-probe", "--network", "host", "--entrypoint", "/tmp/private-probe", ingressImage, "private-route-probe", "100.64.51.1:18444", route.Domain, "/tmp/root.crt", "200", "private-route-marker", "100.64.51.1"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if err := e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "tailnet-probe"}, nil, io.Discard); err != nil {
			t.Error(err)
		}
	})
	for _, item := range [][2]string{{executable, "/tmp/private-probe"}, {filepath.Join(e.Root, "private.crt"), "/tmp/root.crt"}} {
		if err := e.Run(ctx, e.Root, []string{"docker", "cp", item[0], "tailnet-probe:" + item[1]}, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	output, runErr := e.output(ctx, "docker", "start", "-a", "tailnet-probe")
	exit, inspectErr := e.output(ctx, "docker", "inspect", "--format", "{{.State.ExitCode}}", "tailnet-probe")
	if runErr != nil || inspectErr != nil || exit != "0" {
		t.Fatal("simulated Tailscale source could not reach its bound route", runErr, inspectErr, output, exit)
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "exec", "cove-ingress", "/tmp/private-probe", "private-route-probe", fmt.Sprintf("127.0.0.1:%d", state.Ports["tailnet"]), route.Domain, "/data/private.crt", "403", ""}, nil, io.Discard); err != nil {
		t.Fatal("non-Tailscale source was not denied", err)
	}
	wait(443, "/other", 418, "")
	renamed := tail
	renamed.Domain = "second.office.example.test"
	if err := e.applyIngress(ctx, "private-app", []Route{renamed}, io.Discard); err != nil {
		t.Fatal("private hostname change could not retire its previous denials", err)
	}
	if err := e.applyIngress(ctx, "private-app", []Route{route}, io.Discard); err != nil {
		t.Fatal("private hostname rollback could not restore previous denials", err)
	}
	wait(18443, "/api/", 200, "private-route-marker")
	wait(443, "/other", 418, "")
	data, _ := json.Marshal(restored)
	if !strings.Contains(string(data), `"visibility":"lan"`) {
		t.Fatal("private route metadata disappeared")
	}
}

func runPrivateRouteTestProbe() error {
	roots := x509.NewCertPool()
	certificate, err := os.ReadFile(os.Args[4])
	if err != nil {
		return err
	}
	if !roots.AppendCertsFromPEM(certificate) {
		return fmt.Errorf("fixture root is invalid")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if len(os.Args) == 8 {
		ip := net.ParseIP(os.Args[7])
		if ip == nil {
			return fmt.Errorf("fixture source address is invalid")
		}
		dialer.LocalAddr = &net.TCPAddr{IP: ip}
	}
	client := &http.Client{Transport: &http.Transport{DialContext: dialer.DialContext, TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: os.Args[3], MinVersion: tls.VersionTLS12}}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	request, err := http.NewRequest("GET", "https://"+os.Args[2]+"/api/", nil)
	if err != nil {
		return err
	}
	request.Host = os.Args[3]
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return err
	}
	if fmt.Sprint(response.StatusCode) != os.Args[5] || !strings.Contains(string(body), os.Args[6]) {
		return fmt.Errorf("fixture probe status %d did not match expected %s", response.StatusCode, os.Args[5])
	}
	return nil
}
