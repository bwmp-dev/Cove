package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTrafficClonePreservesVolumesAndConfiguration(t *testing.T) {
	id := strings.Repeat("a", 64)
	inspect := map[string]any{"Id": id, "Image": "sha256:" + strings.Repeat("b", 64), "Config": map[string]any{"Image": "traefik:tag", "Cmd": []any{"-ec", `mkdir -p /data; exec traefik "$@"`, "traefik", "--entrypoints.web.address=:80"}, "Labels": map[string]any{"cove.managed": "ingress", "cove.panel.domain": "panel.example.test"}, "Env": []any{"SECRET_FILE=/data/secret"}}, "HostConfig": map[string]any{"NetworkMode": "cove-ingress", "PortBindings": map[string]any{"443/tcp": []any{map[string]string{"HostPort": "443"}}}}, "Mounts": []any{map[string]any{"Type": "volume", "Name": "original-anonymous-data", "Destination": "/data", "RW": true}}, "NetworkSettings": map[string]any{"Networks": map[string]any{"cove-ingress": map[string]any{"Aliases": []any{id, id[:12], "cove-ingress"}}}}}
	config, err := cloneTrafficProxy(inspect, TrafficSettings{Enabled: true, TrustedProxies: []string{"192.0.2.0/24"}}, "change")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(config)
	for _, want := range []string{"original-anonymous-data:/data:rw", "SECRET_FILE=/data/secret", "cove.panel.domain", "sha256:" + strings.Repeat("b", 64), "--accesslog.fields.defaultmode=drop", "max-size", "4m"} {
		if !strings.Contains(string(data), want) {
			t.Fatal("configuration was lost", want, string(data))
		}
	}
	if strings.Contains(string(data), `"Aliases":["`+id) {
		t.Fatal("old container ID retained as network alias")
	}
	inspect["HostConfig"].(map[string]any)["AutoRemove"] = true
	if _, err := cloneTrafficProxy(inspect, TrafficSettings{Enabled: true}, "change"); err == nil {
		t.Fatal("auto-remove proxy could lose rollback source")
	}
}

type dockerTestTransport func(*http.Request) (*http.Response, error)

