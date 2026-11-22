package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// previewFixture simulates GitHub and the worker so the control-plane workflow can be exercised
// end to end against PostgreSQL. Docker execution is covered by the end-to-end acceptance test.
type previewFixture struct {
	*accountFixture
	t          *testing.T
	mu         sync.Mutex
	branches   map[string]string
	submitted  []worker.Request
	rpcs       []worker.RPCRequest
	failData   map[string]bool
	failDeploy map[string]bool
	ids        map[string]string
	images     map[string]map[string]string
}

const fixtureWebhookSecret = "environment-preview-webhook"

func sha(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:20])
}

func imageID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newPreviewFixture(t *testing.T) *previewFixture {
	f := &previewFixture{accountFixture: newAccountFixture(t), t: t, branches: map[string]string{}, failData: map[string]bool{}, failDeploy: map[string]bool{}, ids: map[string]string{}, images: map[string]map[string]string{}}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := f.s.sealJSON(githubAppCredentials{PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), WebhookSecret: fixtureWebhookSecret})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO github_app(id,app_id,slug,credentials) VALUES(1,77,'cove-fixture',$1)`, credentials)
	f.sql(`INSERT INTO github_installations(id,account) VALUES(41,'acme'),(42,'stranger')`)
	f.sql(`INSERT INTO github_installation_teams VALUES(41,'default')`)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('n1','Node','10.0.0.5',22,'',$1,'ready','{"environment_previews":true,"private_links":true,"shared_networks":true,"live_routes":true,"exact_promotion":true}')`, []byte{})
	f.sql(`INSERT INTO team_nodes VALUES('default','n1')`)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('p','Shop','default')`)
	f.sql(`INSERT INTO environments(id,project_id,team_id,name) VALUES('prod','p','default','Production'),('dev','p','default','Development')`)
	f.s.OAuthClient = &http.Client{Transport: oauthTransport(f.github)}
	f.s.workerRPC = f.worker
	return f
}

func (f *previewFixture) github(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(status int, value any) (*http.Response, error) {
		body, _ := json.Marshal(value)
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{"Content-Type": {"application/json"}}}, nil
	}
	path := r.URL.EscapedPath()
	switch {
	case strings.HasPrefix(path, "/app/installations/") && strings.HasSuffix(path, "/access_tokens"):
		if path != "/app/installations/41/access_tokens" {
			return reply(404, map[string]string{})
		}
		return reply(201, map[string]string{"token": "installation-token"})
	case strings.Contains(path, "/git/ref/heads/"):
		var repository int64
		var branch string
		fmt.Sscanf(path, "/repositories/%d/git/ref/heads/", &repository)
		branch = path[strings.Index(path, "/heads/")+7:]
		head, ok := f.branches[fmt.Sprintf("%d:%s", repository, branch)]
		if !ok {
			return reply(404, map[string]string{"message": "Not Found"})
		}
		return reply(200, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"sha": head}})
	case strings.Contains(path, "/git/matching-refs/heads/"):
		var repository int64
		fmt.Sscanf(path, "/repositories/%d/git/matching-refs/heads/", &repository)
		prefix := path[strings.Index(path, "/heads/")+7:]
		refs := []map[string]string{}
		for key, head := range f.branches {
			repo, branch, _ := strings.Cut(key, ":")
			if repo == fmt.Sprint(repository) && strings.HasPrefix(branch, prefix) {
				refs = append(refs, map[string]string{"ref": "refs/heads/" + branch, "sha": head})
			}
		}
		return reply(200, refs)
	}
	f.t.Errorf("unexpected GitHub request %s %s", r.Method, path)
	return reply(500, map[string]string{})
}

func (f *previewFixture) worker(_ context.Context, node string, request worker.RPCRequest) (worker.RPCResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rpcs = append(f.rpcs, request)
	switch request.Method {
	case "environment_preview_inventory":
		return worker.RPCResponse{Inventory: f.inventory(request.ID)}, nil
	case "export_release":
		images := f.images[request.Promotion.Workload]
		if len(images) == 0 {
			return worker.RPCResponse{}, &RemoteError{Message: "release unavailable"}
		}
		return worker.RPCResponse{Images: images}, nil
	case "submit":
		r := *request.Request
		f.submitted = append(f.submitted, r)
		state := "succeeded"
		operation := &worker.Operation{ID: r.ID, Workload: r.Workload, Action: r.Action, State: state}
		if r.Action == "preview_data" {
			if f.failData[r.Workload] {
				operation.State = "failed"
				delete(f.failData, r.Workload)
			} else {
				report := &worker.PreviewReport{}
				for _, v := range r.EnvironmentPreview.Volumes {
					report.Volumes = append(report.Volumes, worker.PreviewVolumeReport{Target: v.Target, Mode: v.Mode, Files: 3, Bytes: 42, Digest: "sha256:fixture"})
				}
				if r.EnvironmentPreview.Database != nil {
					report.Database = &worker.PreviewDatabaseReport{Engine: "postgresql", SnapshotAt: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC), DumpBytes: 1024}
				}
				operation.Preview = report
			}
		}
		if r.Action == "deploy" && f.failDeploy[r.Workload] {
			operation.State = "failed"
			delete(f.failDeploy, r.Workload)
		}
		if r.Action == "deploy" && operation.State == "succeeded" && r.EnvironmentPreview != nil && r.EnvironmentPreview.Database != nil {
			operation.Preview = &worker.PreviewReport{Seeded: &worker.PreviewDatabaseReport{Engine: "postgresql", Tables: 2, Rows: 7, Verified: true}}
		}
		return worker.RPCResponse{Operation: operation}, nil
	case "status":
		return worker.RPCResponse{Operation: &worker.Operation{ID: request.ID, State: "succeeded"}}, nil
	}
	return worker.RPCResponse{}, nil
}

func (f *previewFixture) inventory(id string) *worker.PreviewInventory {
	switch id {
	case f.ids["web"]:
		return &worker.PreviewInventory{Services: []worker.PreviewInventoryService{{Name: "web", Image: imageID("web"), Immutable: true, Ports: true}}}
	case f.ids["api"]:
		return &worker.PreviewInventory{Services: []worker.PreviewInventoryService{{Name: "api", Image: imageID("api"), Immutable: true, DependsOn: []string{"db"}}, {Name: "db", Image: imageID("db"), Tags: []string{"postgres:17"}, Immutable: true}},
			Volumes:  []worker.PreviewInventoryVolume{{Key: "volume:pgdata", Type: "volume", Source: "cove-" + id + "_pgdata", Services: []string{"db"}}, {Key: "volume:uploads", Type: "volume", Source: "cove-" + id + "_uploads", Services: []string{"api"}}},
			Database: &worker.PreviewInventoryDatabase{Engine: "postgresql", Service: "db", Name: "app", Supported: true, Bytes: 4096}}
	case f.ids["worker"]:
		return &worker.PreviewInventory{Services: []worker.PreviewInventoryService{{Name: "worker", Image: imageID("worker"), Immutable: true}}}
	case f.ids["cache"]:
		return &worker.PreviewInventory{Services: []worker.PreviewInventoryService{{Name: "redis", Image: imageID("redis"), Tags: []string{"redis:7"}, Immutable: true}}, Volumes: []worker.PreviewInventoryVolume{{Key: "volume:redisdata", Type: "volume", Source: "cove-" + id + "_redisdata", Services: []string{"redis"}}}}
	case f.ids["cleanup"]:
		return &worker.PreviewInventory{Services: []worker.PreviewInventoryService{{Name: "cleanup", Image: "busybox:1.36"}}}
	}
	return &worker.PreviewInventory{}
}

// resource creates a deployed production resource with a successful deploy operation.
func (f *previewFixture) resource(name string, config workloadInput, repository int64, deployedRevision string) string {
	f.t.Helper()
	config.Name, config.NodeID = name, "n1"
	if config.Kind == "" {
		config.Kind = "service"
	}
	encrypted, err := f.s.sealJSON(config)
	if err != nil {
		f.t.Fatal(err)
	}
	id := resourceID()
	f.ids[name] = id
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,kind,environment_id,team_id,project_id) VALUES($1,'n1',$2,$3,$4,'prod','default','p')`, id, name, encrypted, config.Kind)
	if repository != 0 {
		f.sql(`INSERT INTO github_sources(workload_id,installation_id,repository_id,repository) VALUES($1,41,$2,$3)`, id, repository, config.Source.Repository)
	}
	request := worker.Request{ID: resourceID(), Workload: id, Action: "deploy", Source: config.Source, Compose: config.Compose, Environment: config.Environment, Kind: config.Kind, JobService: config.JobService}
	payload, err := f.s.sealJSON(request)
	if err != nil {
		f.t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]string{"revision": deployedRevision})
	f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES($1,$2,'deploy','succeeded',$3,$4)`, request.ID, id, payload, result)
	if config.Kind != "job" {
		f.images[id] = map[string]string{}
		for _, service := range f.inventory(id).Services {
			f.images[id][service.Name] = service.Image
		}
	}
	return id
}

func (f *previewFixture) push(delivery string, repository int64, branch string, deleted bool, installation int64) int {
	body, _ := json.Marshal(map[string]any{"ref": "refs/heads/" + branch, "after": sha(branch + delivery), "deleted": deleted, "installation": map[string]int64{"id": installation}, "repository": map[string]any{"id": repository, "fork": false}})
	request := httptest.NewRequest("POST", "/hooks/github-app", bytes.NewReader(body))
	mac := hmac.New(sha256.New, []byte(fixtureWebhookSecret))
	mac.Write(body)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", delivery)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response.Code
}

// settle runs the reconciler and the operation dispatcher until nothing changes.
func (f *previewFixture) settle() {
	f.t.Helper()
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		f.sql(`UPDATE environment_previews SET next_attempt_at=now()`)
		f.sql(`UPDATE preview_branch_signals SET next_attempt_at=now() WHERE reason NOT LIKE 'Waiting:%'`)
		if err := f.s.reconcileEnvironmentPreviews(ctx); err != nil {
			f.t.Fatal(err)
		}
		if err := f.s.dispatchOperations(ctx); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *previewFixture) previewState(id string) (string, string, string) {
	var state, stage, detail string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT state,stage,detail FROM environment_previews WHERE id=$1`, id).Scan(&state, &stage, &detail); err != nil {
		return "", "", err.Error()
	}
	return state, stage, detail
}

