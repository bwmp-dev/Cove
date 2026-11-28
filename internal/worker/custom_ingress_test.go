package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestCustomIngressInputBoundary(t *testing.T) {
	for _, input := range []string{
		"{}", "", "http:\n  middlewares:\n    compression:\n      compress: {}",
		"tcp:\n  routers:\n    database:\n      rule: HostSNI(`*`)\n      service: database",
		"tls:\n  certificates:\n    - certFile: /data/certs/site.pem\n      keyFile: /data/certs/site.key",
		"udp:\n  routers:\n    dns:\n      entryPoints: [dns]\n      service: dns",
	} {
		if _, err := parseCustomIngress(input); err != nil {
			t.Errorf("valid dynamic configuration rejected: %v", err)
		}
	}
	for _, input := range []string{
		"entryPoints: {}", "providers: {}", "certificatesResolvers: {}", "http: []", "http: [", "{}\n---\n{}",
		"http: {}\nhttp: {}", "http:\n  routerz: {}", "http:\n  routers:\n    bad@name: {}", "http:\n  routers:\n    plain: text",
		"http:\n  routers:\n    template:\n      rule: '{{ env SECRET }}'", "# {{ comments are rendered too }}\n{}",
		"http: {}\x00", "\xff", strings.Repeat(" ", MaxCustomIngressBytes+1),
	} {
		if _, err := parseCustomIngress(input); err == nil {
			t.Errorf("invalid configuration accepted: %q", input)
		}
	}
}

func TestRuleHosts(t *testing.T) {
	claims := ruleHosts("Host(`a.example.com`) || HostSNI(\"b.example.com\") && !Host(`c.example.com`) && HostRegexp(`^(x|y)\\.example\\.com$`) && MyHost(`ignored`)")
	want := []hostClaim{{"Host", "a.example.com", false}, {"HostSNI", "b.example.com", false}, {"Host", "c.example.com", true}, {"HostRegexp", `^(x|y)\.example\.com$`, false}}
	if !slices.Equal(claims, want) {
		t.Fatalf("rule hosts = %+v", claims)
	}
}

func TestCustomIngressIssues(t *testing.T) {
	context := customIngressContext{
		EntryPoints: map[string]bool{"web": true, "websecure": true, "postgres": true},
		Resolvers:   map[string]bool{"cove": true},
		Domains:     []string{"app.example.com", "panel.example.com"},
		Others:      map[string]string{"http.middlewares/shared": "other.yaml"},
	}
	config, err := parseCustomIngress("http:\n  routers:\n" +
		"    exact: {rule: \"Host(`APP.example.com`)\", service: s}\n" +
		"    pattern: {rule: \"HostRegexp(`^[a-z]+\\\\.example\\\\.com$`)\", entryPoints: [websecure], service: s}\n" +
		"    excluded: {rule: \"Host(`ok.example.com`) && !Host(`app.example.com`)\", service: s}\n" +
		"    pathonly: {rule: \"PathPrefix(`/`)\", entryPoints: [web], service: s}\n" +
		"    resolver: {rule: \"Host(`ok.example.com`)\", service: s, tls: {certResolver: letsencrypt}}\n" +
		"    elsewhere: {rule: \"Host(`ok.example.com`)\", entryPoints: [metrics], service: s}\n" +
		"    cove-owned: {rule: \"Host(`ok.example.com`)\", service: s}\n" +
		"  middlewares:\n    shared: {compress: {}}\n" +
		"tls:\n  certificates:\n" +
		"    - {certFile: /data/migrated-certificates/site.crt, keyFile: /data/migrated-certificates/site.key}\n" +
		"    - {certFile: /etc/ssl/site.crt, keyFile: /data/../etc/ssl/site.key}\n" +
		"    - {certFile: /data/traefik-acme.json, keyFile: relative.key}\n" +
		"tcp:\n  routers:\n" +
		"    database: {rule: \"HostSNI(`*`)\", entryPoints: [postgres], service: db}\n" +
		"    everything: {rule: \"HostSNI(`*`)\", entryPoints: [websecure], service: db}\n")
	if err != nil {
		t.Fatal(err)
	}
	issues, warnings := customIngressIssues(config, context)
	keys := map[string]bool{}
	for _, issue := range issues {
		keys[issue.Key] = true
	}
	for _, key := range []string{"host/app.example.com", "host/panel.example.com", "resolver/letsencrypt", "entrypoint/metrics", "reserved/http.routers/cove-owned", "duplicate/http.middlewares/shared", "catchall",
		"certificate//etc/ssl/site.crt", "certificate//data/../etc/ssl/site.key", "certificate//data/traefik-acme.json", "certificate/relative.key"} {
		if !keys[key] {
			t.Errorf("missing issue %s in %+v", key, issues)
		}
	}
	if len(issues) != 12 {
		t.Errorf("unexpected issues: %+v", issues)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "pathonly") {
		t.Errorf("missing host warning: %v", warnings)
	}
	context.Resolvers = nil
	if issues, _ = customIngressIssues(config, context); slices.ContainsFunc(issues, func(issue ingressIssue) bool { return strings.HasPrefix(issue.Key, "resolver/") }) {
		t.Error("resolvers were checked although the proxy's resolvers are unknown")
	}
}

