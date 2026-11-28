package worker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestCustomIngressDocker exercises custom configuration against a real Traefik proxy. It needs a
// Docker daemon without an existing cove-ingress and runs only with COVE_TEST_INGRESS_DOCKER=1.
func TestCustomIngressDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_INGRESS_DOCKER") != "1" {
		t.Skip("set COVE_TEST_INGRESS_DOCKER=1 to run against a local Docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// An unreachable CA keeps the test proxy from requesting certificates for test hostnames.
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	for _, kind := range []string{"container", "network", "volume"} {
		if _, err := e.output(ctx, "docker", kind, "inspect", "cove-ingress"); err == nil {
			t.Skip("a cove-ingress " + kind + " already exists on this Docker daemon")
		}
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		for _, command := range [][]string{{"docker", "rm", "-f", "cove-ingress"}, {"docker", "network", "rm", "cove-ingress"}, {"docker", "volume", "rm", "cove-ingress"}} {
			_ = e.Run(cleanup, e.Root, command, nil, io.Discard)
		}
		e.removeValidators(cleanup)
	})
	// Staged mode creates the proxy without publishing host ports; normal mode is restored afterwards.
	if err = writeJSON(filepath.Join(e.Root, "ingress-mode.json"), migrationIngressMode{Staged: true, TLSChallenge: true}); err != nil {
		t.Fatal(err)
	}
	if err = e.ensureIngress(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(e.Root, "ingress-mode.json")); err != nil {
		t.Fatal(err)
	}
	routes := map[string][]Route{"app": {{Domain: "app.example.test", Service: "web", Port: 80}}}
	if err = writeJSON(filepath.Join(e.Root, "ingress.json"), routes); err != nil {
		t.Fatal(err)
	}
	if err = e.syncIngress(ctx, routes, io.Discard); err != nil {
		t.Fatal(err)
	}
	policies := map[string][]Route{"app": {{Domain: "app.example.test", Service: "web", Port: 80, Policy: &RoutePolicy{
		PathPrefix: "/api", SecurityHeaders: true, RequestHeaders: map[string]string{"X-App": "api"},
		ResponseHeaders: map[string]string{"X-Version": "1"}, IPAllowList: []string{"10.0.0.0/8", "fd00::/8"},
		RedirectHost: "www.example.test", RedirectPermanent: true, CertificateSource: "default",
		Upstreams: []RouteUpstream{{Service: "api", Port: 8080}}, Sticky: true,
	}}}}
	if err = e.syncIngress(ctx, policies, io.Discard); err != nil {
		t.Fatal("Traefik rejected route policies:", err)
	}
	if err = e.syncIngress(ctx, routes, io.Discard); err != nil {
		t.Fatal("could not restore legacy route:", err)
	}
	started, err := e.output(ctx, "docker", "inspect", "--format", "{{.Id}} {{.State.StartedAt}}", "cove-ingress")
	if err != nil {
		t.Fatal(err)
	}
	legacy := `# Imported from the previous proxy; comments and ordering must survive.
http:
  routers:
    legacy:
      rule: Host(` + "`legacy.example.test`" + `)
      entryPoints: [websecure]
      priority: 50
      service: legacy
      middlewares: [legacy-auth]
      tls: {}
    broken:
      rule: Host(` + "`broken.example.test`" + `)
      entryPoints: [websecure]
      service: legacy
      middlewares: [never-defined]
  middlewares:
    legacy-auth:
      basicAuth:
        users: ["admin:$apr1$Q2bTqGzE$H3xS1yQ9cS1n3RjHbB2vX0"]
  services:
    legacy:
      loadBalancer:
        servers: [{url: "http://10.255.0.1:8080"}]
`
	staged := filepath.Join(t.TempDir(), "legacy.yaml")
	if err = os.WriteFile(staged, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "cp", staged, "cove-ingress:/data/dynamic/legacy-import.yaml"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	waitForLive(t, e, "http.routers/legacy")

	state, err := e.IngressConfig(ctx)
	if err != nil || !state.Running || state.Blocked != "" {
		t.Fatalf("state unavailable: %+v %v", state, err)
	}
	statuses := map[string]string{}
	for _, file := range state.Files {
		statuses[file.Name] = file.Status
	}
	if statuses["routes.yaml"] != "protected" || statuses["legacy-import.yaml"] != "unmanaged" {
		t.Fatalf("unexpected files: %+v", state.Files)
	}
	if !slices.ContainsFunc(state.EntryPoints, func(entry IngressEntrypoint) bool { return entry.Name == "websecure" }) || !slices.Equal(state.Resolvers, []string{"cove"}) {
		t.Fatalf("static configuration not reported: %+v %v", state.EntryPoints, state.Resolvers)
	}
	file, err := e.IngressConfigFile(ctx, "legacy-import.yaml")
	if err != nil || file.Content == nil || *file.Content != legacy {
		t.Fatalf("legacy file not returned exactly: %v", err)
	}
	if !slices.ContainsFunc(file.Objects, func(object IngressConfigObject) bool {
		return object.Name == "broken" && object.Status == "disabled" && len(object.Errors) > 0
	}) {
		t.Fatalf("live object errors not reported: %+v", file.Objects)
	}
	revision := file.Revision
	id := 0
	change := func(action, name string, content *string, revision string) IngressConfigResult {
		id++
		return e.ChangeIngressConfig(ctx, IngressConfigRequest{Action: action, ChangeID: "docker-" + string(rune('a'+id)), Name: name, Content: content, Revision: revision})
	}
	text := func(value string) *string { return &value }

	if result := change("put", "legacy-import.yaml", text(legacy), revision); result.Code != "invalid" {
		t.Fatalf("unmanaged file edited without import: %+v", result)
	}
	if result := change("import", "legacy-import.yaml", nil, revision); result.Change == nil || result.Change.State != "succeeded" || result.Change.Revision != revision {
		t.Fatalf("import failed: %+v", result)
	}
	if current, _, _ := e.readProxyFile(ctx, mustProxy(t, e), "legacy-import.yaml"); current != legacy {
		t.Fatal("import changed the file")
	}

	edited := strings.Replace(legacy, "  services:\n", `    compress:
      compress: {}
  services:
`, 1)
	edited = strings.Replace(edited, "middlewares: [legacy-auth]", "middlewares: [legacy-auth, compress]", 1)
	assertRejected(t, change("put", "legacy-import.yaml", text(strings.Replace(edited, "[legacy-auth, compress]", "[legacy-auth, missing]", 1)), revision), "does not exist")
	assertRejected(t, change("put", "legacy-import.yaml", text(strings.Replace(edited, "Host(`legacy.example.test`)", "Host(`app.example.test`)", 1)), revision), "app.example.test")
	assertRejected(t, change("put", "legacy-import.yaml", text(strings.Replace(edited, "entryPoints: [websecure]\n      priority", "entryPoints: [metrics]\n      priority", 1)), revision), "static configuration")
	assertRejected(t, change("put", "legacy-import.yaml", text(strings.Replace(edited, "rule: Host(`legacy", "rulee: Host(`legacy", 1)), revision), "rulee")
	assertRejected(t, change("put", "legacy-import.yaml", text(edited+"tls:\n  certificates:\n    - certFile: /data/certs/missing.pem\n      keyFile: /data/certs/missing.key\n"), revision), "not readable")
	if current, _, _ := e.readProxyFile(ctx, mustProxy(t, e), "legacy-import.yaml"); current != legacy {
		t.Fatal("a rejected change reached the proxy")
	}

	result := change("put", "legacy-import.yaml", text(edited), revision)
	if result.Change == nil || result.Change.State != "succeeded" {
		t.Fatalf("valid edit failed: %+v", result)
	}
	waitForLive(t, e, "http.middlewares/compress")
	editedRevision := result.Change.Revision
	if result = change("put", "legacy-import.yaml", text(legacy), revision); result.Code != "conflict" {
		t.Fatalf("stale revision accepted: %+v", result)
	}
	if err = e.customHostConflict(nil, []Route{{Domain: "legacy.example.test", Service: "web", Port: 80}}); err == nil {
		t.Fatal("resource route could take a custom hostname")
	}

	tcp := "tcp:\n  routers:\n    database:\n      rule: HostSNI(`db.example.test`)\n      entryPoints: [websecure]\n      service: database\n      tls:\n        passthrough: true\n  services:\n    database:\n      loadBalancer:\n        servers:\n          - address: 10.255.0.2:5432\n"
	if result = change("put", "database.yaml", text(tcp), ""); result.Change == nil || result.Change.State != "succeeded" {
		t.Fatalf("TCP file failed: %+v", result)
	}
	waitForLive(t, e, "tcp.routers/database")
	if result = change("delete", "database.yaml", nil, contentRevision(tcp)); result.Change == nil || result.Change.State != "succeeded" {
		t.Fatalf("delete failed: %+v", result)
	}
	live, err := e.liveRawData(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := live["tcp.routers/database"]; ok {
		t.Fatal("deleted router still loaded")
	}
	retained, err := e.output(ctx, "docker", "exec", "cove-ingress", "ls", "/data/cove-custom-history/database.yaml")
	if err != nil || !strings.Contains(retained, ".deleted") {
		t.Fatalf("deleted file not retained: %q %v", retained, err)
	}

	// Certificates migrated into the proxy volume are adopted and stay editable.
	certificate, key := selfSignedCertificate(t, "legacy.example.test")
	if err = e.Run(ctx, e.Root, []string{"docker", "exec", "cove-ingress", "mkdir", "-p", "/data/migrated-certificates"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"legacy.crt": certificate, "legacy.key": key} {
		local := filepath.Join(t.TempDir(), name)
		if err = os.WriteFile(local, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "cp", local, "cove-ingress:/data/migrated-certificates/" + name}, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	migrated := "tls:\n  certificates:\n    - certFile: /data/migrated-certificates/legacy.crt\n      keyFile: /data/migrated-certificates/legacy.key\n"
	if err = os.WriteFile(staged, []byte(migrated), 0600); err != nil {
		t.Fatal(err)
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "cp", staged, "cove-ingress:/data/dynamic/migrated-certificates.yaml"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if result = change("import", "migrated-certificates.yaml", nil, contentRevision(migrated)); result.Change == nil || result.Change.State != "succeeded" || len(result.Change.Warnings) != 0 {
		t.Fatalf("migrated certificates not adopted cleanly: %+v", result.Change)
	}
	reviewed := "# Migrated certificate for legacy.example.test\n" + migrated
	if result = change("put", "migrated-certificates.yaml", text(reviewed), contentRevision(migrated)); result.Change == nil || result.Change.State != "succeeded" {
		t.Fatalf("valid certificate edit failed: %+v", result.Change)
	}
	assertRejected(t, change("put", "migrated-certificates.yaml", text(strings.ReplaceAll(reviewed, "legacy.key", "legacy.crt")), contentRevision(reviewed)), "TLS problem")
	assertRejected(t, change("put", "migrated-certificates.yaml", text(strings.Replace(reviewed, "/data/migrated-certificates/legacy.key", "/etc/hostname", 1)), contentRevision(reviewed)), "inside the proxy volume")

	// A crash after the journal and the new file reached disk restores the previous file.
	proxy := mustProxy(t, e)
	if err = e.proxyShell(ctx, proxy, io.Discard, `mkdir -p "/data/cove-custom-history/$1"; cp "/data/dynamic/$1" "/data/cove-custom-history/$1/crash.previous"; printf 'http: {}\n' > "/data/dynamic/$1"`, "legacy-import.yaml"); err != nil {
		t.Fatal(err)
	}
	saved, err := e.loadCustomIngress()
	if err != nil {
		t.Fatal(err)
	}
	saved.Change = &customIngressJournal{IngressConfigChange: IngressConfigChange{ID: "crash", Action: "put", Name: "legacy-import.yaml", State: "applying"}, Previous: editedRevision, Desired: contentRevision("http: {}\n"), Backup: "crash.previous"}
	if err = e.saveCustomIngress(saved); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := e.IngressConfig(ctx); blocked.Blocked == "" {
		t.Fatal("unsettled change did not block new changes")
	}
	if err = e.recoverCustomIngress(ctx); err != nil {
		t.Fatal(err)
	}
	if revision, err := e.proxyFileRevision(ctx, proxy, "legacy-import.yaml"); err != nil || revision != editedRevision {
		t.Fatalf("crash recovery did not restore the previous file: %s %v", revision, err)
	}
	if saved, _ = e.loadCustomIngress(); saved.Change.State != "interrupted" {
		t.Fatalf("recovery outcome not recorded: %+v", saved.Change)
	}

	after, err := e.output(ctx, "docker", "inspect", "--format", "{{.Id}} {{.State.StartedAt}}", "cove-ingress")
	if err != nil || after != started {
		t.Fatalf("proxy was recreated or restarted: %s -> %s", started, after)
	}
	if leftovers, _ := e.output(ctx, "docker", "ps", "-aq", "--filter", "label=cove.managed=ingress-check"); leftovers != "" {
		t.Fatalf("validation containers left behind: %s", leftovers)
	}
}

func selfSignedCertificate(t *testing.T, host string) ([]byte, []byte) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalECPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encodedKey})
}

func mustProxy(t *testing.T, e *Engine) proxyInfo {
	t.Helper()
	proxy, err := e.inspectProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return proxy
}

func waitForLive(t *testing.T, e *Engine, key string) {
	t.Helper()
	for attempt := 0; attempt < 30; attempt++ {
		if live, err := e.liveRawData(context.Background()); err == nil {
			if _, ok := live[key]; ok {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s was not loaded by the proxy", key)
}

func assertRejected(t *testing.T, result IngressConfigResult, want string) {
	t.Helper()
	if result.Change == nil || result.Change.State != "rejected" {
		t.Fatalf("change was not rejected: %+v", result)
	}
	t.Logf("rejected: %q", result.Change.Problems)
	if !slices.ContainsFunc(result.Change.Problems, func(problem string) bool { return strings.Contains(problem, want) }) {
		t.Fatalf("rejection does not mention %q: %v", want, result.Change.Problems)
	}
}