func (f *previewFixture) submittedFor(workload, action string) []worker.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := []worker.Request{}
	for _, r := range f.submitted {
		if r.Workload == workload && r.Action == action {
			list = append(list, r)
		}
	}
	return list
}

func (f *previewFixture) copyOf(preview, base string) string {
	var id string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT resource_id FROM environment_preview_resources WHERE preview_id=$1 AND base_id=$2`, preview, base).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *previewFixture) setup() previewRecipe {
	web := f.resource("web", workloadInput{Source: &worker.GitSource{Repository: "https://github.com/acme/web.git", Branch: "main", ComposePath: "compose.yaml"}, Environment: map[string]string{"API_URL": "http://api.internal:8080", "SESSION_SECRET": "production-session", "NODE_ENV": "production"}, Routes: []worker.Route{{Domain: "shop.example.com", Service: "web", Port: 80}}}, 101, sha("web-prod"))
	api := f.resource("api", workloadInput{Source: &worker.GitSource{Repository: "https://github.com/acme/api.git", Branch: "main", ComposePath: "compose.yaml"}, Environment: map[string]string{"DATABASE_URL": "postgres://app:secret@db:5432/app", "POSTGRES_PASSWORD": "secret", "STRIPE_KEY": "sk_live_production", "CACHE_URL": "redis://cache:6379"}, Backup: &worker.BackupConfig{Repository: "/backups", Password: "backup-password", Database: &worker.DatabaseBackup{Engine: "postgresql", Service: "db", Name: "app"}}}, 102, sha("api-prod"))
	f.resource("worker", workloadInput{Source: &worker.GitSource{Repository: "https://github.com/acme/worker.git", Branch: "main", ComposePath: "compose.yaml"}, Environment: map[string]string{"QUEUE": "default"}}, 103, sha("worker-prod"))
	cache := f.resource("cache", workloadInput{Compose: "services:\n  redis:\n    image: redis:7\n    volumes: [redisdata:/data]\nvolumes:\n  redisdata: {}\n"}, 0, "")
	f.resource("cleanup", workloadInput{Kind: "job", JobService: "cleanup", Compose: "services:\n  cleanup:\n    image: busybox:1.36\n"}, 0, "")
	f.sql(`UPDATE workloads SET job_cron='0 * * * *' WHERE id=$1`, f.ids["cleanup"])
	f.sql(`INSERT INTO resource_connections(id,team_id,source_id,source_service,target_id,target_service,port,alias) VALUES('c1','default',$1,'web',$2,'api',8080,'api.internal')`, web, api)
	f.sql(`INSERT INTO shared_networks(id,team_id,node_id,name) VALUES($1,'default','n1','backend')`, strings.Repeat("b", 32))
	f.sql(`INSERT INTO workload_networks(workload_id,network_id,service,alias) VALUES($1,$3,'api','api'),($2,$3,'redis','cache')`, api, cache, strings.Repeat("b", 32))
	return previewRecipe{Enabled: true, BaseEnvironment: "prod", BranchPattern: "staging/*", Repositories: []int64{101, 102}, DomainTemplate: "{resource}-{preview}.preview.example.com", Access: "private", TTLHours: 72, MaxPreviews: 2, Resources: map[string]previewResourcePolicy{
		web:              {Variables: map[string]previewVariable{"SESSION_SECRET": {Mode: "override", Value: "preview-session"}}},
		api:              {Variables: map[string]previewVariable{"STRIPE_KEY": {Mode: "override", Value: "sk_test_sandbox"}, "POSTGRES_PASSWORD": {Mode: "copy"}}},
		f.ids["cleanup"]: {ImageTags: true},
		f.ids["worker"]:  {},
	}}
}

func TestEnvironmentPreviewPlanAndRecipe(t *testing.T) {
	f := newPreviewFixture(t)
	recipe := f.setup()
	unsafe := recipe
	unsafe.Resources = map[string]previewResourcePolicy{}
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", unsafe, 200)
	response := f.call("root", "default", "GET", "/api/projects/p/preview-recipe", nil, 200)
	var view struct {
		Recipe previewRecipe `json:"recipe"`
		Plan   previewPlan   `json:"plan"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Plan.Blocked {
		t.Fatal("credentials without decisions must block previews")
	}
	codes := map[string]map[string]bool{}
	for _, item := range view.Plan.Resources {
		codes[item.Name] = map[string]bool{}
		for _, b := range item.Blockers {
			codes[item.Name][b.Code+":"+b.Key] = true
		}
	}
	for _, want := range []string{"web:variable_decision:SESSION_SECRET", "api:variable_decision:STRIPE_KEY", "api:variable_decision:POSTGRES_PASSWORD", "cleanup:unpinned_image:image_tags"} {
		name, key, _ := strings.Cut(want, ":")
		if !codes[name][key] {
			t.Errorf("missing blocker %s in %v", want, codes)
		}
	}
	for _, unexpected := range []string{"web:variable_decision:API_URL", "api:variable_decision:DATABASE_URL", "api:variable_decision:CACHE_URL", "web:variable_decision:NODE_ENV"} {
		name, key, _ := strings.Cut(unexpected, ":")
		if codes[name][key] {
			t.Errorf("internal or plain variable %s should not need a decision", unexpected)
		}
	}
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", recipe, 200)
	response = f.call("root", "default", "GET", "/api/projects/p/preview-recipe", nil, 200)
	if strings.Contains(response.Body.String(), "sk_test_sandbox") || strings.Contains(response.Body.String(), "preview-session") || strings.Contains(response.Body.String(), "sk_live_production") {
		t.Fatal("recipe response leaked a variable value")
	}
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Plan.Blocked {
		t.Fatalf("resolved recipe still blocked: %s", response.Body.String())
	}
	for _, item := range view.Plan.Resources {
		switch item.Name {
		case "worker":
			if item.Services[0].Run {
				t.Error("a service nothing connects to defaults to disabled")
			}
		case "cache":
			if item.Volumes[0].Mode != "empty" {
				t.Error("cache volumes default to empty")
			}
		case "api":
			modes := map[string]string{}
			for _, v := range item.Volumes {
				modes[v.Key] = v.Mode
			}
			if modes["volume:pgdata"] != "database" || modes["volume:uploads"] != "copy" {
				t.Errorf("unexpected api volume modes %v", modes)
			}
			if !item.Services[1].Run {
				t.Error("database service must run")
			}
		}
	}
	if len(view.Plan.Hostnames) == 0 || view.Plan.Hostnames[0] != "web-example.preview.example.com" {
		t.Errorf("unexpected example hostnames %v", view.Plan.Hostnames)
	}
	// Keeping a saved override without resending it.
	kept := recipe
	kept.Resources = map[string]previewResourcePolicy{}
	for id, policy := range recipe.Resources {
		copied := policy
		copied.Variables = map[string]previewVariable{}
		for key, v := range policy.Variables {
			v.Value = ""
			copied.Variables[key] = v
		}
		kept.Resources[id] = copied
	}
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", kept, 200)
	saved, _, _, err := f.s.loadPreviewRecipe(context.Background(), f.s.DB, "p")
	if err != nil || saved.Resources[f.ids["api"]].Variables["STRIPE_KEY"].Value != "sk_test_sandbox" {
		t.Fatal("override value was not kept")
	}
	invalid := recipe
	invalid.BranchPattern = "staging/*/x"
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", invalid, 400)
	invalid = recipe
	invalid.Repositories = []int64{999}
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", invalid, 400)
	invalid = recipe
	invalid.DomainTemplate = "{preview}.preview.example.com"
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", invalid, 400)
	// Another team's Cloudflare zone cannot be claimed through a template.
	f.sql(`INSERT INTO teams(id,name) VALUES('other','Other')`)
	zone, _ := f.s.sealJSON(cloudflareZone{ZoneID: "zone1", Domain: "other.example", Token: "t", Enabled: true, Nodes: map[string]string{}})
	f.sql(`INSERT INTO cloudflare_zones(zone_id,domain,configuration) VALUES('zone1','other.example',$1)`, zone)
	f.sql(`INSERT INTO cloudflare_zone_teams(zone_id,team_id,domains) VALUES('zone1','other',ARRAY['other.example','*.other.example'])`)
	invalid = recipe
	invalid.DomainTemplate = "{resource}-{preview}.other.example"
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", invalid, 403)
}

