package worker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestRouteAuxiliaryDocker(t *testing.T) {
	if os.Getenv("COVE_TEST_ROUTE_AUX_DOCKER") != "1" || runtime.GOOS != "linux" {
		t.Skip("isolated Docker/Traefik route helper fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	verifyPublicAuthTLSInFixture(t, ctx)
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.ACMEDirectory = "https://127.0.0.1:9/directory"
	workload := "route-aux-acceptance"
	command := func(args ...string) string {
		t.Helper()
		text, err := e.output(ctx, args...)
		if err != nil {
			t.Fatal(args, text, err)
		}
		return text
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		ids, _ := e.output(cleanup, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+workload)
		helpers, _ := e.output(cleanup, "docker", "ps", "-aq", "--filter", "label=cove.managed=route-aux")
		for _, id := range strings.Fields(ids + " " + helpers) {
			e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "-v", id}, nil, io.Discard)
		}
		e.Run(cleanup, e.Root, []string{"docker", "rm", "-f", "cove-ingress"}, nil, io.Discard)
	})
	web := `mkdir -p /www/cgi-bin; echo application-marker > /www/index.html
cat > /www/cgi-bin/who <<'END'
#!/bin/sh
printf 'Content-Type: text/plain\r\n\r\nwho:%s\n' "$HTTP_REMOTE_USER"
END
chmod +x /www/cgi-bin/who
exec httpd -f -p 8080 -h /www`
	auth := `mkdir -p /www/cgi-bin; echo auth-ready > /www/index.html
cat > /www/cgi-bin/check <<'END'
#!/bin/sh
case "$HTTP_COOKIE" in
 *session=fake*)
   printf 'Status: 200 OK\r\n'
   if ! test -f /tmp/no-identity; then printf 'Remote-User: alice\r\n'; fi
   printf 'Set-Cookie: refreshed=fake; Path=/; HttpOnly; Secure; SameSite=Lax\r\nContent-Type: text/plain\r\n\r\n'
   ;;
 *) printf 'Status: 302 Found\r\nLocation: https://auth.example.test/login\r\nContent-Type: text/plain\r\n\r\nlogin-required' ;;
esac
END
chmod +x /www/cgi-bin/check
exec httpd -f -p 8081 -h /www`
	service := func(script string, port int) map[string]any {
		return map[string]any{"image": "busybox:1.37", "pull_policy": "never", "init": true, "command": []string{"sh", "-ec", strings.ReplaceAll(script, "$", "$$")}, "healthcheck": map[string]any{"test": []string{"CMD", "wget", "-qO-", fmt.Sprintf("http://127.0.0.1:%d/", port)}, "interval": "1s", "timeout": "1s", "retries": 3}}
	}
	compose, _ := json.Marshal(map[string]any{"services": map[string]any{"web": service(web, 8080), "auth": service(auth, 8081)}})
	hash, err := bcrypt.GenerateFromPassword([]byte("fake-password"), 10)
	if err != nil {
		t.Fatal(err)
	}
	route := Route{Domain: "pages.example.test", Service: "web", Port: 8080, Auth: "tester:" + string(hash), Policy: &RoutePolicy{CertificateSource: "default"}}
	run := func(id, action string, routes []Route, expected string) {
		t.Helper()
		r := Request{ID: id, Workload: workload, Action: action, Kind: "service", Routes: routes}
		if action == "deploy" {
			r.Compose = string(compose)
		}
		if _, err := e.Submit(r); err != nil {
			t.Fatal(err)
		}
		var jobs sync.WaitGroup
		if err := e.dispatch(ctx, &jobs); err != nil {
			t.Fatal(err)
		}
		jobs.Wait()
		op, err := e.Status(id)
		logs, _ := e.Logs(id)
		if err != nil || op.State != expected {
			t.Fatal(op, err, logs)
		}
	}
	run("initial", "deploy", []Route{route}, "succeeded")
	before := command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+workload)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(path string, basic, cookie bool) (int, string, http.Header) {
		t.Helper()
		r, _ := http.NewRequestWithContext(ctx, "GET", "https://127.0.0.1"+path, nil)
		r.Host = route.Domain
		if basic {
			r.SetBasicAuth("tester", "fake-password")
		}
		if cookie {
			r.Header.Set("Cookie", "session=fake")
		}
		r.Header.Set("Remote-User", "forged-admin")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(body), response.Header
	}
	route.Policy.Maintenance = &RouteMaintenance{Enabled: true, HTML: "<h1>maintenance-marker</h1>"}
	run("maintenance", "routes", []Route{route}, "succeeded")
	if code, body, _ := request("/deep/path?q=1", false, false); code != 401 || strings.Contains(body, "maintenance-marker") {
		t.Fatal("maintenance bypassed basic auth", code, body)
	}
	if code, body, headers := request("/deep/path?q=1", true, false); code != 503 || !strings.Contains(body, "maintenance-marker") || headers.Get("Cache-Control") != "no-store" {
		t.Fatal(code, body, headers)
	}
	if got := command("docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+workload); got != before {
		t.Fatal("maintenance replaced app containers", before, got)
	}
	route.Policy.Maintenance = nil
	route.Policy.ErrorPages = map[string]string{"404": "<h1>custom-404-marker</h1>", "503": "<h1>custom-503-marker</h1>"}
	run("errors", "routes", []Route{route}, "succeeded")
	if code, body, _ := request("/missing", true, false); code != 404 || !strings.Contains(body, "custom-404-marker") {
		t.Fatal("custom page/status missing", code, body)
	}
	route.Policy.ForwardAuth = &RouteForwardAuth{Service: "auth", Port: 8081, Path: "/cgi-bin/check", ResponseHeaders: []string{"Remote-User"}, ResponseCookies: []string{"refreshed"}}
	run("forward-auth", "routes", []Route{route}, "succeeded")
	if code, body, headers := request("/cgi-bin/who", true, false); code != 302 || headers.Get("Location") != "https://auth.example.test/login" || strings.Contains(body, "who:") {
		t.Fatal("forward-auth did not gate application", code, body, headers)
	}
	if code, body, headers := request("/cgi-bin/who", true, true); code != 200 || !strings.Contains(body, "who:alice") || !strings.Contains(strings.Join(headers.Values("Set-Cookie"), ";"), "refreshed=fake") {
		t.Fatal("authenticated identity/cookie missing", code, body, headers)
	}
	originalRun := e.Run
	failedHelper := false
	e.Run = func(ctx context.Context, dir string, args, env []string, out io.Writer) error {
		if !failedHelper && len(args) > 2 && args[0] == "docker" && args[1] == "create" && strings.Contains(strings.Join(args, " "), "cove.managed=route-aux") {
			failedHelper = true
			return fmt.Errorf("injected helper creation failure")
		}
		return originalRun(ctx, dir, args, env, out)
	}
	attempt := route
	policy := *route.Policy
	policy.Maintenance = &RouteMaintenance{Enabled: true, HTML: "must-not-replace-auth"}
	attempt.Policy = &policy
	run("helper-failure", "routes", []Route{attempt}, "failed")
	e.Run = originalRun
	if !failedHelper {
		t.Fatal("helper failure was not exercised")
	}
	if code, body, _ := request("/cgi-bin/who", true, false); code != 302 || strings.Contains(body, "must-not-replace-auth") {
		t.Fatal("failed update removed auth", code, body)
	}
	if code, body, _ := request("/cgi-bin/who", true, true); code != 200 || !strings.Contains(body, "who:alice") {
		t.Fatal("failed update did not restore route", code, body)
	}
	e, err = NewEngine(e.Root)
	if err != nil {
		t.Fatal(err)
	}
	if code, body, _ := request("/cgi-bin/who", true, false); code != 302 {
		t.Fatal("worker restart removed auth", code, body)
	}
	if saved, err := e.loadIngress(); err != nil || !RoutesEqual(saved[workload], []Route{route}) {
		t.Fatal("rollback state changed after worker restart", saved, err)
	}
	authID := strings.TrimSpace(command("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload, "--filter", "label=com.docker.compose.service=auth"))
	command("docker", "exec", authID, "touch", "/tmp/no-identity")
	if code, body, _ := request("/cgi-bin/who", true, true); code != 200 || strings.Contains(body, "forged-admin") || strings.Contains(body, "alice") {
		t.Fatal("client identity was not stripped", code, body)
	}
	helpers := strings.Fields(command("docker", "ps", "-q", "--filter", "label=cove.managed=route-aux"))
	if len(helpers) != 1 {
		t.Fatal("retired helpers were not cleaned up", helpers)
	}
	command("docker", "restart", helpers[0])
	for deadline := time.Now().Add(5 * time.Second); ; {
		code, _, _ := request("/cgi-bin/who", true, true)
		if code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper restart did not recover", code)
		}
		time.Sleep(100 * time.Millisecond)
	}
	route.Policy.RedirectHost = "destination.example.test"
	run("auth-redirect", "routes", []Route{route}, "succeeded")
	if code, _, headers := request("/cgi-bin/who", true, false); code != 302 || headers.Get("Location") != "https://auth.example.test/login" {
		t.Fatal("redirect bypassed forward-auth", code, headers)
	}
	if code, _, headers := request("/cgi-bin/who", true, true); code != 302 || headers.Get("Location") != "https://destination.example.test/cgi-bin/who" {
		t.Fatal("authenticated redirect failed", code, headers)
	}
	route.Policy.RedirectHost = ""
	route.Policy.ForwardAuth = nil
	route.Policy.ErrorPages = nil
	run("disable", "routes", []Route{route}, "succeeded")
	if code, body, _ := request("/", true, false); code != 200 || !strings.Contains(body, "application-marker") {
		t.Fatal(code, body)
	}
	if helpers := command("docker", "ps", "-aq", "--filter", "label=cove.managed=route-aux"); helpers != "" {
		t.Fatal("unused helper retained", helpers)
	}
}