func TestValidationProblemsIgnoreExistingErrorsAndRedactSecrets(t *testing.T) {
	candidate, err := parseCustomIngress("http:\n  routers:\n    broken: {rule: \"Host(`a.example.com`)\", service: s, middlewares: [gone]}\n    fresh: {rule: \"Host(`b.example.com`)\", service: s}\n    missing: {rule: \"Host(`c.example.com`)\", service: s}\n  middlewares:\n    auth: {basicAuth: {users: [\"admin:$apr1$secrethash\"]}}\ntls:\n  options:\n    modern: {minVersion: VersionTLS13}\n")
	if err != nil {
		t.Fatal(err)
	}
	other, err := parseCustomIngress("http:\n  routers:\n    user: {rule: \"Host(`d.example.com`)\", service: s, middlewares: [auth]}\n")
	if err != nil {
		t.Fatal(err)
	}
	existing := []string{`middleware "gone@file" does not exist`}
	v := validation{Loaded: true, Objects: map[string]rawObject{
		"http.routers/broken":    {Status: "disabled", Errors: existing},
		"http.routers/fresh":     {Status: "disabled", Errors: []string{"unknown TLS options"}},
		"http.middlewares/auth":  {Status: "enabled"},
		"http.routers/user":      {Status: "disabled", Errors: []string{`middleware "auth@file" does not exist`}},
		"http.services/whatever": {Status: "enabled"},
	}, Logs: []map[string]any{
		{"level": "error", "message": "Unable to append certificate admin:$apr1$secrethash"},
		{"level": "error", "resolver": "cove", "message": "ACME challenge failed"},
		{"level": "error", "routerName": "broken@file", "error": "already reported in raw data"},
	}}
	live := map[string]rawObject{"http.routers/broken": {Status: "disabled", Errors: existing}, "http.routers/user": {Status: "enabled"}}
	problems := validationProblems(v, "candidate.yaml", candidate, map[string]customIngress{"b.yaml": other}, live, candidate.Secrets)
	joined := strings.Join(problems, "\n")
	for _, want := range []string{`router "fresh": unknown TLS options`, `router "missing" was not loaded`, `router "user" in b.yaml`, "TLS problem: Unable to append certificate [redacted]"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "gone@file") || strings.Contains(joined, "secrethash") || strings.Contains(joined, "ACME") || len(problems) != 4 {
		t.Errorf("unexpected problems:\n%s", joined)
	}
	if problems = validationProblems(validation{Logs: []map[string]any{{"level": "error", "providerName": "file", "error": "collecting file configs: /tmp/cove-check/candidate.yaml: field not found, node: rulee"}}}, "candidate.yaml", candidate, nil, live, nil); len(problems) != 1 || !strings.Contains(problems[0], "candidate.yaml: field not found, node: rulee") || strings.Contains(problems[0], "/tmp/") {
		t.Errorf("load failure not reported: %v", problems)
	}
}

// proxyFake stands in for Docker and the cove-ingress proxy. Volume scripts run in a real shell
// against a temporary directory, and both the live and the validation proxy report the objects
// their files define, with missing middleware references disabled like Traefik reports them.
type proxyFake struct {
	t       *testing.T
	volume  string
	running bool
	exists  bool
	// stale freezes what the live proxy reports, as a proxy that ignores file changes would.
	stale        []byte
	logs         []string
	staticConfig string
	fail         func(command string) error
	mu           sync.Mutex
	commands     []string
	validators   map[string]*fakeValidator
}

type fakeValidator struct {
	target string
	input  string
}

const routesYAML = "{\"http\":{\"routers\":{\"cove-route-1\":{\"rule\":\"Host(`app.example.test`)\",\"entryPoints\":[\"websecure\"],\"service\":\"cove-route-1\",\"tls\":{\"certResolver\":\"cove\"}}},\"services\":{\"cove-route-1\":{\"loadBalancer\":{\"servers\":[{\"url\":\"http://cove-1:80\"}]}}}}}"

func customEngine(t *testing.T) (*Engine, *proxyFake) {
	t.Helper()
	for _, tool := range []string{"sh", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " is required to run proxy volume scripts")
		}
	}
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &proxyFake{t: t, volume: t.TempDir(), running: true, exists: true, validators: map[string]*fakeValidator{}}
	if err = os.MkdirAll(filepath.Join(f.volume, "dynamic"), 0755); err != nil {
		t.Fatal(err)
	}
	f.write("routes.yaml", routesYAML)
	if err = writeJSON(filepath.Join(e.Root, "ingress.json"), map[string][]Route{"app": {{Domain: "app.example.test", Service: "web", Port: 80}}}); err != nil {
		t.Fatal(err)
	}
	e.Run = f.run
	confirm, settleDelay := customConfirmTimeout, validationSettle
	customConfirmTimeout, validationSettle = 50*time.Millisecond, 0
	t.Cleanup(func() { customConfirmTimeout, validationSettle = confirm, settleDelay })
	return e, f
}

func (f *proxyFake) path(name string) string {
	return filepath.Join(f.volume, filepath.FromSlash(strings.TrimPrefix(name, "/data/")))
}

func (f *proxyFake) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.volume, "dynamic", name), []byte(content), 0644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *proxyFake) read(name string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(f.volume, "dynamic", name))
	return string(data), err == nil
}

func (f *proxyFake) history(name string) map[string]string {
	entries, _ := os.ReadDir(filepath.Join(f.volume, "cove-custom-history", name))
	files := map[string]string{}
	for _, entry := range entries {
		data, _ := os.ReadFile(filepath.Join(f.volume, "cove-custom-history", name, entry.Name()))
		files[entry.Name()] = string(data)
	}
	return files
}

func (f *proxyFake) set(update func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	update()
}