func TestEnvironmentPreviewWorkflow(t *testing.T) {
	f := newPreviewFixture(t)
	recipe := f.setup()
	recipe.RequireApproval = true
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", recipe, 200)
	production := map[string]bool{}
	for _, id := range f.ids {
		production[id] = true
	}
	// An installation not shared with the team is ignored, and so are unrelated branches.
	if code := f.push("d-foreign", 101, "staging/checkout", false, 42); code != 202 {
		t.Fatalf("push returned %d", code)
	}
	if code := f.push("d-main", 101, "main", false, 41); code != 202 {
		t.Fatalf("push returned %d", code)
	}
	f.settle()
	var count int
	f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM environment_previews`).Scan(&count)
	if count != 0 {
		t.Fatal("previews were created for an unshared installation or unmatched branch")
	}
	// The branch now exists; the periodic scan or the webhook may discover it first.
	f.branches["101:staging/checkout"] = sha("web-1")
	if code := f.push("d-1", 101, "staging/checkout", false, 41); code != 202 {
		t.Fatalf("push returned %d", code)
	}
	// Replaying the same delivery does not create a second signal.
	f.push("d-1", 101, "staging/checkout", false, 41)
	f.settle()
	var preview string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT id FROM environment_previews WHERE suffix='checkout'`).Scan(&preview); err != nil {
		t.Fatal(err)
	}
	if state, stage, _ := f.previewState(preview); state != "awaiting_approval" || stage != "approval" {
		t.Fatalf("preview should wait for approval, got %s/%s", state, stage)
	}
	viewer := f.user("viewer", "viewer@example.com")
	stranger := f.user("stranger", "stranger@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','viewer','member','[{"scope":"environment","scope_id":"prod","role":"viewer"}]'),('default','stranger','member','[{"scope":"environment","scope_id":"dev","role":"editor"}]')`)
	f.call(viewer, "default", "POST", "/api/environment-previews/"+preview+"/approve", nil, 403)
	f.call(stranger, "default", "GET", "/api/environment-previews/"+preview, nil, 403)
	f.call("root", "default", "POST", "/api/environment-previews/"+preview+"/approve", nil, 202)
	f.settle()
	state, stage, detail := f.previewState(preview)
	if state != "ready" || stage != "ready" {
		t.Fatalf("preview did not become ready: %s/%s %s", state, stage, detail)
	}
	web, api, worker_, cache, cleanup := f.copyOf(preview, f.ids["web"]), f.copyOf(preview, f.ids["api"]), f.copyOf(preview, f.ids["worker"]), f.copyOf(preview, f.ids["cache"]), f.copyOf(preview, f.ids["cleanup"])

	// Nothing was ever submitted to a production resource; production was only inventoried and exported.
	for _, r := range f.submitted {
		if production[r.Workload] {
			t.Fatalf("operation %s was submitted to production resource %s", r.Action, r.Workload)
		}
	}
	for _, rpc := range f.rpcs {
		if rpc.Method != "environment_preview_inventory" && rpc.Method != "export_release" && rpc.Method != "submit" && rpc.Method != "status" {
			t.Fatalf("unexpected worker call %s", rpc.Method)
		}
	}
	data := f.submittedFor(api, "preview_data")
	if len(data) != 1 || data[0].EnvironmentPreview.Database == nil || data[0].EnvironmentPreview.Source != f.ids["api"] {
		t.Fatalf("api data copy not requested correctly: %+v", data)
	}
	for _, v := range data[0].EnvironmentPreview.Volumes {
		if !strings.HasPrefix(v.Target, "cove-pv-"+api+"-") {
			t.Fatalf("volume target %s is not preview-owned", v.Target)
		}
	}
	webDeploys := f.submittedFor(web, "deploy")
	apiDeploys := f.submittedFor(api, "deploy")
	if len(webDeploys) != 1 || webDeploys[0].Source.Branch != "staging/checkout" || webDeploys[0].Source.Revision != sha("web-1") || len(webDeploys[0].ImagePins) != 0 {
		t.Fatalf("web should deploy the matching branch: %+v", webDeploys)
	}
	if len(apiDeploys) != 1 || apiDeploys[0].Source.Branch != "main" || apiDeploys[0].Source.Revision != sha("api-prod") || apiDeploys[0].ImagePins["api"] != imageID("api") {
		t.Fatalf("api should deploy the pinned production baseline: %+v", apiDeploys)
	}
	if webDeploys[0].Environment["SESSION_SECRET"] != "preview-session" || apiDeploys[0].Environment["STRIPE_KEY"] != "sk_test_sandbox" || apiDeploys[0].Environment["POSTGRES_PASSWORD"] != "secret" {
		t.Fatal("variable policies were not applied")
	}
	if len(webDeploys[0].Routes) != 0 {
		t.Fatal("routes must not be published before every resource is healthy")
	}
	if workerDeploys := f.submittedFor(worker_, "deploy"); len(workerDeploys) != 1 || len(workerDeploys[0].EnvironmentPreview.Disabled) != 1 {
		t.Fatalf("background service should deploy disabled: %+v", workerDeploys)
	}
	if len(f.submittedFor(cleanup, "deploy")) != 0 {
		t.Fatal("jobs must not run in previews unless scheduled")
	}
	var cron string
	f.s.DB.QueryRow(context.Background(), `SELECT job_cron FROM workloads WHERE id=$1`, cleanup).Scan(&cron)
	if cron != "" {
		t.Fatal("job schedules are disabled in previews by default")
	}
	routes := f.submittedFor(web, "routes")
	if len(routes) != 1 || routes[0].Routes[0].Domain != "web-checkout.preview.example.com" || routes[0].Routes[0].Auth == "" {
		t.Fatalf("protected route not published: %+v", routes)
	}
	credentials := f.call("root", "default", "GET", "/api/environment-previews/"+preview+"/credentials", nil, 200)
	var login struct{ Username, Password string }
	json.Unmarshal(credentials.Body.Bytes(), &login)
	_, hash, _ := strings.Cut(routes[0].Routes[0].Auth, ":")
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(login.Password)) != nil {
		t.Fatal("credentials do not match the protected route")
	}
	f.call(viewer, "default", "GET", "/api/environment-previews/"+preview+"/credentials", nil, 403)
	detailResponse := f.call(viewer, "default", "GET", "/api/environment-previews/"+preview, nil, 200)
	if strings.Contains(detailResponse.Body.String(), login.Password) || strings.Contains(detailResponse.Body.String(), "$2a$") {
		t.Fatal("preview detail leaked credentials")
	}
	resource := f.call("root", "default", "GET", "/api/workloads/"+web, nil, 200)
	if strings.Contains(resource.Body.String(), "$2a$") || strings.Contains(resource.Body.String(), "preview-session") {
		t.Fatal("resource view leaked route credentials or variable values")
	}
	// The graph shows the preview's own copies connected to each other.
	var connections int
	f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM resource_connections WHERE source_id=$1 AND target_id=$2 AND alias='api.internal'`, web, api).Scan(&connections)
	if connections != 1 {
		t.Fatal("private connection was not recreated between preview copies")
	}
	var crossings int
	f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM resource_connections c WHERE (c.source_id IN ($1,$2,$3) AND c.target_id NOT IN ($1,$2,$3)) OR (c.target_id IN ($1,$2,$3) AND c.source_id NOT IN ($1,$2,$3))`, web, api, cache).Scan(&crossings)
	if crossings != 0 {
		t.Fatal("a preview resource is connected to a production resource")
	}
	if networks := apiDeploys[0].Networks; len(networks) != 1 || networks[0].ID == strings.Repeat("b", 32) || networks[0].Alias != "api" {
		t.Fatalf("api must join the preview's own copy of the shared network: %+v", networks)
	}
	var previewEnvironment string
	f.s.DB.QueryRow(context.Background(), `SELECT environment_id FROM environment_previews WHERE id=$1`, preview).Scan(&previewEnvironment)
	graph := f.call(viewer, "default", "GET", "/api/projects/p/connections?environment="+previewEnvironment, nil, 200)
	var topology struct {
		Resources []struct {
			ID            string `json:"id"`
			EnvironmentID string `json:"environment_id"`
		} `json:"resources"`
		Connections []struct {
			Source struct {
				ID string `json:"id"`
			} `json:"source"`
			Target struct {
				ID string `json:"id"`
			} `json:"target"`
			CanEdit bool `json:"can_edit"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(graph.Body.Bytes(), &topology); err != nil {
		t.Fatal(err)
	}
	previewEdge := false
	for _, c := range topology.Connections {
		previewEdge = previewEdge || (c.Source.ID == web && c.Target.ID == api)
	}
	inPreview := 0
	for _, r := range topology.Resources {
		if r.EnvironmentID == previewEnvironment {
			inPreview++
		}
	}
	if !previewEdge || inPreview != 5 {
		t.Fatalf("the preview graph must show its own resources and connections: %s", graph.Body.String())
	}
	// Managed resources and environments refuse manual changes.
	f.call("root", "default", "PUT", "/api/workloads/"+web, workloadInput{Name: "x", NodeID: "n1", Compose: "services: {}"}, 409)
	f.call("root", "default", "POST", "/api/workloads/"+web+"/operations", map[string]string{"action": "deploy"}, 409)
	f.call("root", "default", "POST", "/api/workloads/"+web+"/operations", map[string]string{"action": "restart"}, 202)
	f.settle()
	if restarts := f.submittedFor(web, "restart"); len(restarts) != 1 || restarts[0].EnvironmentPreview == nil {
		t.Fatal("manual operations on preview resources must carry their isolation")
	}
	var environment string
	f.s.DB.QueryRow(context.Background(), `SELECT environment_id FROM environment_previews WHERE id=$1`, preview).Scan(&environment)
	f.call("root", "default", "DELETE", "/api/environments/"+environment, nil, 409)
	f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "x", NodeID: "n1", Compose: "services:\n  a:\n    image: busybox\n", EnvironmentID: environment}, 409)
	f.call("root", "default", "POST", "/api/private-connections", map[string]any{"source_id": web, "source_service": "web", "target_id": f.ids["api"], "target_service": "api", "port": 8080, "alias": "prod.internal"}, 409)

	// A push to the web branch redeploys only web; the database is not copied again.
	f.branches["101:staging/checkout"] = sha("web-2")
	f.push("d-2", 101, "staging/checkout", false, 41)
	f.settle()
	if len(f.submittedFor(web, "deploy")) != 2 || len(f.submittedFor(api, "deploy")) != 1 || len(f.submittedFor(api, "preview_data")) != 1 {
		t.Fatal("a push must redeploy only the affected resources and never reset data")
	}
	if latest := f.submittedFor(web, "deploy")[1]; latest.Source.Revision != sha("web-2") || len(latest.Routes) != 1 {
		t.Fatalf("redeploy must use the new head and keep published routes: %+v", latest)
	}
	// A stale delivery cannot move the preview back: the head is read from GitHub.
	f.push("d-0-late", 101, "staging/checkout", false, 41)
	f.settle()
	if len(f.submittedFor(web, "deploy")) != 2 {
		t.Fatal("a stale delivery redeployed the preview")
	}
	// Production pushes do not advance the pinned baseline.
	f.sql(`UPDATE operations SET result='{"revision":"`+sha("api-new-prod")+`"}' WHERE workload_id=$1`, f.ids["api"])
	// The api branch appears: only api moves to it.
	f.branches["102:staging/checkout"] = sha("api-1")
	f.push("d-3", 102, "staging/checkout", false, 41)
	f.settle()
	apiDeploys = f.submittedFor(api, "deploy")
	if len(apiDeploys) != 2 || apiDeploys[1].Source.Revision != sha("api-1") || len(apiDeploys[1].ImagePins) != 0 || len(f.submittedFor(web, "deploy")) != 2 {
		t.Fatalf("a new matching branch must update only its resources: %+v", apiDeploys)
	}
	// Deleting the web branch returns web to the pinned production baseline; the preview stays.
	delete(f.branches, "101:staging/checkout")
	f.push("d-4", 101, "staging/checkout", true, 41)
	f.settle()
	webDeploys = f.submittedFor(web, "deploy")
	if state, _, _ := f.previewState(preview); state != "ready" || len(webDeploys) != 3 || webDeploys[2].Source.Branch != "main" || webDeploys[2].Source.Revision != sha("web-prod") || webDeploys[2].ImagePins["web"] != imageID("web") {
		t.Fatalf("deleted branch must fall back to the baseline pinned at creation: %+v", webDeploys)
	}
	// The api baseline stayed pinned at creation even though production moved on.
	var baseline string
	f.s.DB.QueryRow(context.Background(), `SELECT baseline_revision FROM environment_preview_resources WHERE resource_id=$1`, api).Scan(&baseline)
	if baseline != sha("api-prod") {
		t.Fatal("the pinned baseline changed after a production deployment")
	}
	// Refreshing data requires confirmation and redeploys from a new snapshot.
	var slug string
	f.s.DB.QueryRow(context.Background(), `SELECT slug FROM environment_previews WHERE id=$1`, preview).Scan(&slug)
	f.call("root", "default", "POST", "/api/environment-previews/"+preview+"/refresh", map[string]string{"confirm": "wrong"}, 400)
	f.call(viewer, "default", "POST", "/api/environment-previews/"+preview+"/refresh", map[string]string{"confirm": slug}, 403)
	f.call("root", "default", "POST", "/api/environment-previews/"+preview+"/refresh", map[string]string{"confirm": slug}, 202)
	f.settle()
	refresh := f.submittedFor(api, "preview_data")
	if len(refresh) != 2 || !refresh[1].EnvironmentPreview.Refresh || len(f.submittedFor(api, "deploy")) != 3 {
		t.Fatal("refresh must take a new snapshot and redeploy")
	}
	if state, _, _ := f.previewState(preview); state != "ready" {
		t.Fatal("preview not ready after refresh")
	}
	// Deleting the last branch removes the preview and every record it owns.
	delete(f.branches, "102:staging/checkout")
	f.push("d-5", 102, "staging/checkout", true, 41)
	f.settle()
	if state, _, _ := f.previewState(preview); state != "" {
		t.Fatalf("preview still exists in state %s", state)
	}
	for _, copy := range []string{web, api, worker_, cache, cleanup} {
		if removals := f.submittedFor(copy, "environment_preview_remove"); len(removals) != 1 {
			t.Fatalf("resource %s was not removed from its node", copy)
		}
		var exists bool
		f.s.DB.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM workloads WHERE id=$1)`, copy).Scan(&exists)
		if exists {
			t.Fatal("preview resource record remains")
		}
	}
	var leftovers int
	f.s.DB.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM environments WHERE id=$1)+(SELECT count(*) FROM shared_networks WHERE name LIKE '%preview%')+(SELECT count(*) FROM resource_connections WHERE id<>'c1')`, environment).Scan(&leftovers)
	if leftovers != 0 {
		t.Fatal("preview environment, networks or connections remain")
	}
	for id := range production {
		var exists bool
		f.s.DB.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM workloads WHERE id=$1)`, id).Scan(&exists)
		if !exists {
			t.Fatal("a production resource was removed")
		}
	}
}

