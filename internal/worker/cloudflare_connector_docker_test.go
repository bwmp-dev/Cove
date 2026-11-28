package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func runCloudflareFakeEdge(ctx context.Context) error {
	data, err := os.ReadFile("/fake-edge/proof.json")
	if err != nil {
		return err
	}
	server := &http.Server{Addr: ":8443", ReadHeaderTimeout: 3 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		if _, err := os.Stat("/fake-edge/online"); err != nil {
			w.WriteHeader(503)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/auth" || !bytes.Equal(body, data) {
			w.WriteHeader(403)
			return
		}
		w.WriteHeader(200)
	})}
	go func() { <-ctx.Done(); server.Close() }()
	err = server.ListenAndServeTLS("/fake-edge/ca.pem", "/fake-edge/key.pem")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func runCloudflareFakeConnector(ctx context.Context) error {
	public, err := os.ReadFile("/fake-edge/ca.pem")
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(public) {
		return errors.New("invalid fake edge root")
	}
	credential, err := os.ReadFile("/config/credentials.json")
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "fake-edge.internal", MinVersion: tls.VersionTLS13}}}
	var authenticated atomic.Bool
	server := &http.Server{Addr: "127.0.0.1:20241", ReadHeaderTimeout: 3 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ready" || !authenticated.Load() {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	})}
	go func() { <-ctx.Done(); server.Close() }()
	go func() {
		for {
			request, _ := http.NewRequestWithContext(ctx, "POST", os.Getenv("COVE_TEST_FAKE_EDGE_URL"), bytes.NewReader(credential))
			response, err := client.Do(request)
			ok := false
			if err == nil {
				ok = response.StatusCode == 200
				response.Body.Close()
			}
			authenticated.Store(ok)
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}()
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func runCloudflareFixtureClient(ctx context.Context) error {
	data, err := os.ReadFile("/config/config.json")
	if err != nil {
		return err
	}
	var config struct {
		Ingress []struct {
			Hostname, Service string
			OriginRequest     struct {
				OriginServerName string `json:"originServerName"`
			}
		}
	}
	if json.Unmarshal(data, &config) != nil {
		return errors.New("invalid connector fixture configuration")
	}
	root, err := os.ReadFile("/config/origin.pem")
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(root) {
		return errors.New("invalid origin fixture root")
	}
	for _, entry := range config.Ingress {
		if entry.Hostname != "app.example.test" {
			continue
		}
		request, err := http.NewRequestWithContext(ctx, "GET", entry.Service+"/", nil)
		if err != nil {
			return err
		}
		request.Host = entry.Hostname
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: entry.OriginRequest.OriginServerName, MinVersion: tls.VersionTLS13}}}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		want := os.Getenv("COVE_TEST_TUNNEL_STATUS")
		if fmt.Sprint(response.StatusCode) != want || want == "200" && !bytes.Contains(body, []byte("tunnel-lifecycle-marker")) {
			return fmt.Errorf("origin status %d, wanted %s", response.StatusCode, want)
		}
		return nil
	}
	return errors.New("fixture hostname missing")
}

func TestCloudflareTunnelLifecycleDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_CLOUDFLARE_TUNNEL_DOCKER") != "1" {
		t.Skip("requires owned isolated Tunnel fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	e, err := NewEngine("/fixture-root/engine")
	if err != nil {
		t.Fatal(err)
	}
	labels, err := e.output(ctx, "docker", "info", "--format", "{{json .Labels}}")
	if err != nil || !strings.Contains(labels, "cove.test-tunnel-recovery=cove-tunnel-replay-") {
		t.Fatal("refusing unowned Docker daemon")
	}
	run := func(args ...string) string {
		t.Helper()
		data, err := e.output(ctx, args...)
		if err != nil {
			t.Fatal(args, err, data)
		}
		return data
	}
	image, err := e.linkGatewayImage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	value := CloudflarePublishing{ID: strings.Repeat("a", 32), TunnelID: "11111111-2222-3333-4444-555555555555", AccountID: strings.Repeat("b", 32), TunnelSecret: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{43}, 32)), Hostnames: []string{"app.example.test"}}
	fakeDir := "/fixture-root/fake-edge"
	if err := os.MkdirAll(fakeDir, 0700); err != nil {
		t.Fatal(err)
	}
	public, private := selfSignedCertificate(t, "fake-edge.internal")
	_, proof, err := cloudflareConnectorFiles(value, "172.16.0.3")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"ca.pem": public, "key.pem": private, "proof.json": proof, "online": []byte("online")} {
		if err := os.WriteFile(filepath.Join(fakeDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("docker", "run", "-d", "--name", "cove-fake-edge", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--mount", "type=bind,source="+fakeDir+",target=/fake-edge,readonly", image, "cloudflare-fake-edge")
	edge := run("docker", "inspect", "--format", `{{(index .NetworkSettings.Networks "bridge").IPAddress}}`, "cove-fake-edge")
	normal := e.Run
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		if len(args) > 3 && args[0] == "docker" && args[1] == "image" && args[2] == "inspect" && (args[len(args)-1] == DefaultCloudflaredImage || args[len(args)-1] == strings.TrimPrefix(DefaultCloudflaredImage, "docker.io/")) {
			copy := slices.Clone(args)
			copy[len(copy)-1] = image
			return normal(ctx, dir, copy, env, out)
		}
		if len(args) > 2 && args[0] == "docker" && args[1] == "create" {
			if position := slices.IndexFunc(args, func(value string) bool {
				return value == DefaultCloudflaredImage || value == strings.TrimPrefix(DefaultCloudflaredImage, "docker.io/")
			}); position >= 0 {
				copy := slices.Clone(args)
				copy[position] = image
				copy = append(copy[:position], append([]string{"--env", "COVE_TEST_FAKE_EDGE_URL=https://" + edge + ":8443/auth", "--mount", "type=bind,source=" + fakeDir + ",target=/fake-edge,readonly"}, copy[position:]...)...)
				return normal(ctx, dir, copy, env, out)
			}
		}
		return normal(ctx, dir, args, env, out)
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	run("docker", "run", "-d", "--name", "tunnel-app", "--label", "com.docker.compose.project=cove-app", "--label", "com.docker.compose.service=app", "--entrypoint", "sh", "busybox:1.37", "-ec", "mkdir /www; echo tunnel-lifecycle-marker >/www/index.html; exec httpd -f -p 8080 -h /www")
	certificate, key := selfSignedCertificate(t, "app.example.test")
	stored := RouteCertificate{ID: strings.Repeat("c", 32), CertificatePEM: string(certificate), PrivateKeyPEM: string(key)}
	if err := e.saveRouteCertificate("app", stored); err != nil {
		t.Fatal(err)
	}
	route := Route{Domain: "app.example.test", Service: "app", Port: 8080, Policy: &RoutePolicy{CertificateSource: "uploaded", CertificateID: stored.ID}}
	if err := e.applyIngress(ctx, "app", []Route{route}, io.Discard); err != nil {
		t.Fatal(err)
	}
	var status CloudflarePublishingStatus
	for deadline := time.Now().Add(25 * time.Second); ; {
		status, err = e.ApplyCloudflarePublishing(ctx, &value)
		if err == nil && status.Connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("local fake authenticated lifecycle did not connect", status, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	state, err := e.readCloudflarePublishing()
	if err != nil || state.Active == nil {
		t.Fatal(err)
	}
	unlock, err := e.lockNodeChange(false)
	if err != nil {
		t.Fatal(err)
	}
	status, err = e.ApplyCloudflarePublishing(ctx, &value)
	unlock()
	if err != nil || !status.Connected {
		t.Fatal("unchanged active publishing attempted another mutation instead of returning verified status", err)
	}
	probe := func(want string) {
		t.Helper()
		run("docker", "exec", "-e", "COVE_TEST_TUNNEL_STATUS="+want, state.Active.Connector, "/cove-worker", "cloudflare-fixture-client")
	}
	probe("200")
	inventory, err := os.ReadFile(filepath.Join(e.Root, "ingress.json"))
	if err != nil || !bytes.Contains(inventory, []byte("cove-protected-tunnel-v1")) {
		t.Fatal("older workers could read the active tunnel routing inventory", err)
	}
	originConfigPath := e.cloudflarePublishingPath("generations", state.Active.Revision, "origin", "relay.json")
	originalConfig, err := os.ReadFile(originConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var originConfig cloudflareOriginConfig
	if json.Unmarshal(originalConfig, &originConfig) != nil {
		t.Fatal("invalid saved relay config")
	}
	originConfig.CAPEM = string(certificate)
	originConfig.Roots[route.Domain] = ""
	if err := writeJSON(originConfigPath, originConfig); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		if e.Run(ctx, e.Root, []string{"docker", "exec", state.Active.Relay, "/cove-worker", "cloudflare-origin-probe"}, nil, io.Discard) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scratch helper did not use the worker-provided public CA pool")
		}
		time.Sleep(100 * time.Millisecond)
	}
	probe("200")
	if err := writeFileAtomic(originConfigPath, originalConfig, 0600); err != nil {
		t.Fatal(err)
	}
	originalGeneration := *state.Active
	if err := os.Remove(filepath.Join(fakeDir, "online")); err != nil {
		t.Fatal(err)
	}
	value.Image = strings.TrimPrefix(DefaultCloudflaredImage, "docker.io/")
	pending, err := e.ApplyCloudflarePublishing(ctx, &value)
	if err != nil || pending.Connected {
		t.Fatal("failed candidate was reported connected", pending, err)
	}
	waiting, err := e.readCloudflarePublishing()
	if err != nil || waiting.Active == nil || waiting.Candidate == nil || waiting.Active.Revision != originalGeneration.Revision {
		t.Fatal("unready candidate replaced the active connector", err)
	}
	failedCandidate := *waiting.Candidate
	value.Image = ""
	if _, err := e.ApplyCloudflarePublishing(ctx, &value); err != nil {
		t.Fatal(err)
	}
	reverted, err := e.readCloudflarePublishing()
	if err != nil || reverted.Candidate != nil || len(reverted.Retired) > 0 || reverted.Active.Revision != originalGeneration.Revision {
		t.Fatal("return to active did not retire failed candidate", err)
	}
	if text := run("docker", "ps", "-aq", "--filter", "name=^/"+failedCandidate.Connector+"$"); text != "" {
		t.Fatal("failed connector survived reversion")
	}
	if err := os.WriteFile(filepath.Join(fakeDir, "online"), []byte("online"), 0600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		status, err = e.ApplyCloudflarePublishing(ctx, &value)
		if err == nil && status.Connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("active connector did not recover local fake edge", status, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	state, err = e.readCloudflarePublishing()
	if err != nil {
		t.Fatal(err)
	}
	probe("200")
	// The outer harness runs the original pinned image offline, avoiding Docker's incomplete image export.
	configDir := e.cloudflarePublishingPath("generations", state.Active.Revision, "connector")
	if err := os.MkdirAll("/fixture-root/offline-connector", 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.json", "credentials.json", "origin.pem"} {
		data, err := os.ReadFile(filepath.Join(configDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("/fixture-root/offline-connector", name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}

	route.Policy.IPAllowList = []string{"10.0.0.0/8"}
	if err := e.applyIngress(ctx, "app", []Route{route}, io.Discard); err != nil {
		t.Fatal(err)
	}
	probe("404")
	restarted, err := NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Run = e.Run
	if err := restarted.restrictCloudflareOrigins(ctx, map[string][]Route{}); err != nil {
		t.Fatal(err)
	}
	probe("404")
	status, err = restarted.ApplyCloudflarePublishing(ctx, nil)
	if err != nil || status.Phase != "disabled" || status.Running || status.Connected {
		t.Fatal("disable not confirmed", status, err)
	}
	inventory, err = os.ReadFile(filepath.Join(e.Root, "ingress.json"))
	if err != nil || bytes.Contains(inventory, []byte("cove-protected-tunnel-v1")) {
		t.Fatal("confirmed disable did not release the tunnel hold", err)
	}
	for _, name := range []string{state.Active.Connector, state.Active.Relay} {
		if text := run("docker", "ps", "-aq", "--filter", "name=^/"+name+"$"); text != "" {
			t.Fatal("owned connector remains after disable")
		}
	}
}
