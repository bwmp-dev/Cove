package worker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
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
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestManagedRegistryDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_MANAGED_REGISTRY_DOCKER") != "1" || runtime.GOOS != "linux" {
		t.Skip("owned isolated Docker fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	command := func(args ...string) string {
		t.Helper()
		value, err := e.output(ctx, args...)
		if err != nil {
			t.Fatal(args, value, err)
		}
		return strings.TrimSpace(value)
	}
	for _, kind := range []string{"container", "network", "volume"} {
		if _, err = e.output(ctx, "docker", kind, "inspect", "cove-ingress"); err == nil {
			t.Fatal("fixture daemon is not empty")
		}
	}
	if err = writeJSON(filepath.Join(e.Root, "ingress-mode.json"), migrationIngressMode{Staged: true, TLSChallenge: true}); err != nil {
		t.Fatal(err)
	}
	if err = e.ensureIngress(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err = e.syncIngress(ctx, map[string][]Route{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	registryID := strings.Repeat("a", 32)
	host := "registry.example.test"
	password := "generated-local-registry-fixture-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		t.Fatal(err)
	}
	request, err := (ManagedRegistryConfig{ID: registryID, Hostname: host, Username: "cove_registry", Hash: string(hash)}).Deployment("registry-provision")
	if err != nil {
		t.Fatal(err)
	}
	cert, key := selfSignedCertificate(t, host)
	certificateID := strings.Repeat("b", 32)
	request.Routes[0].Policy = &RoutePolicy{CertificateSource: "uploaded", CertificateID: certificateID}
	request.Certificates = []RouteCertificate{{ID: certificateID, CertificatePEM: string(cert), PrivateKeyPEM: string(key)}}
	run := func(r Request) Operation {
		t.Helper()
		if _, err = e.Submit(r); err != nil {
			t.Fatal(err)
		}
		var jobs sync.WaitGroup
		if err = e.dispatch(ctx, &jobs); err != nil {
			t.Fatal(err)
		}
		jobs.Wait()
		op, err := e.Status(r.ID)
		if err != nil || op.State != "succeeded" {
			log, _ := e.Logs(r.ID)
			ids, _ := e.output(ctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+r.Workload)
			for _, id := range strings.Fields(ids) {
				diagnostic, _ := e.output(ctx, "docker", "logs", "--tail", "30", id)
				diagnostic = strings.ReplaceAll(diagnostic, password, "[redacted]")
				diagnostic = strings.ReplaceAll(diagnostic, string(hash), "[redacted]")
				t.Log(diagnostic)
			}
			t.Fatal(op, err, log)
		}
		log, _ := e.Logs(r.ID)
		if strings.Contains(log, password) || strings.Contains(log, string(hash)) {
			t.Fatal("registry credential in operation log")
		}
		return op
	}
	run(request)
	proxy := command("docker", "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", "cove-ingress")
	file, err := os.OpenFile("/etc/hosts", os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("\n" + proxy + " " + host + "\n")
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	trust := filepath.Join("/etc/docker/certs.d", host)
	if err = os.MkdirAll(trust, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(trust, "ca.crt"), cert, 0600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(cert)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: host}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(proxy, "443"))
	}}}
	for attempt := 0; attempt < 30; attempt++ {
		response, err := client.Get("https://" + host + "/v2/")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode != 401 {
				t.Fatal("unauthenticated registry accepted", response.StatusCode)
			}
			break
		}
		if attempt == 29 {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
	}
	registry := &Registry{Server: host, Username: "cove_registry", Password: password}
	if err = e.TestRegistry(ctx, registry); err != nil {
		t.Fatal("generated native credentials or verified TLS failed", err)
	}
	empty := run(Request{ID: "registry-empty-gc", Workload: registryID, Action: "registry_maintenance", Kind: "service", ManagedRegistry: &ManagedRegistryOperation{Mode: "gc", Policy: ManagedRegistryRetention{KeepPerRepository: 1, MinimumAgeDays: 7}}})
	if empty.ManagedRegistry == nil || empty.ManagedRegistry.Manifests != 0 || empty.ManagedRegistry.Deleted != 0 {
		t.Fatal("empty registry maintenance invented deleted content", empty)
	}
	auth, err := registryConfig(e.Root, registry)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(auth)
	seed := command("docker", "create", "--name", "registry-seed", "--network", "none", "--read-only", "--cap-drop", "ALL", "busybox:1.37", "true")
	var rootfs bytes.Buffer
	if err = e.Run(ctx, e.Root, []string{"docker", "export", seed}, nil, &rootfs); err != nil {
		t.Fatal(err)
	}
	command("docker", "rm", seed)
	push := func(tag, marker string, age int) string {
		t.Helper()
		archive := registryFixtureImageArchive(t, rootfs.Bytes(), host+"/team/app:"+tag, marker, age)
		name := filepath.Join(e.Root, tag+".tar")
		if err = os.WriteFile(name, archive, 0600); err != nil {
			t.Fatal(err)
		}
		command("docker", "load", "-i", name)
		if err = e.Run(ctx, e.Root, []string{"docker", "--config", auth, "push", host + "/team/app:" + tag}, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
		var refs []string
		if err = e.imageJSON(ctx, nil, &refs, "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", host+"/team/app:"+tag); err != nil || len(refs) != 1 {
			t.Fatal(refs, err)
		}
		_, digest, _ := strings.Cut(refs[0], "@")
		volume := command("docker", "volume", "inspect", "--format", "{{.Mountpoint}}", "cove-"+registryID+"_data")
		aged := time.Now().UTC().Add(-time.Duration(age) * 24 * time.Hour)
		for _, relative := range []string{"revisions/sha256/" + strings.TrimPrefix(digest, "sha256:") + "/link", "tags/" + tag + "/current/link"} {
			if err = os.Chtimes(filepath.Join(volume, "docker/registry/v2/repositories/team/app/_manifests", relative), aged, aged); err != nil {
				t.Fatal(err)
			}
		}
		return refs[0]
	}
	protected := push(registryBuildFixtureTag("1"), "protected", 30)
	expired := push(registryBuildFixtureTag("2"), "expired", 40)
	_ = push(registryBuildFixtureTag("3"), "newest", 10)
	app := strings.Repeat("c", 32)
	compose := "services:\n  app:\n    image: " + protected + "\n    pull_policy: always\n"
	run(Request{ID: "registry-consumer", Workload: app, Action: "deploy", Kind: "service", Compose: compose, Registry: registry})
	consumerID := command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+app)
	consumerStarted := command("docker", "inspect", "--format", "{{.State.StartedAt}}", consumerID)
	refs, err := e.ManagedRegistryReferences(ctx, app, host, nil)
	if err != nil || len(refs) != 1 || host+"/"+refs[0] != protected {
		t.Fatal("retained deployment inventory missing", refs, err)
	}
	before := command("docker", "exec", command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+registryID), "du", "-sk", "/var/lib/registry")
	op := run(Request{ID: "registry-gc", Workload: registryID, Action: "registry_maintenance", Kind: "service", ManagedRegistry: &ManagedRegistryOperation{Mode: "gc", Policy: ManagedRegistryRetention{KeepPerRepository: 1, MinimumAgeDays: 7}, Protected: refs}})
	if op.ManagedRegistry == nil || op.ManagedRegistry.Deleted != 1 {
		t.Fatal("retention did not delete exactly the unreferenced old image", op)
	}
	if err = e.TestRegistry(ctx, registry); err != nil {
		t.Fatal("registry auth not restored after maintenance", err)
	}
	check := func(ref string, want int) {
		t.Helper()
		repo, digest, _ := strings.Cut(strings.TrimPrefix(ref, host+"/"), "@")
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://"+host+"/v2/"+repo+"/manifests/"+digest, nil)
		req.SetBasicAuth(registry.Username, registry.Password)
		req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != want {
			t.Fatal("manifest retention mismatch", want, response.StatusCode)
		}
	}
	check(protected, 200)
	check(expired, 404)
	after := command("docker", "exec", command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+registryID), "du", "-sk", "/var/lib/registry")
	if before == after {
		t.Fatal("garbage collection did not reclaim storage")
	}
	for _, boundary := range []string{"stop", "inventory", "gc", "cancel"} {
		r := Request{ID: "registry-fault-" + boundary, Workload: registryID, Action: "registry_maintenance", Kind: "service", ManagedRegistry: &ManagedRegistryOperation{Mode: "gc", Policy: ManagedRegistryRetention{KeepPerRepository: 1, MinimumAgeDays: 7}, Protected: refs}}
		base := e.Run
		injected := false
		e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
			if injected && boundary != "cancel" && args[1] == "start" {
				return errors.New("fixture worker lost before restart confirmation")
			}
			err := base(ctx, dir, args, env, out)
			if err != nil {
				return err
			}
			match := (boundary == "stop" || boundary == "cancel") && args[1] == "stop" || (boundary == "inventory" || boundary == "gc") && args[1] == "run" && strings.Contains(strings.Join(args, " "), registryMaintenanceName(r, boundary))
			if !injected && match {
				injected = true
				if boundary == "cancel" {
					_, err := e.Cancel(r)
					if err != nil {
						return err
					}
					return context.Canceled
				}
				return errors.New("fixture lost acknowledgement at registry maintenance boundary")
			}
			return nil
		}
		if _, err = e.Submit(r); err != nil {
			t.Fatal(err)
		}
		var jobs sync.WaitGroup
		if err = e.dispatch(ctx, &jobs); err != nil {
			t.Fatal(err)
		}
		jobs.Wait()
		e.Run = base
		failed, err := e.Status(r.ID)
		if err != nil || !injected {
			t.Fatal("fault boundary was not exercised", boundary, failed, err)
		}
		if boundary == "cancel" {
			if failed.State != "cancelled" || failed.RegistryRecoveryPending {
				t.Fatal("cancellation did not restore registry", failed)
			}
		} else {
			if failed.State != "failed" || !failed.RegistryRecoveryPending {
				t.Fatal("lost acknowledgement discarded cleanup ownership", boundary, failed)
			}
			recovered, err := NewEngine(e.Root)
			if err != nil {
				t.Fatal(err)
			}
			if err = recovered.Recover(); err != nil {
				t.Fatal("new worker could not recover durable maintenance checkpoint", boundary, err)
			}
			e = recovered
			e.ACMEDirectory = "https://127.0.0.1:9/directory"
			status, err := e.Status(r.ID)
			if err != nil || status.RegistryRecoveryPending {
				t.Fatal("recovered worker still has pending registry restoration", status, err)
			}
		}
		if err = e.TestRegistry(ctx, registry); err != nil {
			t.Fatal("recovery lost authenticated TLS service", boundary, err)
		}
		check(protected, 200)
	}
	remaining, err := e.Containers(ctx, app)
	if err != nil || len(remaining) != 1 || remaining[0].State != "running" {
		t.Fatal("registry maintenance changed consumer", remaining, err)
	}
	if command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+app) != consumerID || command("docker", "inspect", "--format", "{{.State.StartedAt}}", consumerID) != consumerStarted {
		t.Fatal("registry recovery restarted the consumer")
	}
	remove := run(Request{ID: "registry-remove", Workload: registryID, Action: "registry_maintenance", Kind: "service", ManagedRegistry: &ManagedRegistryOperation{Mode: "remove", Policy: ManagedRegistryRetention{KeepPerRepository: 1, MinimumAgeDays: 7}, Protected: refs}})
	if remove.ManagedRegistry == nil || !remove.ManagedRegistry.Removed || !remove.ManagedRegistry.DataRetained {
		t.Fatal(remove)
	}
	if command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+registryID) != "" {
		t.Fatal("registry container left behind")
	}
	if err = e.ownedRegistryVolume(ctx, registryID); err != nil {
		t.Fatal("retained registry data lost", err)
	}
	run(Request{ID: "registry-purge", Workload: registryID, Action: "registry_maintenance", Kind: "service", ManagedRegistry: &ManagedRegistryOperation{Mode: "remove", Policy: ManagedRegistryRetention{KeepPerRepository: 1, MinimumAgeDays: 7}, DeleteData: true}})
	if command("docker", "volume", "ls", "-q", "--filter", "name=^cove-"+registryID+"_data$") != "" {
		t.Fatal("explicitly purged registry volume left behind")
	}
}

func registryFixtureImageArchive(t *testing.T, rootfs []byte, tag, marker string, age int) []byte {
	t.Helper()
	layerSum := sha256.Sum256(rootfs)
	config, _ := json.Marshal(map[string]any{"created": time.Now().UTC().Add(-time.Duration(age) * 24 * time.Hour), "architecture": runtime.GOARCH, "os": "linux", "config": map[string]any{"Cmd": []string{"sh", "-ec", "echo " + marker + "; exec sleep 86400"}}, "rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + hex.EncodeToString(layerSum[:])}}})
	configSum := sha256.Sum256(config)
	configName := hex.EncodeToString(configSum[:]) + ".json"
	manifest, _ := json.Marshal([]map[string]any{{"Config": configName, "RepoTags": []string{tag}, "Layers": []string{"layer.tar"}}})
	var output bytes.Buffer
	archive := tar.NewWriter(&output)
	for _, entry := range []struct {
		name string
		data []byte
	}{{configName, config}, {"layer.tar", rootfs}, {"manifest.json", manifest}} {
		if err := archive.WriteHeader(&tar.Header{Name: entry.name, Mode: 0600, Size: int64(len(entry.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