func TestEnvironmentPreviewFailuresLimitsAndCleanup(t *testing.T) {
	f := newPreviewFixture(t)
	recipe := f.setup()
	recipe.MaxPreviews = 1
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", recipe, 200)
	f.branches["101:staging/one"] = sha("one")
	f.branches["101:staging/two"] = sha("two")
	f.push("e-1", 101, "staging/one", false, 41)
	f.settle()
	var first string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT id FROM environment_previews WHERE suffix='one'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	// The limit holds the second branch back and says why.
	f.push("e-2", 101, "staging/two", false, 41)
	f.settle()
	list := f.call("root", "default", "GET", "/api/projects/p/environment-previews", nil, 200)
	if !strings.Contains(list.Body.String(), "1 of 1 previews are in use") {
		t.Fatalf("waiting branch not reported: %s", list.Body.String())
	}
	// A failed data copy fails the preview without duplicates; retry copies only what failed.
	if state, _, _ := f.previewState(first); state != "ready" {
		t.Fatal("first preview not ready")
	}
	f.call("root", "default", "POST", "/api/environment-previews/"+first+"/retry", nil, 409)
	// Inactivity cleanup removes an unpinned preview; pinning keeps it.
	f.call("root", "default", "PUT", "/api/environment-previews/"+first, map[string]bool{"pinned": true}, 200)
	f.sql(`UPDATE environment_previews SET last_activity_at=now()-interval '100 hours' WHERE id=$1`, first)
	f.settle()
	if state, _, _ := f.previewState(first); state != "ready" {
		t.Fatal("a pinned preview was cleaned up")
	}
	f.call("root", "default", "PUT", "/api/environment-previews/"+first, map[string]bool{"pinned": false}, 200)
	f.sql(`UPDATE environment_previews SET last_activity_at=now()-interval '100 hours' WHERE id=$1`, first)
	f.settle()
	if state, _, _ := f.previewState(first); state != "" {
		t.Fatalf("inactive preview remains in %s", state)
	}
	// The freed slot lets the waiting branch through. Its first data copy fails.
	f.sql(`UPDATE preview_branch_signals SET next_attempt_at=now()`)
	f.settle()
	var second string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT id FROM environment_previews WHERE suffix='two'`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := f.previewState(second); state != "ready" {
		t.Fatal("waiting branch was not created after a slot freed")
	}
	secondAPI := f.copyOf(second, f.ids["api"])
	secondWeb := f.copyOf(second, f.ids["web"])
	// A failing deploy is reported; a pushed fix retries automatically without copying data again.
	f.failDeploy[secondWeb] = true
	f.branches["101:staging/two"] = sha("two-broken")
	f.push("e-3", 101, "staging/two", false, 41)
	f.settle()
	if state, stage, _ := f.previewState(second); state != "failed" || stage != "deploy" {
		t.Fatalf("failed deploy not reported: %s/%s", state, stage)
	}
	f.branches["101:staging/two"] = sha("two-fixed")
	f.push("e-4", 101, "staging/two", false, 41)
	f.settle()
	if state, _, _ := f.previewState(second); state != "ready" || len(f.submittedFor(secondAPI, "preview_data")) != 1 {
		t.Fatal("a pushed fix should recover the preview without resetting data")
	}
	// Data copy failure and retry on a manual preview.
	f.call("root", "default", "DELETE", "/api/environment-previews/"+second, map[string]string{"confirm": "nope"}, 400)
	var slug string
	f.s.DB.QueryRow(context.Background(), `SELECT slug FROM environment_previews WHERE id=$1`, second).Scan(&slug)
	f.call("root", "default", "DELETE", "/api/environment-previews/"+second, map[string]string{"confirm": slug}, 202)
	f.settle()
	f.branches["101:staging/three"] = sha("three")
	f.call("root", "default", "POST", "/api/projects/p/environment-previews", map[string]string{"suffix": "three"}, 202)
	f.sql(`UPDATE preview_branch_signals SET next_attempt_at=now()`)
	if err := f.s.processPreviewSignals(context.Background()); err != nil {
		t.Fatal(err)
	}
	var third string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT id FROM environment_previews WHERE suffix='three'`).Scan(&third); err != nil {
		t.Fatal(err)
	}
	// Advance through planning, then make the api copy fail.
	if err := f.s.advancePreview(context.Background(), third); err != nil {
		t.Fatal(err)
	}
	thirdAPI := f.copyOf(third, f.ids["api"])
	f.failData[thirdAPI] = true
	f.settle()
	if state, stage, _ := f.previewState(third); state != "failed" || stage != "data" {
		t.Fatalf("data failure not reported: %s/%s", state, stage)
	}
	webCopies := len(f.submittedFor(f.copyOf(third, f.ids["web"]), "preview_data"))
	var environments int
	f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM environments WHERE project_id='p'`).Scan(&environments)
	f.call("root", "default", "POST", "/api/environment-previews/"+third+"/retry", nil, 202)
	f.settle()
	if state, _, detail := f.previewState(third); state != "ready" {
		t.Fatalf("retry did not recover: %s %s", state, detail)
	}
	var after int
	f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM environments WHERE project_id='p'`).Scan(&after)
	if after != environments || len(f.submittedFor(thirdAPI, "preview_data")) != 2 || len(f.submittedFor(f.copyOf(third, f.ids["web"]), "preview_data")) != webCopies {
		t.Fatal("retry duplicated the environment or repeated a completed copy")
	}
}