func (f dockerTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCommittedTrafficRecoveryIsIdempotent(t *testing.T) {
	e := &Engine{Root: t.TempDir()}
	id := strings.Repeat("a", 64)
	nonce := strings.Repeat("b", 32)
	j := proxyTrafficJournal{OldID: id, BackupName: "cove-ingress-previous-" + nonce, Nonce: nonce, Committed: true}
	if err := writeJSON(filepath.Join(e.Root, "proxy-traffic-change.json"), j); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: dockerTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/containers/"+id+"/json" && r.URL.Path != "/containers/"+id {
			t.Fatal("unrelated Docker resource touched", r.URL)
		}
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"message":"gone"}`))}, nil
	})}
	if err := e.recoverProxyTraffic(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.Root, "proxy-traffic-change.json")); !os.IsNotExist(err) {
		t.Fatal("completed cleanup marker remained", err)
	}
}

func TestProxyTrafficDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_PROXY_TRAFFIC_DOCKER") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires Linux test binary in a disposable Docker-in-Docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			debug, _ := exec.CommandContext(ctx, "docker", "logs", "cove-traffic-app").CombinedOutput()
			t.Log(string(debug))
			debug, _ = exec.CommandContext(ctx, "docker", "inspect", "cove-traffic-app").CombinedOutput()
			t.Log(string(debug))
			t.Fatal(err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	if exec.CommandContext(ctx, "docker", "inspect", "cove-ingress").Run() == nil {
		t.Fatal("refusing to adopt an existing proxy; run this test only in disposable DinD")
	}
	run("pull", ingressImage)
	run("pull", "alpine:3.21")
	run("pull", "busybox:1.37")
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	if err = e.ensureIngress(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	run("start", "cove-ingress")
	if err = e.proxyRoutesReady(ctx, "cove-ingress", nil); err != nil {
		t.Fatal(err)
	}
	run("exec", "cove-ingress", "sh", "-ec", "printf preserved > /data/marker")
	run("run", "-d", "--name", "cove-traffic-app", "--label", "com.docker.compose.project=cove-app", "--label", "com.docker.compose.service=app", "--network", "cove-ingress", "--network-alias", routeAlias("app", "app"), "busybox:1.37", "sh", "-ec", "mkdir -p /www; printf okay > /www/index.html; exec httpd -f -p 8080 -h /www")
	if err = e.applyIngress(ctx, "app", []Route{{Domain: "app.example.test", Service: "app", Port: 8080}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	settings := TrafficSettings{Enabled: true, TrustedProxies: []string{}}
	_, plan, err := e.ConfigureTraffic(ctx, ProxyTrafficControl{Action: "preview", Settings: &settings})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := e.ConfigureTraffic(ctx, ProxyTrafficControl{Action: "apply", Settings: &settings, Revision: plan.Revision})
	if err != nil || !state.Configured {
		t.Fatal(state, err)
	}
	if err = e.ensureIngress(ctx, io.Discard); err != nil {
		t.Fatal("reconfigured proxy cannot accept future deployments", err)
	}
	if run("exec", "cove-ingress", "cat", "/data/marker") != "preserved" {
		t.Fatal("proxy data lost")
	}
	run("exec", "cove-ingress", "wget", "--no-check-certificate", "--header", "Host: app.example.test", "-qO-", "https://127.0.0.1/")
	report, err := e.CollectTraffic(ctx, time.Now().Add(-time.Minute))
	if err != nil || !report.Available || len(report.Entries) != 1 || report.Entries[0].Status != 200 || report.Entries[0].ClientIP == "" || report.Entries[0].Bytes == 0 {
		t.Fatal(report, err)
	}
	old := run("inspect", "--format", "{{.Id}}", "cove-ingress")
	nonce := strings.Repeat("e", 32)
	interrupted := proxyTrafficJournal{OldID: old, BackupName: "cove-ingress-previous-" + nonce, Nonce: nonce, Previous: settings}
	if err = writeJSON(filepath.Join(e.Root, "proxy-traffic-change.json"), interrupted); err != nil {
		t.Fatal(err)
	}
	run("stop", "cove-ingress")
	run("rename", "cove-ingress", interrupted.BackupName)
	restarted, err := NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.recoverTrafficOnStartup(ctx); err != nil {
		t.Fatal("startup did not restore interrupted rename", err)
	}
	if run("inspect", "--format", "{{.Id}}", "cove-ingress") != old {
		t.Fatal("startup changed the original proxy identity")
	}
	settings.TrustedProxies = []string{"127.0.0.1/32"}
	_, plan, err = e.ConfigureTraffic(ctx, ProxyTrafficControl{Action: "preview", Settings: &settings})
	if err != nil {
		t.Fatal(err)
	}
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		if len(args) > 2 && args[1] == "exec" && args[2] != old {
			return errors.New("replacement readiness failure")
		}
		return RunCommand(ctx, dir, args, env, out)
	}
	if _, _, err = e.ConfigureTraffic(ctx, ProxyTrafficControl{Action: "apply", Settings: &settings, Revision: plan.Revision}); err == nil {
		t.Fatal("failed readiness did not roll back")
	}
	e.Run = RunCommand
	if current := run("inspect", "--format", "{{.Id}}", "cove-ingress"); current != old {
		t.Fatal("previous proxy not restored", current, old)
	}
	if run("exec", "cove-ingress", "cat", "/data/marker") != "preserved" {
		t.Fatal("proxy volume changed during rollback")
	}
	if _, err := os.Stat(filepath.Join(e.Root, "proxy-traffic-change.json")); !os.IsNotExist(err) {
		t.Fatal("successful rollback remains pending", err)
	}
}