func (f *proxyFake) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.commands)
}

func (f *proxyFake) shell(script string, args []string, out io.Writer) error {
	script = strings.ReplaceAll(script, "/data/", filepath.ToSlash(f.volume)+"/")
	cmd := exec.Command("sh", append([]string{"-ec", script, "sh"}, args...)...)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

func (f *proxyFake) dynamic(exclude string) map[string]string {
	files := map[string]string{}
	entries, _ := os.ReadDir(filepath.Join(f.volume, "dynamic"))
	for _, entry := range entries {
		if entry.Name() != exclude && !entry.IsDir() {
			data, _ := os.ReadFile(filepath.Join(f.volume, "dynamic", entry.Name()))
			files[entry.Name()] = string(data)
		}
	}
	return files
}

func fakeRawData(files map[string]string) []byte {
	type entry struct {
		section, name string
		definition    map[string]any
	}
	var entries []entry
	defined := map[string]bool{}
	for _, content := range files {
		var config map[string]any
		if yaml.Unmarshal([]byte(content), &config) != nil {
			// Like Traefik's file provider, one unreadable file fails the whole directory.
			return []byte(`{"routers":{}}`)
		}
		for kind, section := range rawSections {
			parts := strings.SplitN(kind, ".", 2)
			group, _ := asMap(lookup(config, parts[0]))
			objects, _ := asMap(lookup(group, parts[1]))
			for name, definition := range objects {
				body, _ := asMap(definition)
				entries = append(entries, entry{section, name, body})
				defined[section+"/"+name] = true
			}
		}
	}
	raw := map[string]map[string]any{}
	for _, item := range entries {
		object := map[string]any{"status": "enabled", "usedBy": []string{"runtime"}}
		for key, value := range item.definition {
			object[key] = value
		}
		if middlewares, ok := object["middlewares"].([]any); ok && item.section == "routers" {
			for _, middleware := range middlewares {
				if name, _ := middleware.(string); !defined["middlewares/"+name] {
					object["status"], object["error"] = "disabled", []string{fmt.Sprintf("middleware %q does not exist", name+"@file")}
				}
			}
		}
		if raw[item.section] == nil {
			raw[item.section] = map[string]any{}
		}
		raw[item.section][item.name+"@file"] = object
	}
	data, _ := json.Marshal(raw)
	return data
}

func (f *proxyFake) run(_ context.Context, _ string, args, _ []string, out io.Writer) error {
	command := strings.Join(args, " ")
	f.mu.Lock()
	f.commands = append(f.commands, command)
	fail, running, exists, stale := f.fail, f.running, f.exists, f.stale
	f.mu.Unlock()
	if fail != nil {
		if err := fail(command); err != nil {
			return err
		}
	}
	write := func(text string) error { _, err := io.WriteString(out, text); return err }
	switch {
	case args[1] == "inspect" && args[2] == "--type":
		if !exists {
			_ = write("Error: No such object: cove-ingress")
			return errors.New("exit status 1")
		}
		proxyArgs := []string{"-ec", "mkdir -p /data/dynamic; exec traefik \"$@\"", "traefik", "--entrypoints.web.address=:80", "--certificatesresolvers.cove.acme.storage=/data/traefik-acme.json"}
		if f.staticConfig != "" {
			proxyArgs = []string{"-ec", "exec traefik --configFile=/etc/traefik/traefik.yaml"}
		}
		return json.NewEncoder(out).Encode(map[string]any{"labels": map[string]string{"cove.managed": "ingress", "cove.panel.domain": "panel.example.test"}, "running": running, "image": "traefik:v3.7.19", "args": proxyArgs, "ports": map[string]any{"443/tcp": []map[string]string{{"HostIp": "", "HostPort": "443"}}}})
	case args[1] == "exec" && args[2] == "cove-ingress" && args[3] == "sh":
		if !running {
			return errors.New("container is not running")
		}
		return f.shell(args[5], args[7:], out)
	case args[1] == "run":
		index := slices.Index(args, "-ec")
		return f.shell(args[index+1], args[index+3:], out)
	case args[1] == "exec" && args[2] == "cove-ingress" && args[3] == "wget":
		if !running {
			return errors.New("container is not running")
		}
		switch strings.TrimPrefix(args[5], "http://127.0.0.1:8080") {
		case "/api/entrypoints":
			return write(`[{"name":"traefik","address":"127.0.0.1:8080"},{"name":"web","address":":80"},{"name":"websecure","address":":443"}]`)
		case "/api/rawdata":
			if stale != nil {
				_, err := out.Write(stale)
				return err
			}
			_, err := out.Write(fakeRawData(f.dynamic("")))
			return err
		}
	case args[1] == "exec" && args[2] == "cove-ingress" && args[3] == "cat":
		return write(f.staticConfig)
	case args[1] == "cp":
		container, target, _ := strings.Cut(args[3], ":")
		if container == "cove-ingress" {
			data, err := os.ReadFile(args[2])
			if err != nil {
				return err
			}
			return os.WriteFile(f.path(target), data, 0600)
		}
		input := f.t.TempDir()
		entries, err := os.ReadDir(args[2])
		if err != nil {
			return err
		}
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(args[2], entry.Name()))
			if err != nil {
				return err
			}
			if err = os.WriteFile(filepath.Join(input, entry.Name()), data, 0600); err != nil {
				return err
			}
		}
		f.mu.Lock()
		f.validators[container].input = input
		f.mu.Unlock()
		return nil
	case args[1] == "create":
		index := slices.Index(args, "-ec")
		f.mu.Lock()
		f.validators[args[3]] = &fakeValidator{target: args[index+3]}
		f.mu.Unlock()
		return nil
	case args[1] == "start" || (args[1] == "inspect" && strings.HasPrefix(args[4], "cove-ingress-check-")):
		return write("true")
	case args[1] == "exec" && strings.HasPrefix(args[2], "cove-ingress-check-"):
		f.mu.Lock()
		validator := f.validators[args[2]]
		f.mu.Unlock()
		if args[3] == "sh" {
			for _, path := range args[7:] {
				if _, err := os.Stat(f.path(path)); err != nil {
					_ = write(path + "\n")
				}
			}
			return nil
		}
		files := f.dynamic(validator.target)
		if data, err := os.ReadFile(filepath.Join(validator.input, "candidate")); err == nil {
			files[validator.target] = string(data)
		}
		sentinel, _ := os.ReadFile(filepath.Join(validator.input, "sentinel.yaml"))
		files["cove-check-sentinel.yaml"] = string(sentinel)
		_, err := out.Write(fakeRawData(files))
		return err
	case args[1] == "logs":
		return write(strings.Join(f.logs, "\n"))
	case args[1] == "rm":
		f.mu.Lock()
		delete(f.validators, args[3])
		f.mu.Unlock()
		return nil
	case args[1] == "ps":
		f.mu.Lock()
		defer f.mu.Unlock()
		for name := range f.validators {
			_ = write(name + "\n")
		}
		return nil
	}
	f.t.Fatalf("unexpected command: %s", command)
	return nil
}