func TestEnvironmentPreviewRecoversFromRacesAndOutages(t *testing.T) {
	f := newPreviewFixture(t)
	recipe := f.setup()
	recipe.RequireApproval = false
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", recipe, 200)

	// A disconnected node makes a new preview wait instead of failing it.
	f.sql(`UPDATE nodes SET state='unreachable' WHERE id='n1'`)
	f.branches["101:staging/race"] = sha("race-1")
	f.push("r-1", 101, "staging/race", false, 41)
	f.settle()
	var preview string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT id FROM environment_previews WHERE suffix='race'`).Scan(&preview); err != nil {
		t.Fatal(err)
	}
	if state, stage, detail := f.previewState(preview); state != "creating" || stage != "plan" || !strings.Contains(detail, "Waiting for a node") {
		t.Fatalf("preview should wait for the node: %s/%s %s", state, stage, detail)
	}
	f.sql(`UPDATE nodes SET state='ready' WHERE id='n1'`)
	f.settle()
	if state, _, detail := f.previewState(preview); state != "ready" {
		t.Fatalf("preview did not recover after the node reconnected: %s %s", state, detail)
	}

	// The verified restore report from the deployment reaches the data report.
	api := f.copyOf(preview, f.ids["api"])
	var verified bool
	f.s.DB.QueryRow(context.Background(), `SELECT COALESCE((data_report->'seeded'->>'verified')::boolean,false) FROM environment_preview_resources WHERE resource_id=$1`, api).Scan(&verified)
	if !verified {
		t.Fatal("the verified row counts were not recorded on the preview")
	}

	// A push recorded while the preview is still publishing is deployed afterwards.
	web := f.copyOf(preview, f.ids["web"])
	f.sql(`UPDATE environment_previews SET state='creating',stage='publish' WHERE id=$1`, preview)
	f.branches["101:staging/race"] = sha("race-2")
	f.push("r-2", 101, "staging/race", false, 41)
	f.settle()
	deploys := f.submittedFor(web, "deploy")
	if state, _, _ := f.previewState(preview); state != "ready" || len(deploys) != 2 || deploys[1].Source.Revision != sha("race-2") {
		t.Fatalf("a push during publishing was not deployed: %+v", deploys)
	}

	// Reusing the branch name while its previous preview is being removed waits for the removal.
	var slug string
	f.s.DB.QueryRow(context.Background(), `SELECT slug FROM environment_previews WHERE id=$1`, preview).Scan(&slug)
	f.call("root", "default", "DELETE", "/api/environment-previews/"+preview, map[string]string{"confirm": slug}, 202)
	f.branches["101:staging/race"] = sha("race-3")
	f.push("r-3", 101, "staging/race", false, 41)
	if err := f.s.processPreviewSignals(context.Background()); err != nil {
		t.Fatal(err)
	}
	var reason string
	f.s.DB.QueryRow(context.Background(), `SELECT reason FROM preview_branch_signals WHERE branch='staging/race'`).Scan(&reason)
	if !strings.Contains(reason, "previous preview") {
		t.Fatalf("the reused branch was not kept waiting: %q", reason)
	}
	f.settle()
	var recreated string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT id FROM environment_previews WHERE suffix='race'`).Scan(&recreated); err != nil || recreated == preview {
		t.Fatalf("the branch did not get a new preview after the old one was removed: %v", err)
	}
	if state, _, _ := f.previewState(recreated); state != "ready" {
		t.Fatal("the recreated preview is not ready")
	}

	// Previews waiting for approval expire like any other inactive preview.
	recipe.RequireApproval = true
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", recipe, 200)
	f.branches["101:staging/idle"] = sha("idle")
	f.push("r-4", 101, "staging/idle", false, 41)
	f.settle()
	var idle string
	if err := f.s.DB.QueryRow(context.Background(), `SELECT id FROM environment_previews WHERE suffix='idle'`).Scan(&idle); err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE environment_previews SET last_activity_at=now()-interval '100 hours' WHERE id=$1`, idle)
	f.settle()
	if state, _, _ := f.previewState(idle); state != "" {
		t.Fatalf("an expired preview waiting for approval remains in %s", state)
	}
}

func TestEnvironmentPreviewStaysRemovedUntilBranchMoves(t *testing.T) {
	f := newPreviewFixture(t)
	recipe := f.setup()
	recipe.RequireApproval = false
	f.call("root", "default", "PUT", "/api/projects/p/preview-recipe", recipe, 200)
	scan := func() {
		f.sql(`UPDATE preview_recipes SET next_scan_at=now()`)
		f.settle()
	}
	current := func() (string, string) {
		var id, state string
		f.s.DB.QueryRow(context.Background(), `SELECT id,state FROM environment_previews WHERE suffix='gone'`).Scan(&id, &state)
		return id, state
	}
	f.branches["101:staging/gone"] = sha("gone-1")
	f.push("g-1", 101, "staging/gone", false, 41)
	f.settle()
	first, state := current()
	if state != "ready" {
		t.Fatalf("preview not created: %q", state)
	}
	var slug string
	f.s.DB.QueryRow(context.Background(), `SELECT slug FROM environment_previews WHERE id=$1`, first).Scan(&slug)
	f.call("root", "default", "DELETE", "/api/environment-previews/"+first, map[string]string{"confirm": slug}, 202)
	f.settle()
	// The branch still exists, but the scan must not bring back a preview someone deleted.
	scan()
	if id, _ := current(); id != "" {
		t.Fatal("the periodic scan re-created a deleted preview")
	}
	// A new push is new activity and starts a new preview.
	f.branches["101:staging/gone"] = sha("gone-2")
	f.push("g-2", 101, "staging/gone", false, 41)
	f.settle()
	second, state := current()
	if second == "" || second == first || state != "ready" {
		t.Fatalf("a new push did not start a new preview: %q %q", second, state)
	}
	// Inactivity cleanup also stays in effect across scans.
	f.sql(`UPDATE environment_previews SET last_activity_at=now()-interval '100 hours' WHERE id=$1`, second)
	f.settle()
	scan()
	if id, _ := current(); id != "" {
		t.Fatal("the periodic scan re-created an expired preview")
	}
	// Asking for the preview again from the Previews page brings it back without a new push.
	f.call("root", "default", "POST", "/api/projects/p/environment-previews", map[string]string{"suffix": "gone"}, 202)
	f.settle()
	if id, state := current(); id == "" || state != "ready" {
		t.Fatalf("an explicit request did not re-create the preview: %q", state)
	}
}
