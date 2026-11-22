package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestEnvironmentPreviewDockerAcceptance runs the shipped preview workflow against a real worker
// over SSH, a real HTTPS Git server and real PostgreSQL, MariaDB, Redis and application containers.
// GitHub's API and webhook deliveries are simulated in process; branch heads come from the real
// repositories. It needs COVE_E2E=1, a disposable COVE_TEST_DATABASE_URL and a Docker host on which
// /var/lib/cove does not exist.
func TestEnvironmentPreviewDockerAcceptance(t *testing.T) {
	if os.Getenv("COVE_E2E") != "1" {
		t.Skip("COVE_E2E=1 required; test creates disposable Docker resources")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	host := func(args ...string) (string, error) {
		call, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(call, "docker", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	hostMust := func(args ...string) string {
		t.Helper()
		out, err := host(args...)
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	image := "cove-preview-e2e-" + resourceID()[:12] + ":test"
	name := "cove-preview-e2e-" + resourceID()[:12]
	// Workloads run on the fixture's own Docker daemon, never on the host daemon.
	docker := func(args ...string) (string, error) {
		return host(append([]string{"exec", name, "docker"}, args...)...)
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := docker(args...)
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	f := newAccountFixture(t)
	s := f.s
	call := func(method, path string, input any, status int) []byte {
		t.Helper()
		return f.call("root", "default", method, path, input, status).Body.Bytes()
	}

	// Real Git repositories served over HTTPS with a private CA the worker trusts.
	gitRoot := t.TempDir()
	caDir := t.TempDir()
	certificate, caPEM := e2eCertificate(t, "host.docker.internal")
	if err = os.WriteFile(filepath.Join(caDir, "ca.pem"), caPEM, 0644); err != nil {
		t.Fatal(err)
	}
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	server := httptest.NewUnstartedServer(&cgi.Handler{Path: backend, Root: "/", Env: []string{"GIT_PROJECT_ROOT=" + gitRoot, "GIT_HTTP_EXPORT_ALL=1"}})
	server.Listener, err = net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	server.StartTLS()
	defer server.Close()
	gitPort := server.Listener.Addr().(*net.TCPAddr).Port
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Cove", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Cove", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	repos := map[string]string{}
	repoIDs := map[string]int64{"web": 201, "api": 202, "worker": 203}
	createRepo := func(name string, files map[string]string) {
		work := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(work, 0755); err != nil {
			t.Fatal(err)
		}
		git(work, "init", "--initial-branch=main")
		for file, content := range files {
			if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
		git(work, "add", ".")
		git(work, "commit", "-m", "initial")
		bare := filepath.Join(gitRoot, name+".git")
		git(gitRoot, "clone", "--bare", work, bare)
		git(bare, "config", "uploadpack.allowReachableSHA1InWant", "true")
		git(work, "remote", "add", "origin", bare)
		repos[name] = work
	}
	commit := func(name, branch, file, content string) string {
		work := repos[name]
		git(work, "checkout", "-B", branch)
		if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		git(work, "commit", "-am", "change "+file)
		git(work, "push", "--force", "origin", branch)
		return git(work, "rev-parse", "HEAD")
	}
	deleteBranch := func(name, branch string) {
		git(repos[name], "checkout", "main")
		git(repos[name], "push", "origin", "--delete", branch)
	}
	webApp := func(version string) string {
		return "import http.server, os\nVERSION = '" + version + "'\nclass H(http.server.BaseHTTPRequestHandler):\n    def do_GET(self):\n        body = (VERSION + ' ' + os.environ.get('NODE_ENV', '') + '\\n').encode()\n        self.send_response(200)\n        self.send_header('Content-Length', str(len(body)))\n        self.end_headers()\n        self.wfile.write(body)\n    def log_message(self, *args):\n        pass\nhttp.server.ThreadingHTTPServer(('0.0.0.0', 8080), H).serve_forever()\n"
	}
	createRepo("web", map[string]string{
		"Dockerfile":   "FROM python:3.13-alpine\nCOPY app.py /app.py\nCMD [\"python\", \"/app.py\"]\n",
		"app.py":       webApp("web-main"),
		"compose.yaml": "services:\n  web:\n    build: .\n    environment:\n      NODE_ENV: ${NODE_ENV}\n      SESSION_SECRET: ${SESSION_SECRET}\n    ports: ['127.0.0.1::8080']\n    healthcheck:\n      test: [CMD, python, -c, \"import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080')\"]\n      interval: 2s\n      retries: 30\n",
	})
	apiStart := func(migration string) string {
		return "set -e\nexport PGPASSWORD=\"$POSTGRES_PASSWORD\"\nuntil pg_isready -h db -U app -d app; do sleep 1; done\npsql -h db -U app -d app -v ON_ERROR_STOP=1 -c 'CREATE TABLE IF NOT EXISTS items(id serial PRIMARY KEY, name text NOT NULL)'\n" + migration + "echo started > /uploads/started-$(hostname).txt\ntouch /tmp/ready\nexec tail -f /dev/null\n"
	}
	createRepo("api", map[string]string{
		"Dockerfile":   "FROM postgres:17-alpine\nCOPY start.sh /start.sh\nCMD [\"sh\", \"/start.sh\"]\n",
		"start.sh":     apiStart(""),
		"compose.yaml": "services:\n  db:\n    image: postgres:17-alpine\n    environment:\n      POSTGRES_USER: app\n      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n      POSTGRES_DB: app\n    volumes: [pgdata:/var/lib/postgresql/data]\n    healthcheck:\n      test: [CMD-SHELL, 'pg_isready -U app -d app']\n      interval: 2s\n      retries: 60\n  api:\n    build: .\n    environment:\n      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n      STRIPE_KEY: ${STRIPE_KEY}\n      CACHE_URL: ${CACHE_URL}\n    volumes: [uploads:/uploads]\n    depends_on:\n      db: {condition: service_healthy}\n    healthcheck:\n      test: [CMD, test, -f, /tmp/ready]\n      interval: 2s\n      retries: 90\nvolumes:\n  pgdata: {}\n  uploads: {}\n",
	})
	createRepo("worker", map[string]string{
		"compose.yaml": "services:\n  worker:\n    image: busybox:1.37\n    command: [sh, -c, 'while true; do echo tick; sleep 30; done']\n",
	})

	// GitHub's API, answered from the real repositories.
	installationRequests := 0
	f.s.OAuthClient = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		reply := func(status int, value any) (*http.Response, error) {
			body, _ := json.Marshal(value)
			return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{"Content-Type": {"application/json"}}}, nil
		}
		path := r.URL.EscapedPath()
		if path == "/app/installations/41/access_tokens" {
			installationRequests++
			return reply(201, map[string]string{"token": "e2e-installation-token"})
		}
		var repository int64
		var kind string
		if _, err := fmt.Sscanf(path, "/repositories/%d/git/%s", &repository, &kind); err != nil {
			return reply(404, map[string]string{})
		}
		name := ""
		for candidate, id := range repoIDs {
			if id == repository {
				name = candidate
			}
		}
		bare := filepath.Join(gitRoot, name+".git")
		if strings.HasPrefix(kind, "ref/heads/") {
			branch := strings.TrimPrefix(kind, "ref/heads/")
			head, err := exec.Command("git", "--git-dir", bare, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Output()
			if err != nil {
				return reply(404, map[string]string{"message": "Not Found"})
			}
			return reply(200, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"sha": strings.TrimSpace(string(head))}})
		}
		prefix := strings.TrimPrefix(kind, "matching-refs/heads/")
		out, _ := exec.Command("git", "--git-dir", bare, "for-each-ref", "--format=%(refname)", "refs/heads/"+prefix).Output()
		refs := []map[string]string{}
		for _, ref := range strings.Fields(string(out)) {
			refs = append(refs, map[string]string{"ref": ref})
		}
		return reply(200, refs)
	})}
	push := func(delivery, repo, branch string, deleted bool) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"ref": "refs/heads/" + branch, "after": strings.Repeat("0", 40), "deleted": deleted, "installation": map[string]int64{"id": 41}, "repository": map[string]any{"id": repoIDs[repo]}})
		if !deleted {
			head, err := exec.Command("git", "--git-dir", filepath.Join(gitRoot, repo+".git"), "rev-parse", "refs/heads/"+branch).Output()
			if err != nil {
				t.Fatal(err)
			}
			body, _ = json.Marshal(map[string]any{"ref": "refs/heads/" + branch, "after": strings.TrimSpace(string(head)), "deleted": false, "installation": map[string]int64{"id": 41}, "repository": map[string]any{"id": repoIDs[repo]}})
		}
		request := httptest.NewRequest("POST", "/hooks/github-app", bytes.NewReader(body))
		mac := hmac.New(sha256.New, []byte(fixtureWebhookSecret))
		mac.Write(body)
		request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		request.Header.Set("X-GitHub-Event", "push")
		request.Header.Set("X-GitHub-Delivery", delivery)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		if response.Code != 202 && response.Code != 200 {
			t.Fatalf("webhook returned %d %s", response.Code, response.Body.String())
		}
	}

	// The worker fixture: a real cove-worker daemon reached over SSH, with its own Docker daemon.
	build := exec.Command("go", "build", "-o", filepath.Join(root, "bin", "cove-worker-linux-amd64"), "./cmd/cove-worker")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("worker build: %v %s", err, out)
	}
	hostMust("build", "-q", "-t", image, "-f", "test/previews/Dockerfile", ".")
	defer func() {
		host("rm", "-f", "-v", name)
		host("image", "rm", image)
	}()
	hostMust("run", "-d", "--privileged", "--name", name, "--label", "cove.test=true", "--add-host", "host.docker.internal:host-gateway", "-p", "127.0.0.1::22", "-v", caDir+":/run/cove-ca:ro", image)
	for attempt := 0; ; attempt++ {
		if _, err := docker("info"); err == nil {
			break
		}
		if attempt == 60 {
			logs, _ := host("logs", name)
			t.Fatalf("fixture Docker daemon did not start: %s", logs)
		}
		time.Sleep(time.Second)
	}
	// Seed the fixture daemon from the host's image cache instead of pulling every image again.
	for _, cached := range []string{"python:3.13-alpine", "postgres:17-alpine", "busybox:1.37", "mariadb:11.4", "redis:7-alpine", "curlimages/curl:8.11.1", "traefik:v3.7.19"} {
		save := exec.Command("docker", "save", cached)
		load := exec.Command("docker", "exec", "-i", name, "docker", "load", "-q")
		pipe, err := save.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		load.Stdin = pipe
		if err = save.Start(); err != nil {
			t.Fatal(err)
		}
		out, loadErr := load.CombinedOutput()
		if err = save.Wait(); err != nil || loadErr != nil {
			t.Logf("could not preload %s (%v %v %s); the fixture will pull it", cached, err, loadErr, out)
		}
	}
	pub, seed, err := keyPair()
	if err != nil {
		t.Fatal(err)
	}
	authorized := fmt.Sprintf("restrict,command=\"/usr/local/bin/cove-worker rpc\" %s", pub)
	hostMust("exec", name, "sh", "-c", "printf '%s\n' \"$0\" > /var/lib/cove/.ssh/authorized_keys && chown cove:cove /var/lib/cove/.ssh/authorized_keys && chmod 600 /var/lib/cove/.ssh/authorized_keys && printf '{\"staged\":true,\"tls_challenge\":true}' > /var/lib/cove/ingress-mode.json && chown cove:cove /var/lib/cove/ingress-mode.json", authorized)
	hostKey := hostMust("exec", name, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
	published := hostMust("port", name, "22/tcp")
	port, err := strconv.Atoi(published[strings.LastIndex(published, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := seal(s.Key, seed)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,host_key,state,mode) VALUES('e2e','E2E worker','127.0.0.1',$1,$2,$3,$4,'enrolled','ssh')`, port, pub, encrypted, strings.TrimSpace(hostKey))
	f.sql(`INSERT INTO team_nodes VALUES('default','e2e')`)
	f.sql(`INSERT INTO node_links(node_id,link_address) VALUES('e2e','127.0.0.1')`)
	for attempt := 0; ; attempt++ {
		response, err := s.nodeRPC(ctx, "e2e", worker.RPCRequest{Method: "probe"})
		if err == nil && response.Probe != nil && response.Probe.Ready {
			capabilities, _ := json.Marshal(response.Probe)
			f.sql(`UPDATE nodes SET state='ready',capabilities=$1 WHERE id='e2e'`, capabilities)
			break
		}
		if attempt == 60 {
			logs, _ := host("logs", name)
			t.Fatalf("worker not ready: %v %s", err, logs)
		}
		time.Sleep(time.Second)
	}
	credentials, err := s.sealJSON(githubAppCredentials{PEM: e2eRSAKey(t), WebhookSecret: fixtureWebhookSecret})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO github_app(id,app_id,slug,credentials) VALUES(1,77,'cove-e2e',$1)`, credentials)
	f.sql(`INSERT INTO github_installations(id,account) VALUES(41,'acme')`)
	f.sql(`INSERT INTO github_installation_teams VALUES(41,'default')`)

	// Production, created and deployed through the ordinary API.
	project := jsonString(t, f.call("root", "default", "POST", "/api/projects", map[string]string{"name": "Shop"}, 201), "id")
	production := jsonString(t, f.call("root", "default", "POST", "/api/projects/"+project+"/environments", map[string]string{"name": "Production"}, 201), "id")
	repository := func(name string) string { return fmt.Sprintf("https://host.docker.internal:%d/%s.git", gitPort, name) }
	create := func(input workloadInput) string {
		input.NodeID, input.EnvironmentID = "e2e", production
		return jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	}
	web := create(workloadInput{Name: "web", Source: &worker.GitSource{Repository: repository("web"), Branch: "main", ComposePath: "compose.yaml"}, Environment: map[string]string{"NODE_ENV": "production", "SESSION_SECRET": "production-session-secret"}, Routes: []worker.Route{{Domain: "shop.e2e.test", Service: "web", Port: 8080}}})
	api := create(workloadInput{Name: "api", Source: &worker.GitSource{Repository: repository("api"), Branch: "main", ComposePath: "compose.yaml"}, Environment: map[string]string{"POSTGRES_PASSWORD": "production-db-password", "STRIPE_KEY": "sk_live_production", "CACHE_URL": "redis://cache:6379"}})
	backgroundWorker := create(workloadInput{Name: "worker", Source: &worker.GitSource{Repository: repository("worker"), Branch: "main", ComposePath: "compose.yaml"}})
	shop := create(workloadInput{Name: "shop-db", Compose: "services:\n  db:\n    image: mariadb:11.4\n    environment:\n      MARIADB_ROOT_PASSWORD: ${MARIADB_ROOT_PASSWORD}\n      MARIADB_DATABASE: shop\n    volumes: [data:/var/lib/mysql]\n    healthcheck:\n      test: [CMD, healthcheck.sh, --connect, --innodb_initialized]\n      interval: 2s\n      retries: 90\nvolumes:\n  data: {}\n", Environment: map[string]string{"MARIADB_ROOT_PASSWORD": "production-maria-password"}})
	cache := create(workloadInput{Name: "cache", Compose: "services:\n  redis:\n    image: redis:7-alpine\n    volumes: [redisdata:/data]\nvolumes:\n  redisdata: {}\n"})
	for name, id := range map[string]string{"web": web, "api": api, "worker": backgroundWorker} {
		f.sql(`INSERT INTO github_sources(workload_id,installation_id,repository_id,repository) VALUES($1,41,$2,$3)`, id, repoIDs[name], repository(name))
	}
	call("PUT", "/api/workloads/"+api+"/backup-policy", map[string]any{"backup": map[string]any{"repository": "local", "password": "e2e-backup-password-0123", "keep_last": 3, "interval_hours": 0, "database": map[string]string{"engine": "postgresql", "service": "db", "name": "app"}}}, 200)
	call("PUT", "/api/workloads/"+shop+"/backup-policy", map[string]any{"backup": map[string]any{"repository": "local", "password": "e2e-backup-password-0123", "keep_last": 3, "interval_hours": 0, "database": map[string]string{"engine": "mariadb", "service": "db", "name": "shop"}}}, 200)
	network := jsonString(t, f.call("root", "default", "POST", "/api/networks", map[string]string{"name": "backend", "node_id": "e2e"}, 201), "id")
	call("PUT", "/api/workloads/"+api+"/networks", map[string]any{"networks": []map[string]string{{"network_id": network, "service": "api", "alias": "api"}}}, 200)
	call("PUT", "/api/workloads/"+cache+"/networks", map[string]any{"networks": []map[string]string{{"network_id": network, "service": "redis", "alias": "cache"}}}, 200)
	call("POST", "/api/private-connections", map[string]any{"source_id": web, "source_service": "web", "target_id": api, "target_service": "db", "port": 5432, "alias": "pg.internal"}, 201)

	recent := func() string {
		type entry struct{ id, action, state, resource string }
		entries := []entry{}
		rows, err := s.DB.Query(ctx, `SELECT o.id,o.action,o.state,w.name FROM operations o JOIN workloads w ON w.id=o.workload_id ORDER BY o.created_at DESC LIMIT 8`)
		if err == nil {
			for rows.Next() {
				var e entry
				rows.Scan(&e.id, &e.action, &e.state, &e.resource)
				entries = append(entries, e)
			}
			rows.Close()
		}
		var logs strings.Builder
		for _, e := range entries {
			response, _ := s.nodeRPC(ctx, "e2e", worker.RPCRequest{Method: "logs", ID: e.id})
			fmt.Fprintf(&logs, "--- %s %s %s\n%s\n", e.resource, e.action, e.state, response.Logs)
		}
		return logs.String()
	}
	loop := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Minute)
		for !done() {
			if time.Now().After(deadline) {
				var state, stage, detail string
				s.DB.QueryRow(ctx, `SELECT state,stage,detail FROM environment_previews ORDER BY created_at DESC LIMIT 1`).Scan(&state, &stage, &detail)
				t.Fatalf("timed out waiting for %s (preview %s/%s %s)\n%s", what, state, stage, detail, recent())
			}
			s.DB.Exec(ctx, `UPDATE environment_previews SET next_attempt_at=now() WHERE next_attempt_at>now()`)
			s.DB.Exec(ctx, `UPDATE preview_branch_signals SET next_attempt_at=now() WHERE reason NOT LIKE 'Waiting:%'`)
			if err := s.reconcileEnvironmentPreviews(ctx); err != nil {
				t.Fatal(err)
			}
			if err := s.dispatchOperations(ctx); err != nil {
				t.Fatal(err)
			}
			if err := s.reconcileLinks(ctx); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Second)
		}
	}
	operationDone := func(id string) func() bool {
		return func() bool {
			var state string
			s.DB.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1`, id).Scan(&state)
			if state == "failed" || state == "interrupted" || state == "cancelled" {
				response, _ := s.nodeRPC(ctx, "e2e", worker.RPCRequest{Method: "logs", ID: id})
				t.Fatalf("operation %s %s:\n%s", id, state, response.Logs)
			}
			return state == "succeeded"
		}
	}
	for _, id := range []string{cache, shop, api, web, backgroundWorker} {
		operation := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "deploy"}, 202), "id")
		loop("production deployment", operationDone(operation))
	}
	container := func(workload, service string) string {
		t.Helper()
		id := must("ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload, "--filter", "label=com.docker.compose.service="+service)
		if id == "" || strings.Contains(id, "\n") {
			t.Fatalf("expected one %s container for %s, got %q", service, workload, id)
		}
		return id
	}
	psql := func(workload, query string) string {
		t.Helper()
		return must("exec", container(workload, "db"), "psql", "-U", "app", "-d", "app", "-Atc", query)
	}
	maria := func(workload, password, query string) string {
		t.Helper()
		return must("exec", "-e", "MYSQL_PWD="+password, container(workload, "db"), "mariadb", "-uroot", "-N", "-B", "-e", query)
	}
	psql(api, "INSERT INTO items(name) SELECT 'item-'||g FROM generate_series(1,2500) g")
	maria(shop, "production-maria-password", "USE shop; CREATE TABLE shop.orders(id INT PRIMARY KEY AUTO_INCREMENT, note TEXT) ENGINE=InnoDB; INSERT INTO shop.orders(note) SELECT CONCAT('order-',seq) FROM seq_1_to_1200; CREATE TABLE shop.empty_table(id INT PRIMARY KEY) ENGINE=InnoDB;")
	must("exec", container(api, "api"), "sh", "-c", "mkdir -p /uploads/img && head -c 300000 /dev/urandom > /uploads/img/photo.bin && echo hello > /uploads/readme.txt")
	must("exec", container(cache, "redis"), "redis-cli", "SET", "production-key", "cached")
	digest := func(volume string) string {
		t.Helper()
		tag := must("image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference=cove-link:*")
		if tag == "" {
			t.Fatal("helper image unavailable")
		}
		out := must("run", "--rm", "--network", "none", "--mount", "type=volume,src="+volume+",dst=/from,readonly", "--entrypoint", "/cove-worker", strings.Fields(tag)[0], "volume-digest")
		var result struct{ Digest string }
		json.Unmarshal([]byte(out[strings.LastIndex(out, "{"):]), &result)
		return result.Digest
	}
	checksum := func(workload string) string {
		return psql(workload, "SELECT count(*)||':'||md5(string_agg(id||'='||name, ',' ORDER BY id)) FROM items")
	}
	started := func(workload, service string) string {
		return must("inspect", "--format", "{{.State.StartedAt}}", container(workload, service))
	}
	productionChecksum := checksum(api)
	productionMaria := maria(shop, "production-maria-password", "CHECKSUM TABLE shop.orders")
	productionStarts := map[string]string{"api/db": started(api, "db"), "api/api": started(api, "api"), "shop/db": started(shop, "db"), "web/web": started(web, "web"), "cache/redis": started(cache, "redis")}
	productionNetworks := map[string]bool{}
	for _, workload := range []string{web, api, shop, cache, backgroundWorker} {
		out := must("ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)
		for _, id := range strings.Fields(out) {
			networks := must("inspect", "--format", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", id)
			for _, n := range strings.Fields(networks) {
				productionNetworks[n] = true
			}
		}
	}
	delete(productionNetworks, "cove-ingress")

	// The recipe: web and api follow staging/* branches; credentials need explicit decisions.
	recipe := previewRecipe{Enabled: true, BaseEnvironment: production, BranchPattern: "staging/*", Repositories: []int64{201, 202}, DomainTemplate: "{resource}-{preview}.preview.e2e.test", Access: "private", TTLHours: 72, MaxPreviews: 2, Resources: map[string]previewResourcePolicy{
		web:  {Variables: map[string]previewVariable{"SESSION_SECRET": {Mode: "override", Value: "preview-session-secret"}}},
		api:  {Variables: map[string]previewVariable{"STRIPE_KEY": {Mode: "override", Value: "sk_test_sandbox"}, "POSTGRES_PASSWORD": {Mode: "copy"}}},
		shop: {Variables: map[string]previewVariable{"MARIADB_ROOT_PASSWORD": {Mode: "override", Value: "preview-maria-password"}}},
	}}
	call("PUT", "/api/projects/"+project+"/preview-recipe", recipe, 200)
	planView := call("GET", "/api/projects/"+project+"/preview-recipe", nil, 200)
	var planned struct {
		Plan previewPlan `json:"plan"`
	}
	json.Unmarshal(planView, &planned)
	if planned.Plan.Blocked {
		t.Fatalf("recipe blocked: %s", planView)
	}
	uploads := ""
	for _, item := range planned.Plan.Resources {
		if item.ID == api {
			for _, v := range item.Volumes {
				if v.Key == "volume:uploads" {
					uploads = v.Source
				}
			}
		}
	}
	if uploads == "" {
		t.Fatal("the plan did not find the uploads volume")
	}

	// A branch push creates the preview: web follows the branch, the rest use the production baseline.
	webBranch := commit("web", "staging/demo", "app.py", webApp("web-branch-1"))
	push("e2e-1", "web", "staging/demo", false)
	var preview string
	loop("preview creation", func() bool {
		var state, stage, detail string
		s.DB.QueryRow(ctx, `SELECT id,state,stage,detail FROM environment_previews WHERE suffix='demo'`).Scan(&preview, &state, &stage, &detail)
		if state == "failed" {
			t.Fatalf("preview failed at %s: %s\n%s", stage, detail, recent())
		}
		return state == "ready"
	})
	copies := map[string]string{}
	for base, label := range map[string]string{web: "web", api: "api", shop: "shop", cache: "cache", backgroundWorker: "worker"} {
		var copy string
		s.DB.QueryRow(ctx, `SELECT resource_id FROM environment_preview_resources WHERE preview_id=$1 AND base_id=$2`, preview, base).Scan(&copy)
		copies[label] = copy
	}
	detail := call("GET", "/api/environment-previews/"+preview, nil, 200)
	var view previewView
	if err = json.Unmarshal(detail, &view); err != nil {
		t.Fatal(err)
	}
	snapshots := 0
	for _, resource := range view.Resources {
		if resource.SnapshotAt != nil {
			snapshots++
		}
		switch resource.Name {
		case "web":
			if resource.Source != "branch" || resource.Revision != webBranch {
				t.Fatalf("web should run the branch head: %+v", resource)
			}
		case "api":
			if resource.Source != "baseline" || resource.Revision == "" {
				t.Fatalf("api should run the pinned production commit: %+v", resource)
			}
		}
	}
	if snapshots != 2 {
		t.Fatalf("both database snapshot times should be recorded: %s", detail)
	}

	// Production was only read: data, container lifetimes and storage are unchanged.
	if checksum(api) != productionChecksum || maria(shop, "production-maria-password", "CHECKSUM TABLE shop.orders") != productionMaria {
		t.Fatal("production database contents changed")
	}
	for key, value := range productionStarts {
		parts := strings.Split(key, "/")
		workload := map[string]string{"api": api, "shop": shop, "web": web, "cache": cache}[parts[0]]
		if started(workload, parts[1]) != value {
			t.Fatalf("production container %s was restarted", key)
		}
	}
	if strings.Contains(must("exec", container(api, "api"), "ls", "/uploads"), container(copies["api"], "api")[:12]) {
		t.Fatal("the preview wrote into production uploads")
	}
	// The copies match production at the snapshot.
	if checksum(copies["api"]) != productionChecksum {
		t.Fatalf("preview database differs from production: %s vs %s", checksum(copies["api"]), productionChecksum)
	}
	if maria(copies["shop"], "preview-maria-password", "CHECKSUM TABLE shop.orders") != productionMaria {
		t.Fatal("preview MariaDB differs from production")
	}
	if maria(copies["shop"], "preview-maria-password", "SELECT COUNT(*) FROM shop.empty_table") != "0" {
		t.Fatal("empty tables were not copied")
	}
	var report struct {
		Volumes []worker.PreviewVolumeReport  `json:"volumes"`
		Seeded  *worker.PreviewDatabaseReport `json:"seeded"`
	}
	for _, resource := range view.Resources {
		if resource.Name == "api" {
			json.Unmarshal(resource.DataReport, &report)
		}
	}
	copied := ""
	for _, v := range report.Volumes {
		if v.Mode == "copy" {
			copied = v.Digest
		}
	}
	if copied == "" || copied != digest(uploads) {
		t.Fatalf("uploads copy digest %s does not match production %s", copied, digest(uploads))
	}
	if out, _ := docker("exec", container(copies["cache"], "redis"), "redis-cli", "GET", "production-key"); out != "" {
		t.Fatal("cache should start empty")
	}
	if out := must("ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+copies["worker"]); out != "" {
		t.Fatal("background worker should not run in the preview")
	}
	if !strings.Contains(must("exec", container(copies["api"], "api"), "printenv", "STRIPE_KEY"), "sk_test_sandbox") {
		t.Fatal("variable override not applied")
	}
	// No preview container joins a production network or publishes host ports.
	for label, workload := range copies {
		out := must("ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload)
		for _, id := range strings.Fields(out) {
			networks := must("inspect", "--format", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", id)
			for _, n := range strings.Fields(networks) {
				if productionNetworks[n] {
					t.Fatalf("preview %s joined production network %s", label, n)
				}
			}
			if ports := must("inspect", "--format", "{{json .HostConfig.PortBindings}}", id); ports != "{}" && ports != "null" {
				t.Fatalf("preview %s publishes host ports: %s", label, ports)
			}
		}
	}
	productionDB := must("inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", container(api, "db"))
	previewWeb := container(copies["web"], "web")
	for _, address := range strings.Fields(productionDB) {
		if out, err := docker("exec", previewWeb, "python", "-c", "import socket; s=socket.create_connection(('"+address+"', 5432), 3); print('connected')"); err == nil {
			t.Fatalf("preview web reached the production database at %s: %s", address, out)
		}
	}
	if _, err := docker("exec", previewWeb, "python", "-c", "import socket; socket.gethostbyname('"+strings.TrimPrefix(must("inspect", "--format", "{{.Name}}", container(api, "db")), "/")+"')"); err == nil {
		t.Fatal("preview web resolved a production container name")
	}
	// The recreated private connection reaches the preview's own database.
	loop("preview private connection", func() bool {
		_, err := docker("exec", previewWeb, "python", "-c", "import socket; socket.create_connection(('pg.internal', 5432), 3)")
		return err == nil
	})
	// Protected route: refused without credentials, served with them.
	login := map[string]string{}
	json.Unmarshal(call("GET", "/api/environment-previews/"+preview+"/credentials", nil, 200), &login)
	curl := func(args ...string) string {
		out, _ := docker(append([]string{"run", "--rm", "--network", "cove-ingress", "curlimages/curl:8.11.1", "-sk", "-o", "/dev/null", "-w", "%{http_code}", "--connect-to", "web-demo.preview.e2e.test:443:cove-ingress:443", "https://web-demo.preview.e2e.test/"}, args...)...)
		return out
	}
	body := func() string {
		out, _ := docker("run", "--rm", "--network", "cove-ingress", "curlimages/curl:8.11.1", "-sk", "-u", login["username"]+":"+login["password"], "--connect-to", "web-demo.preview.e2e.test:443:cove-ingress:443", "https://web-demo.preview.e2e.test/")
		return out
	}
	if code := curl(); code != "401" {
		t.Fatalf("unauthenticated request returned %s", code)
	}
	if code := curl("-u", login["username"]+":wrong"); code != "401" {
		t.Fatalf("wrong password returned %s", code)
	}
	if out := body(); !strings.Contains(out, "web-branch-1 production") {
		t.Fatalf("protected route served %q", out)
	}

	// Data written in the preview survives a push; only the pushed repository's resource redeploys.
	psql(copies["api"], "INSERT INTO items(name) VALUES('preview-only')")
	apiStarted := started(copies["api"], "api")
	commit("web", "staging/demo", "app.py", webApp("web-branch-2"))
	push("e2e-2", "web", "staging/demo", false)
	loop("web redeploy", func() bool { return strings.Contains(body(), "web-branch-2") })
	if started(copies["api"], "api") != apiStarted {
		t.Fatal("a web push restarted the api")
	}
	if psql(copies["api"], "SELECT count(*) FROM items WHERE name='preview-only'") != "1" {
		t.Fatal("a push reset preview data")
	}
	// A schema migration on the api branch applies to the preview only.
	commit("api", "staging/demo", "start.sh", apiStart("psql -h db -U app -d app -v ON_ERROR_STOP=1 -c 'ALTER TABLE items ADD COLUMN IF NOT EXISTS preview_note text'\n"))
	push("e2e-3", "api", "staging/demo", false)
	loop("api branch deploy", func() bool {
		id, _ := docker("ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+copies["api"], "--filter", "label=com.docker.compose.service=db")
		if id == "" || strings.Contains(id, "\n") {
			return false
		}
		out, _ := docker("exec", id, "psql", "-U", "app", "-d", "app", "-Atc", "SELECT count(*) FROM information_schema.columns WHERE table_name='items' AND column_name='preview_note'")
		return out == "1"
	})
	if psql(api, "SELECT count(*) FROM information_schema.columns WHERE table_name='items' AND column_name='preview_note'") != "0" {
		t.Fatal("the preview migration reached production")
	}
	if psql(copies["api"], "SELECT count(*) FROM items WHERE name='preview-only'") != "1" {
		t.Fatal("the api branch deploy reset preview data")
	}
	// Restarting the worker mid-flight is recovered: redeploy, restart, settle.
	loop("preview ready after the api branch deploy", func() bool {
		var state string
		s.DB.QueryRow(ctx, `SELECT state FROM environment_previews WHERE id=$1`, preview).Scan(&state)
		return state == "ready"
	})
	call("POST", "/api/environment-previews/"+preview+"/redeploy", nil, 202)
	if err := s.reconcileEnvironmentPreviews(ctx); err != nil {
		t.Fatal(err)
	}
	s.dispatchOperations(ctx)
	// Restart only the worker process; its Docker daemon and containers keep running, as on a real node.
	// Separate commands, so the kill pattern can only match the daemon itself.
	hostMust("exec", name, "pkill", "-f", "^/usr/local/bin/cove-worker daemon$")
	for attempt := 0; ; attempt++ {
		if _, err := host("exec", name, "pgrep", "-f", "^/usr/local/bin/cove-worker daemon$"); err != nil {
			break
		}
		if attempt == 100 {
			t.Fatal("the worker daemon did not stop")
		}
		time.Sleep(200 * time.Millisecond)
	}
	hostMust("exec", "-d", name, "su", "-s", "/bin/sh", "cove", "-c", "GIT_SSL_CAINFO=/run/cove-ca/ca.pem exec /usr/local/bin/cove-worker daemon")
	loop("recovery after worker restart", func() bool {
		var state, stage string
		s.DB.QueryRow(ctx, `SELECT state,stage FROM environment_previews WHERE id=$1`, preview).Scan(&state, &stage)
		if state == "failed" {
			f.call("root", "default", "POST", "/api/environment-previews/"+preview+"/retry", nil, 202)
		}
		return state == "ready"
	})
	var environments int
	s.DB.QueryRow(ctx, `SELECT count(*) FROM environments WHERE project_id=$1`, project).Scan(&environments)
	if environments != 2 || psql(copies["api"], "SELECT count(*) FROM items WHERE name='preview-only'") != "1" {
		t.Fatal("recovery duplicated the environment or reset data")
	}
	// Deleting the web branch returns web to the production commit; the preview stays.
	deleteBranch("web", "staging/demo")
	push("e2e-4", "web", "staging/demo", true)
	loop("web baseline fallback", func() bool { return strings.Contains(body(), "web-main production") })
	loop("preview ready after the fallback", func() bool {
		var state string
		s.DB.QueryRow(ctx, `SELECT state FROM environment_previews WHERE id=$1`, preview).Scan(&state)
		return state == "ready"
	})
	// Refreshing from production replaces the preview database after confirmation.
	call("POST", "/api/environment-previews/"+preview+"/refresh", map[string]string{"confirm": view.Slug}, 202)
	loop("data refresh", func() bool {
		var state string
		s.DB.QueryRow(ctx, `SELECT state FROM environment_previews WHERE id=$1`, preview).Scan(&state)
		if state != "ready" {
			return false
		}
		id, _ := docker("ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+copies["api"], "--filter", "label=com.docker.compose.service=db")
		if id == "" || strings.Contains(id, "\n") {
			return false
		}
		out, _ := docker("exec", id, "psql", "-U", "app", "-d", "app", "-Atc", "SELECT count(*) FROM items WHERE name='preview-only'")
		return out == "0"
	})
	if checksum(copies["api"]) != productionChecksum || checksum(api) != productionChecksum {
		t.Fatal("refresh did not restore the production copy, or changed production")
	}
	// Deleting the last branch removes the preview and everything it owns; production keeps running.
	deleteBranch("api", "staging/demo")
	push("e2e-5", "api", "staging/demo", true)
	loop("preview removal", func() bool {
		var exists bool
		s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environment_previews WHERE id=$1)`, preview).Scan(&exists)
		return !exists
	})
	for label, workload := range copies {
		if out := must("ps", "-aq", "--filter", "label=com.docker.compose.project=cove-"+workload); out != "" {
			t.Fatalf("preview %s containers remain", label)
		}
		if out := must("volume", "ls", "-q", "--filter", "label=dev.cove.environment-preview="+workload); out != "" {
			t.Fatalf("preview %s volumes remain: %s", label, out)
		}
	}
	if checksum(api) != productionChecksum || digest(uploads) != copied {
		t.Fatal("production data changed during the preview's lifetime")
	}
	for key, value := range productionStarts {
		parts := strings.Split(key, "/")
		workload := map[string]string{"api": api, "shop": shop, "web": web, "cache": cache}[parts[0]]
		if started(workload, parts[1]) != value {
			t.Fatalf("production container %s was restarted", key)
		}
	}
	t.Logf("installation tokens requested: %d", installationRequests)
}

func e2eCertificate(t *testing.T, host string) (tls.Certificate, []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Cove E2E CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	caCert, _ := x509.ParseCertificate(caDER)
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	return certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func e2eRSAKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}