func lifecycleCommands(commands []string) []string {
	var found []string
	for _, command := range commands {
		if command == "docker start cove-ingress" || command == "docker rm -f cove-ingress" || strings.HasPrefix(command, "docker create --name cove-ingress ") {
			found = append(found, command)
		}
		for _, prefix := range []string{"docker restart", "docker stop", "docker compose", "docker kill"} {
			if strings.HasPrefix(command, prefix) {
				found = append(found, command)
			}
		}
	}
	return found
}

var changeCounter int

func put(e *Engine, name, content, revision string) IngressConfigResult {
	changeCounter++
	return e.ChangeIngressConfig(context.Background(), IngressConfigRequest{Action: "put", ChangeID: fmt.Sprintf("change-%d", changeCounter), Name: name, Content: &content, Revision: revision})
}

func change(e *Engine, action, name, revision string) IngressConfigResult {
	changeCounter++
	return e.ChangeIngressConfig(context.Background(), IngressConfigRequest{Action: action, ChangeID: fmt.Sprintf("change-%d", changeCounter), Name: name, Revision: revision})
}

func succeeded(t *testing.T, result IngressConfigResult) *IngressConfigChange {
	t.Helper()
	if result.Change == nil || result.Change.State != "succeeded" {
		t.Fatalf("change did not succeed: %+v %+v", result, result.Change)
	}
	return result.Change
}

func rejected(t *testing.T, result IngressConfigResult, want string) {
	t.Helper()
	if result.Change == nil || result.Change.State != "rejected" {
		t.Fatalf("change was not rejected (expected %q): %+v %+v", want, result, result.Change)
	}
	if !slices.ContainsFunc(result.Change.Problems, func(problem string) bool { return strings.Contains(problem, want) }) {
		t.Fatalf("problems do not mention %q: %v", want, result.Change.Problems)
	}
}

const extraConfig = "# keep this comment\nhttp:\n  routers:\n    extra:\n      rule: Host(`extra.example.test`)\n      entryPoints: [websecure]\n      service: extra\n      middlewares: [compress]\n  middlewares:\n    compress:\n      compress: {}\n  services:\n    extra:\n      loadBalancer:\n        servers:\n          - url: http://10.0.0.5:8080\n"

func TestCustomIngressLifecycleKeepsProxyRunning(t *testing.T) {
	e, f := customEngine(t)
	first := succeeded(t, put(e, "extra.yaml", extraConfig, ""))
	if content, _ := f.read("extra.yaml"); content != extraConfig || first.Revision != contentRevision(extraConfig) {
		t.Fatalf("file not written exactly: %q", content)
	}
	state, err := e.loadCustomIngress()
	if err != nil || !slices.Equal(state.Managed["extra.yaml"].Hosts, []string{"extra.example.test"}) {
		t.Fatalf("managed record missing hosts: %+v %v", state.Managed, err)
	}

	// A retried request returns the recorded outcome without changing the proxy again.
	copies := len(slices.DeleteFunc(f.recorded(), func(command string) bool { return !strings.HasPrefix(command, "docker cp") }))
	content := extraConfig
	retry := e.ChangeIngressConfig(context.Background(), IngressConfigRequest{Action: "put", ChangeID: first.ID, Name: "extra.yaml", Content: &content})
	if retry.Change == nil || retry.Change.State != "succeeded" || len(slices.DeleteFunc(f.recorded(), func(command string) bool { return !strings.HasPrefix(command, "docker cp") })) != copies {
		t.Fatalf("retry was not idempotent: %+v", retry)
	}
	other := content + "\n"
	if result := e.ChangeIngressConfig(context.Background(), IngressConfigRequest{Action: "put", ChangeID: first.ID, Name: "extra.yaml", Content: &other}); result.Code != "invalid" {
		t.Fatalf("reused change ID accepted different input: %+v", result)
	}
	if result := put(e, "extra.yaml", extraConfig, ""); result.Code != "conflict" {
		t.Fatalf("stale revision accepted: %+v", result)
	}

	edited := strings.Replace(extraConfig, "10.0.0.5", "10.0.0.6", 1)
	second := succeeded(t, put(e, "extra.yaml", edited, first.Revision))
	if content, _ := f.read("extra.yaml"); content != edited {
		t.Fatal("edit not written")
	}
	if f.history("extra.yaml")[second.ID+".previous"] != extraConfig {
		t.Fatalf("previous version not retained: %v", f.history("extra.yaml"))
	}

	removed := succeeded(t, change(e, "delete", "extra.yaml", second.Revision))
	if _, ok := f.read("extra.yaml"); ok {
		t.Fatal("deleted file still in the dynamic directory")
	}
	if f.history("extra.yaml")[removed.ID+".deleted"] != edited {
		t.Fatal("deleted file was not retained in the proxy volume")
	}
	if state, _ = e.loadCustomIngress(); len(state.Managed) != 0 {
		t.Fatalf("deleted file still managed: %+v", state.Managed)
	}
	if found := lifecycleCommands(f.recorded()); len(found) != 0 {
		t.Fatalf("custom configuration ran lifecycle commands: %v", found)
	}
	if len(f.validators) != 0 {
		t.Fatalf("validation containers left behind: %v", f.validators)
	}
}

