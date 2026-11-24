package worker

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
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

func TestIngressExposureTransportRoutesDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_PROXY_RECOVERY_DOCKER") != "1" {
		t.Skip("requires owned isolated Docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	labels, err := e.output(ctx, "docker", "info", "--format", "{{json .Labels}}")
	if err != nil || !strings.Contains(labels, "cove.test-proxy-recovery=cove-proxy-replay-") {
		t.Fatal("transport test refuses an unowned daemon")
	}
	for _, kind := range []string{"container", "network", "volume"} {
		if _, err := e.output(ctx, "docker", kind, "inspect", "cove-ingress"); err == nil {
			t.Fatal("isolated daemon already contains ingress")
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
		var out strings.Builder
		if err := e.Run(ctx, e.Root, args, nil, &out); err != nil {
			t.Fatal(args, err, out.String())
		}
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	if err := e.ensureIngress(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	resources = append(resources, [2]string{"volume", "cove-ingress"}, [2]string{"network", "cove-ingress"}, [2]string{"container", "cove-ingress"})
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
		t.Fatal("owned daemon has no private IPv4")
	}
	settings := IngressExposureSettings{Listeners: []IngressListener{{ID: "socket", Protocol: "tcp", Visibility: "public", Bindings: []IngressBinding{{Address: address, Port: 19000}, {Address: "::1", Port: 19000}}}, {ID: "datagram", Protocol: "udp", Visibility: "public", Bindings: []IngressBinding{{Address: address, Port: 19001}, {Address: "::1", Port: 19001}}}}}
	plan, err := e.PlanIngressExposures(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.ChangeIngressExposures(ctx, IngressExposureRequest{Settings: settings, Revision: plan.Revision, Digest: plan.Digest, AcceptInterruption: true}); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	appCertificate, appKey := selfSignedCertificate(t, "database.example.test")
	for name, data := range map[string][]byte{"fixture.crt": appCertificate, "fixture.key": appKey} {
		if err := os.WriteFile(filepath.Join(e.Root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"replica-one", "replica-two", "other-upstream"} {
		service := "app"
		if name == "other-upstream" {
			service = "other"
		}
		run("docker", "create", "--name", name, "--label", "com.docker.compose.project=cove-transport-app", "--label", "com.docker.compose.service="+service, "--entrypoint", "/fixture", "busybox:1.37", "transport-route-fixture", name)
		resources = append(resources, [2]string{"container", name})
		run("docker", "cp", binary, name+":/fixture")
		for _, file := range []string{"fixture.crt", "fixture.key"} {
			run("docker", "cp", filepath.Join(e.Root, file), name+":"+"/"+file)
		}
		run("docker", "start", name)
	}
	tcp := Route{Protocol: "tcp", Service: "app", Port: 9000, Policy: &RoutePolicy{Listener: "socket", Visibility: "public", Upstreams: []RouteUpstream{{Service: "other", Port: 9000}}}}
	udp := Route{Protocol: "udp", Service: "app", Port: 9001, Policy: &RoutePolicy{Listener: "datagram", Visibility: "public", Upstreams: []RouteUpstream{{Service: "other", Port: 9001}}}}
	routes := []Route{tcp, udp}
	if err := e.applyIngress(ctx, "transport-app", routes, io.Discard); err != nil {
		t.Fatal(err)
	}
	probe := func(protocol string) (string, error) {
		port := "19000"
		if protocol == "udp" {
			port = "19001"
		}
		conn, err := net.DialTimeout(protocol, net.JoinHostPort(address, port), time.Second)
		if err != nil {
			return "", err
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(time.Second))
		if _, err = conn.Write([]byte("probe\n")); err != nil {
			return "", err
		}
		data := make([]byte, 128)
		n, err := conn.Read(data)
		return string(data[:n]), err
	}
	verify := func(protocol string) {
		t.Helper()
		seen := map[string]bool{}
		for i := 0; i < 24; i++ {
			body, err := probe(protocol)
			if err != nil || !strings.HasSuffix(body, ":probe\n") {
				t.Fatal(protocol, body, err)
			}
			seen[body] = true
		}
		if len(seen) != 3 {
			t.Fatal("transport did not balance replicas and upstream", protocol, seen)
		}
	}
	verify("tcp")
	verify("udp")
	for _, protocol := range []string{"tcp", "udp"} {
		port := "19000"
		if protocol == "udp" {
			port = "19001"
		}
		name := "ipv6-" + protocol + "-probe"
		run("docker", "create", "--name", name, "--network", "host", "--entrypoint", "/fixture", "busybox:1.37", "transport-route-probe", protocol, "[::1]:"+port)
		resources = append(resources, [2]string{"container", name})
		run("docker", "cp", binary, name+":/fixture")
		run("docker", "start", "-a", name)
		code, err := e.output(ctx, "docker", "inspect", "--format", "{{.State.ExitCode}}", name)
		if err != nil || code != "0" {
			t.Fatal("IPv6 transport probe failed", protocol, code, err)
		}
	}
	encrypted := tcp
	encrypted.Port = 9002
	encryptedPolicy := *tcp.Policy
	encryptedPolicy.Upstreams = []RouteUpstream{{Service: "other", Port: 9002}}
	encrypted.Policy = &encryptedPolicy
	if err := e.applyIngress(ctx, "transport-app", []Route{encrypted, udp}, io.Discard); err != nil {
		t.Fatal(err)
	}
	appRoots := x509.NewCertPool()
	appRoots.AppendCertsFromPEM(appCertificate)
	secure, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", net.JoinHostPort(address, "19000"), &tls.Config{RootCAs: appRoots, ServerName: "database.example.test", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal("application TLS was not passed through", err)
	}
	secure.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := secure.Write([]byte("probe\n")); err != nil {
		t.Fatal(err)
	}
	securedBody, err := bufio.NewReader(secure).ReadString('\n')
	secure.Close()
	if err != nil || !strings.HasSuffix(securedBody, ":probe\n") {
		t.Fatal(securedBody, err)
	}
	if err := e.applyIngress(ctx, "transport-app", routes, io.Discard); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.Root, "workloads", "transport-app", "suspension.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, workloadSuspension{Operation: "transport-pause", Options: SuspensionOptions{Reason: "environment", Scope: "environment"}, Phase: "paused", Created: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	saved, err := e.loadIngress()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.syncIngress(ctx, saved, io.Discard); err != nil {
		t.Fatal("paused transport routes", err)
	}
	for _, protocol := range []string{"tcp", "udp"} {
		if body, err := probe(protocol); err == nil && body != "" {
			t.Fatal("paused app received new traffic", protocol, body)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := e.syncIngress(ctx, saved, io.Discard); err != nil {
		t.Fatal(err)
	}
	verify("tcp")
	verify("udp")
	blocked := `tcp: {routers: {steal: {rule: 'HostSNI(` + "`*`" + `)', entryPoints: [cove-listener-socket], service: app}}}`
	if result := e.ChangeIngressConfig(ctx, IngressConfigRequest{Action: "validate", Name: "steal.yaml", Content: &blocked}); result.Code != "invalid" {
		t.Fatal("custom interception accepted", result)
	}
	if err := e.applyIngress(ctx, "foreign", []Route{tcp}, io.Discard); err == nil {
		t.Fatal("shared transport listener accepted")
	}
	if _, err := e.PlanIngressExposures(ctx, IngressExposureSettings{}); err == nil {
		t.Fatal("reserved listener removal accepted")
	}
	restarted, err := NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	restarted.ACMEDirectory = e.ACMEDirectory
	saved, err = restarted.loadIngress()
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.syncIngress(ctx, saved, io.Discard); err != nil {
		t.Fatal("worker restart", err)
	}
	verify("tcp")
	verify("udp")
	for _, name := range []string{"replica-one", "replica-two", "other-upstream"} {
		run("docker", "stop", "-t", "0", name)
	}
	if err := e.refreshTrafficIngress(ctx, "transport-app", io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"tcp", "udp"} {
		if body, err := probe(protocol); err == nil && body != "" {
			t.Fatal("stopped app received traffic", protocol, body)
		}
	}
	if err := e.applyIngress(ctx, "transport-app", routes, io.Discard); err != nil {
		t.Fatal("route save while stopped", err)
	}
	for _, name := range []string{"replica-one", "replica-two", "other-upstream"} {
		run("docker", "start", name)
	}
	if err := e.refreshTrafficIngress(ctx, "transport-app", io.Discard); err != nil {
		t.Fatal(err)
	}
	verify("tcp")
	verify("udp")
	if err := e.retireUDPIngress(ctx, newIngressConfig(), io.Discard); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	recovered.ACMEDirectory = e.ACMEDirectory
	if err := recovered.syncIngress(ctx, saved, io.Discard); err != nil {
		t.Fatal("restart between retirement and final publication", err)
	}
	if checkpoint, err := recovered.loadIngressTransition(); err != nil || checkpoint != nil {
		t.Fatal("settled transition checkpoint retained", err)
	}
	verify("tcp")
	verify("udp")
	for _, failAt := range []int{1, 2} {
		originalRun := e.Run
		publications := 0
		e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
			if len(args) == 6 && args[0] == "docker" && args[1] == "exec" && args[5] == "mkdir -p /data/dynamic; mv /data/routes.next /data/dynamic/routes.yaml" {
				publications++
				if publications == failAt {
					return fmt.Errorf("fixture interrupted UDP publication %d", failAt)
				}
			}
			return originalRun(ctx, dir, args, env, out)
		}
		err = e.applyIngress(ctx, "transport-app", nil, io.Discard)
		e.Run = originalRun
		if err == nil || publications <= failAt {
			t.Fatal("failed removal did not restore predecessor", err, publications)
		}
		verify("tcp")
		verify("udp")
	}
	if err := e.applyIngress(ctx, "transport-app", nil, io.Discard); err != nil {
		t.Fatal("transport removal", err)
	}
	for _, protocol := range []string{"tcp", "udp"} {
		if body, err := probe(protocol); err == nil && body != "" {
			t.Fatal("removed route received traffic", protocol, body)
		}
	}
	verifyRouteWebSockets(t, e, ctx, address, run)
}

func verifyRouteWebSockets(t *testing.T, e *Engine, ctx context.Context, address string, run func(...string)) {
	t.Helper()
	certificate, key := selfSignedCertificate(t, "websocket.example.test")
	dynamic := filepath.Join(e.Root, "dynamic")
	if err := os.MkdirAll(dynamic, 0700); err != nil {
		t.Fatal(err)
	}
	run("docker", "cp", dynamic, "cove-ingress:/data/")
	for name, data := range map[string][]byte{"socket.crt": certificate, "socket.key": key, "socket.yaml": []byte(`{"tls":{"stores":{"default":{"defaultCertificate":{"certFile":"/data/socket.crt","keyFile":"/data/socket.key"}}}}}`)} {
		path := filepath.Join(e.Root, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		target := "/data/" + name
		if name == "socket.yaml" {
			target = "/data/dynamic/" + name
		}
		run("docker", "cp", path, "cove-ingress:"+target)
	}
	route := Route{Domain: "websocket.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "default", Timeouts: &RouteTimeouts{DialSeconds: 1, ResponseHeaderSeconds: 1, IdleConnectionSeconds: 1}}}
	if err := e.applyIngress(ctx, "transport-app", []Route{route}, io.Discard); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certificate)
	tlsConfig := &tls.Config{RootCAs: roots, ServerName: route.Domain, MinVersion: tls.VersionTLS12}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	request, _ := http.NewRequestWithContext(ctx, "GET", "https://"+net.JoinHostPort(address, "443")+"/slow", nil)
	request.Host = route.Domain
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 504 {
		t.Fatal("response-header timeout not applied", response.StatusCode)
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", net.JoinHostPort(address, "443"), tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", route.Domain)
	reader := bufio.NewReader(conn)
	response, err = http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil || response.StatusCode != 101 || response.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatal("WebSocket upgrade failed", response, err)
	}
	for i := 0; i < 3; i++ {
		// The pooled idle timeout must not terminate an upgraded session, including while it is idle.
		time.Sleep(1500 * time.Millisecond)
		frame := []byte{0x81, 0x84, 1, 2, 3, 4, 'p' ^ 1, 'i' ^ 2, 'n' ^ 3, 'g' ^ 4}
		if _, err := conn.Write(frame); err != nil {
			t.Fatal(err)
		}
		var echoed [6]byte
		if _, err := io.ReadFull(reader, echoed[:]); err != nil || string(echoed[2:]) != "ping" || echoed[0] != 0x81 {
			t.Fatal("upgraded traffic failed after idle", echoed, err)
		}
	}
}