func TestCustomIngressRejectionsLeaveProxyUnchanged(t *testing.T) {
	e, f := customEngine(t)
	succeeded(t, put(e, "shared.yaml", "http:\n  middlewares:\n    shared-auth:\n      basicAuth:\n        users: [\"admin:$apr1$abc$secretvalue\"]\n", ""))
	succeeded(t, put(e, "user.yaml", "http:\n  routers:\n    user:\n      rule: Host(`user.example.test`)\n      entryPoints: [websecure]\n      service: user\n      middlewares: [shared-auth]\n  services:\n    user:\n      loadBalancer:\n        servers:\n          - url: http://10.0.0.7\n", ""))
	before := f.dynamic("")

	rejected(t, put(e, "new.yaml", strings.Replace(extraConfig, "[compress]", "[missing]", 1), ""), `middleware "missing@file" does not exist`)
	rejected(t, put(e, "new.yaml", strings.Replace(extraConfig, "extra.example.test", "app.example.test", 1), ""), "app.example.test")
	rejected(t, put(e, "new.yaml", strings.Replace(extraConfig, "extra.example.test", "panel.example.test", 1), ""), "dashboard")
	rejected(t, put(e, "new.yaml", strings.Replace(extraConfig, "[websecure]", "[metrics]", 1), ""), "static configuration")
	rejected(t, put(e, "new.yaml", strings.Replace(extraConfig, "    extra:\n      rule", "    cove-extra:\n      rule", 1), ""), "reserved")
	rejected(t, put(e, "new.yaml", strings.Replace(extraConfig, "  middlewares:\n", "  middlewares:\n    shared-auth:\n      compress: {}\n", 1), ""), "shared.yaml")
	rejected(t, put(e, "new.yaml", "http:\n  routers:\n    x:\n      rule: \"{{ env `HOST` }}\"\n", ""), "templates")
	rejected(t, put(e, "new.yaml", extraConfig+"tls:\n  certificates:\n    - certFile: /data/certs/missing.pem\n      keyFile: /data/certs/missing.key\n", ""), "/data/certs/missing.pem is not readable")
	// Removing a middleware that another file uses would break that file's router.
	shared, _ := f.read("shared.yaml")
	rejected(t, change(e, "delete", "shared.yaml", contentRevision(shared)), "user.yaml")

	f.set(func() {
		f.logs = []string{`{"level":"error","message":"Unable to parse certificate","error":"tls: failed to find any PEM data"}`}
	})
	rejected(t, put(e, "tls.yaml", "tls:\n  options:\n    modern:\n      minVersion: VersionTLS13\n", ""), "Unable to parse certificate")
	f.set(func() { f.logs = nil })

	if after := f.dynamic(""); !mapsEqual(before, after) {
		t.Fatalf("a rejected change reached the dynamic directory: %v", after)
	}
	state, err := e.IngressConfig(context.Background())
	if err != nil || state.Blocked != "" || state.Change == nil || state.Change.State != "rejected" {
		t.Fatalf("rejection not recorded or blocks later changes: %+v %v", state, err)
	}

	content := extraConfig
	valid := e.ChangeIngressConfig(context.Background(), IngressConfigRequest{Action: "validate", Name: "new.yaml", Content: &content})
	if valid.Change == nil || valid.Change.State != "valid" {
		t.Fatalf("validation failed: %+v", valid)
	}
	if _, ok := f.read("new.yaml"); ok {
		t.Fatal("validation wrote the file")
	}
	if recorded, _ := e.loadCustomIngress(); recorded.Change.State != "rejected" {
		t.Fatal("validation replaced the recorded change")
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func TestCustomIngressImportPreservesExistingFile(t *testing.T) {
	e, f := customEngine(t)
	legacy := "# imported\nhttp:\n  routers:\n    legacy:\n      rule: Host(`app.example.test`)\n      entryPoints: [websecure]\n      priority: 100\n      service: legacy\n    broken:\n      rule: Host(`broken.example.test`)\n      entryPoints: [websecure]\n      service: legacy\n      middlewares: [never-defined]\n  middlewares:\n    cove-legacy:\n      compress: {}\n  services:\n    legacy:\n      loadBalancer:\n        servers:\n          - url: http://10.0.0.9\n"
	f.write("legacy-import.yaml", legacy)
	state, err := e.IngressConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, file := range state.Files {
		statuses[file.Name] = file.Status
	}
	if statuses["legacy-import.yaml"] != "unmanaged" || statuses["routes.yaml"] != "protected" || !slices.Equal(state.Resolvers, []string{"cove"}) || len(state.EntryPoints) != 3 || !slices.Equal(state.Ports, []string{"443 → 443/tcp"}) {
		t.Fatalf("unexpected state: %+v", state)
	}
	file, err := e.IngressConfigFile(context.Background(), "legacy-import.yaml")
	if err != nil || *file.Content != legacy || len(file.Warnings) == 0 {
		t.Fatalf("file detail wrong: %+v %v", file, err)
	}
	if !slices.ContainsFunc(file.Objects, func(object IngressConfigObject) bool { return object.Name == "broken" && object.Status == "disabled" }) {
		t.Fatalf("live object status missing: %+v", file.Objects)
	}
	revision := contentRevision(legacy)
	if result := put(e, "legacy-import.yaml", legacy, revision); result.Code != "invalid" {
		t.Fatalf("unmanaged file edited without import: %+v", result)
	}
	imported := succeeded(t, change(e, "import", "legacy-import.yaml", revision))
	if content, _ := f.read("legacy-import.yaml"); content != legacy || len(imported.Warnings) < 2 {
		t.Fatalf("import changed the file or hid existing issues: %+v", imported)
	}
	// Existing conflicts and errors stay warnings; new conflicts are refused.
	edited := strings.Replace(legacy, "  services:\n", "    headers:\n      headers:\n        customResponseHeaders:\n          X-Imported: \"yes\"\n  services:\n", 1)
	succeeded(t, put(e, "legacy-import.yaml", edited, revision))
	rejected(t, put(e, "legacy-import.yaml", strings.Replace(edited, "broken.example.test", "panel.example.test", 1), contentRevision(edited)), "panel.example.test")
}

func TestCustomIngressRestoresWhenProxyKeepsPreviousConfiguration(t *testing.T) {
	e, f := customEngine(t)
	first := succeeded(t, put(e, "extra.yaml", extraConfig, ""))
	f.set(func() { f.stale = fakeRawData(f.dynamic("")) })
	result := put(e, "extra.yaml", strings.Replace(extraConfig, "10.0.0.5", "10.0.0.6", 1), first.Revision)
	if result.Change == nil || result.Change.State != "failed" || !strings.Contains(result.Change.Message, "previous configuration is back") {
		t.Fatalf("unconfirmed change not reported as failed: %+v", result.Change)
	}
	if content, _ := f.read("extra.yaml"); content != extraConfig {
		t.Fatal("previous file not restored")
	}
	if result = put(e, "new.yaml", strings.NewReplacer("extra", "fresh", "compress", "gzip").Replace(extraConfig), ""); result.Change == nil || result.Change.State != "failed" {
		t.Fatalf("unconfirmed new file not reported as failed: %+v", result.Change)
	}
	if _, ok := f.read("new.yaml"); ok {
		t.Fatal("unconfirmed new file left in the dynamic directory")
	}
	state, err := e.IngressConfig(context.Background())
	if err != nil || state.Blocked != "" {
		t.Fatalf("settled failure blocks changes: %+v %v", state, err)
	}
	if recorded, _ := e.loadCustomIngress(); recorded.Managed["extra.yaml"].Revision != first.Revision || len(recorded.Managed) != 1 {
		t.Fatalf("failed changes altered managed records: %+v", recorded.Managed)
	}
}

func TestCustomIngressRetriesRestoreUntilSettled(t *testing.T) {
	retryRoutesImmediately(t)
	e, f := customEngine(t)
	first := succeeded(t, put(e, "extra.yaml", extraConfig, ""))
	f.set(func() {
		f.stale = fakeRawData(f.dynamic(""))
		f.fail = func(command string) error {
			if strings.Contains(command, ".restore") {
				return errors.New("volume unavailable")
			}
			return nil
		}
	})
	edited := strings.Replace(extraConfig, "10.0.0.5", "10.0.0.6", 1)
	result := put(e, "extra.yaml", edited, first.Revision)
	if result.Change == nil || result.Change.State != "restoring" {
		t.Fatalf("unrestored change not reported as restoring: %+v", result.Change)
	}
	state, err := e.IngressConfig(context.Background())
	if err != nil || state.Blocked == "" || state.Change.State != "restoring" {
		t.Fatalf("unsettled change not visible: %+v %v", state, err)
	}
	if result = put(e, "extra.yaml", extraConfig, contentRevision(edited)); result.Code != "blocked" {
		t.Fatalf("new change accepted while restoring: %+v", result)
	}
	settle(t, e)
	if recorded, _ := e.loadCustomIngress(); recorded.Change.State != "restoring" {
		t.Fatalf("restore recorded before it succeeded: %+v", recorded.Change)
	}
	f.set(func() { f.fail, f.stale = nil, nil })
	settle(t, e)
	recorded, err := e.loadCustomIngress()
	if err != nil || recorded.Change.State != "failed" || recorded.Change.FinishedAt == nil {
		t.Fatalf("recovery did not settle: %+v %v", recorded.Change, err)
	}
	if content, _ := f.read("extra.yaml"); content != extraConfig {
		t.Fatal("recovery did not restore the previous file")
	}
	if e.customUnsettled {
		t.Fatal("recovery still scheduled after settling")
	}
}

func TestCustomIngressRecoversAfterWorkerRestart(t *testing.T) {
	e, f := customEngine(t)
	first := succeeded(t, put(e, "extra.yaml", extraConfig, ""))
	edited := strings.Replace(extraConfig, "10.0.0.5", "10.0.0.6", 1)
	for _, crash := range []struct {
		name    string
		prepare func() *customIngressJournal
		want    string
		present bool
	}{
		{"replaced file", func() *customIngressJournal {
			if err := os.WriteFile(filepath.Join(f.volume, "cove-custom-history", "extra.yaml", "crash-a.previous"), []byte(extraConfig), 0600); err != nil {
				t.Fatal(err)
			}
			f.write("extra.yaml", edited)
			return &customIngressJournal{IngressConfigChange: IngressConfigChange{ID: "crash-a", Action: "put", Name: "extra.yaml", State: "applying"}, Previous: first.Revision, Desired: contentRevision(edited), Backup: "crash-a.previous"}
		}, extraConfig, true},
		{"delete before the move", func() *customIngressJournal {
			return &customIngressJournal{IngressConfigChange: IngressConfigChange{ID: "crash-b", Action: "delete", Name: "extra.yaml", State: "applying"}, Previous: first.Revision, Backup: "crash-b.deleted"}
		}, extraConfig, true},
		{"new file", func() *customIngressJournal {
			f.write("fresh.yaml", edited)
			return &customIngressJournal{IngressConfigChange: IngressConfigChange{ID: "crash-c", Action: "put", Name: "fresh.yaml", State: "applying"}, Desired: contentRevision(edited)}
		}, "", false},
	} {
		state, err := e.loadCustomIngress()
		if err != nil {
			t.Fatal(err)
		}
		state.Change = crash.prepare()
		if err = e.saveCustomIngress(state); err != nil {
			t.Fatal(err)
		}
		restarted, err := NewEngine(e.Root)
		if err != nil {
			t.Fatal(err)
		}
		restarted.Run = f.run
		if err = restarted.Recover(); err != nil || !restarted.customUnsettled {
			t.Fatalf("%s: restart did not schedule recovery: %v", crash.name, err)
		}
		settle(t, restarted)
		recorded, err := restarted.loadCustomIngress()
		if err != nil || recorded.Change.State != "interrupted" {
			t.Fatalf("%s: outcome not recorded: %+v %v", crash.name, recorded.Change, err)
		}
		if content, present := f.read(state.Change.Name); present != crash.present || content != crash.want {
			t.Fatalf("%s: file not restored: %q %v", crash.name, content, present)
		}
	}
}

func TestCustomIngressRefusesUnsafeRequests(t *testing.T) {
	e, f := customEngine(t)
	for _, name := range []string{"routes.yaml", "panel.yaml", "cove-extra.yaml", "Upper.yaml", "../escape.yaml", "notes.toml"} {
		if result := put(e, name, "{}", ""); result.Code != "invalid" {
			t.Errorf("%s accepted: %+v", name, result)
		}
	}
	content := "{}"
	for _, request := range []IngressConfigRequest{
		{Action: "put", Name: "a.yaml", Content: &content},
		{Action: "put", ChangeID: "x", Name: "a.yaml"},
		{Action: "delete", ChangeID: "x", Name: "a.yaml", Content: &content},
		{Action: "put", ChangeID: "x", Name: "a.yaml", Content: &content, Revision: "abc"},
		{Action: "restart", ChangeID: "x", Name: "a.yaml"},
	} {
		if result := e.ChangeIngressConfig(context.Background(), request); result.Code != "invalid" {
			t.Errorf("invalid request accepted: %+v", request)
		}
	}
	if _, err := e.Maintenance(true); err != nil {
		t.Fatal(err)
	}
	if result := put(e, "a.yaml", "{}", ""); result.Code != "blocked" {
		t.Errorf("change accepted while draining: %+v", result)
	}
	if _, err := e.Maintenance(false); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(e.Root, "ingress-mode.json"), migrationIngressMode{Staged: true, TLSChallenge: true}); err != nil {
		t.Fatal(err)
	}
	if result := put(e, "a.yaml", "{}", ""); result.Code != "blocked" || !strings.Contains(result.Error, "migration") {
		t.Errorf("change accepted on a staged proxy: %+v", result)
	}
	if err := os.Remove(filepath.Join(e.Root, "ingress-mode.json")); err != nil {
		t.Fatal(err)
	}
	f.set(func() { f.running = false })
	if result := put(e, "a.yaml", "{}", ""); result.Code != "unavailable" {
		t.Errorf("change accepted while the proxy is stopped: %+v", result)
	}
	state, err := e.IngressConfig(context.Background())
	if err != nil || state.Running || len(state.Files) != 1 || state.Notice == "" {
		t.Fatalf("stopped proxy state wrong: %+v %v", state, err)
	}
	if found := lifecycleCommands(f.recorded()); len(found) != 0 {
		t.Fatalf("stopped proxy was started: %v", found)
	}
	f.set(func() { f.exists = false })
	if state, err = e.IngressConfig(context.Background()); err != nil || state.Available || !strings.Contains(state.Unavailable, "first resource route") {
		t.Fatalf("missing proxy state wrong: %+v %v", state, err)
	}
	f.set(func() { f.exists, f.running = true, true })
	for i := 0; i < maxCustomIngressFiles; i++ {
		f.write(fmt.Sprintf("file-%02d.yaml", i), "{}")
	}
	if result := put(e, "one-more.yaml", "{}", ""); result.Code != "invalid" || !strings.Contains(result.Error, "at most") {
		t.Errorf("file limit not enforced: %+v", result)
	}
}

func TestCustomIngressReadsResolversFromStaticFile(t *testing.T) {
	e, f := customEngine(t)
	f.set(func() {
		f.staticConfig = "certificatesResolvers:\n  cove:\n    acme:\n      storage: /data/traefik-acme.json\n  letsencrypt:\n    acme:\n      storage: /data/le.json\n"
	})
	state, err := e.IngressConfig(context.Background())
	if err != nil || !slices.Equal(state.Resolvers, []string{"cove", "letsencrypt"}) {
		t.Fatalf("resolvers not read from the static file: %+v %v", state.Resolvers, err)
	}
	succeeded(t, put(e, "le.yaml", strings.Replace(extraConfig, "[compress]", "[compress]\n      tls:\n        certResolver: letsencrypt", 1), ""))
}

func TestResourceRoutesCannotTakeCustomHostnames(t *testing.T) {
	web := newContainer("0123456789ab", "app", "web", "running")
	e, _ := routeEngine(t, web)
	deployed(t, e, "app", []Route{oldRoute})
	if err := e.saveCustomIngress(customIngressState{Managed: map[string]customIngressRecord{"legacy.yaml": {Hosts: []string{"custom.example.com"}, HostPatterns: []string{`^.+\.wild\.example\.com$`}}}}); err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"custom.example.com", "a.wild.example.com"} {
		op := runRoutes(t, e, "take-"+strings.Split(domain, ".")[0], "app", []Route{oldRoute, {Domain: domain, Service: "web", Port: 80}})
		logs, _ := e.Logs(op.ID)
		if op.State != "failed" || !strings.Contains(logs, "custom proxy configuration legacy.yaml") {
			t.Fatalf("route took a custom hostname: %+v %s", op, logs)
		}
	}
	if owned, err := e.loadIngress(); err != nil || !slices.Equal(owned["app"], []Route{oldRoute}) {
		t.Fatalf("refused routes changed the saved routes: %v %v", owned, err)
	}
	if err := e.customHostConflict([]Route{{Domain: "custom.example.com"}}, []Route{{Domain: "custom.example.com"}}); err != nil {
		t.Fatal("a hostname the resource already serves was refused:", err)
	}
	if op := runRoutes(t, e, "unrelated", "app", []Route{newRoute}); op.State != "succeeded" {
		t.Fatalf("unrelated route refused: %+v", op)
	}
}

func TestCustomIngressAdoptsMigratedCertificates(t *testing.T) {
	e, f := customEngine(t)
	if err := os.MkdirAll(filepath.Join(f.volume, "migrated-certificates"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mineplace.crt", "mineplace.key", "vtuber.crt", "vtuber.key"} {
		if err := os.WriteFile(filepath.Join(f.volume, "migrated-certificates", name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	migrated := "tls:\n  certificates:\n    - certFile: /data/migrated-certificates/mineplace.crt\n      keyFile: /data/migrated-certificates/mineplace.key\n"
	f.write("migrated-certificates.yaml", migrated)
	imported := succeeded(t, change(e, "import", "migrated-certificates.yaml", contentRevision(migrated)))
	if content, _ := f.read("migrated-certificates.yaml"); content != migrated || len(imported.Warnings) != 0 {
		t.Fatalf("import changed the file or warned about valid paths: %v", imported.Warnings)
	}
	added := migrated + "    - certFile: /data/migrated-certificates/vtuber.crt\n      keyFile: /data/migrated-certificates/vtuber.key\n"
	succeeded(t, put(e, "migrated-certificates.yaml", added, contentRevision(migrated)))
	rejected(t, put(e, "migrated-certificates.yaml", added+"    - certFile: /data/migrated-certificates/missing.crt\n      keyFile: /data/migrated-certificates/vtuber.key\n", contentRevision(added)), "missing.crt is not readable")
	rejected(t, put(e, "migrated-certificates.yaml", added+"    - certFile: /data/traefik-acme.json\n      keyFile: /data/cloudflare-dns-token\n", contentRevision(added)), "inside the proxy volume")

	// A file that already points outside the volume is adopted with a warning and stays editable.
	outside := "tls:\n  certificates:\n    - certFile: /etc/certs/tracker.crt\n      keyFile: /etc/certs/tracker.key\n"
	f.write("tracker-migration-certificate.yaml", outside)
	adopted := succeeded(t, change(e, "import", "tracker-migration-certificate.yaml", contentRevision(outside)))
	if !slices.ContainsFunc(adopted.Warnings, func(warning string) bool {
		return strings.Contains(warning, "Existing issue") && strings.Contains(warning, "/etc/certs/tracker.crt")
	}) {
		t.Fatalf("existing path issue not reported: %v", adopted.Warnings)
	}
	succeeded(t, put(e, "tracker-migration-certificate.yaml", "# reviewed\n"+outside, contentRevision(outside)))
}

func TestCustomIngressCannotReferenceManagedRouteKeys(t *testing.T) {
	for _, path := range []string{"/data/cove-certificates/app/version.key", "/data/cove-certificates/app/version.crt", "/data/certificate-probes.json"} {
		if certificatePath(path) {
			t.Fatal("custom configuration could reference private managed certificate material", path)
		}
	}
	if !certificatePath("/data/certs/operator-managed.key") {
		t.Fatal("operator-managed certificate path regressed")
	}
}
