package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type registryHookFixture struct {
	*accountFixture
	id, path, endpoint, token string
}

func newRegistryHookFixture(t *testing.T, provider, server string) *registryHookFixture {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('node','Node','localhost',22,'',$1,'ready','{"image_lifecycle":true,"suspensions":true,"guarded_deploy":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('project','Project','default')`)
	f.sql(`INSERT INTO environments(id,project_id,name) VALUES('environment','project','Production')`)
	guarded := true
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "App", NodeID: "node", ProjectID: "project", EnvironmentID: "environment", Kind: "service", Compose: "services:\n  app:\n    image: " + server + "/team/app:stable\n", GuardedDeploy: &guarded, Environment: map[string]string{"FIXTURE": "preserved"}}, 201), "id")
	registry := jsonString(t, f.call("root", "default", "POST", "/api/registries", connectionConfig{Name: "Fixture", Registry: &worker.Registry{Server: server, Username: "fixture", Password: "generated-fixture-only"}}, 201), "id")
	path := "/api/workloads/" + id
	f.call("root", "default", "PUT", path+"/registry", map[string]string{"registry_id": registry}, 200)
	var created registryWebhookStatus
	input := map[string]any{"provider": provider, "enabled": true, "filters": []registryWebhookFilter{{Repository: "team/app", Tag: "stable"}}}
	if err := json.Unmarshal(f.call("root", "default", "PUT", path+"/registry-webhook", input, 201).Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	endpoint, err := url.Parse(*created.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return &registryHookFixture{f, id, path, endpoint.Path, created.Token}
}

func (f *registryHookFixture) send(body []byte, provider, delivery string, want int) *httptest.ResponseRecorder {
	f.t.Helper()
	endpoint := f.endpoint
	if provider == "docker_hub" {
		endpoint += "?secret=" + f.token
	}
	r := httptest.NewRequest("POST", endpoint, bytes.NewReader(body))
	if provider == "ghcr" {
		r.Header.Set("X-GitHub-Event", "package")
		r.Header.Set("X-GitHub-Delivery", delivery)
		mac := hmac.New(sha256.New, []byte(f.token))
		mac.Write(body)
		r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	} else if provider == "distribution" {
		r.Header.Set("Authorization", "Bearer "+f.token)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != want {
		f.t.Fatalf("notification got%d want%d: %s", w.Code, want, w.Body.String())
	}
	if provider == "docker_hub" && r.URL.RawQuery != "" {
		f.t.Fatal("native URL credential remained in request")
	}
	return w
}

func distributionPush(id string) []byte {
	return []byte(fmt.Sprintf(`{"events":[{"id":%q,"action":"push","target":{"mediaType":"application/vnd.oci.image.index.v1+json","repository":"team/app","tag":"stable"}}]}`, id))
}

func (f *registryHookFixture) status() registryWebhookStatus {
	f.t.Helper()
	var status registryWebhookStatus
	if err := json.Unmarshal(f.call("root", "default", "GET", f.path+"/registry-webhook", nil, 200).Body.Bytes(), &status); err != nil {
		f.t.Fatal(err)
	}
	return status
}

func TestRegistryRedeployUsesUnifiedApprovalFreezeAndWindowAdmission(t *testing.T) {
	f := newRegistryHookFixture(t, "distribution", "registry.example.test")
	ctx := context.Background()
	f.call("root", "default", "PUT", "/api/projects/project/deployment-policy", deploymentPolicy{Approval: true}, 200)
	f.send(distributionPush("first"), "distribution", "", 202)
	if f.status().Pending == nil {
		t.Fatal("delivery was not durably pending")
	}
	if err := f.s.dispatchRegistryWebhooks(ctx); err != nil {
		t.Fatal(err)
	}
	status := f.status()
	if status.Pending != nil || status.LastOperation == nil || status.LastOperation.State != "queued" {
		t.Fatal("pending intent was not atomically claimed", status)
	}
	op := status.LastOperation.ID
	var payload []byte
	if err := f.s.DB.QueryRow(ctx, "SELECT payload FROM operations WHERE id=$1", op).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	raw, err := unseal(f.s.Key, payload)
	var request worker.Request
	if err != nil || json.Unmarshal(raw, &request) != nil || request.Action != "deploy" || !request.PullLatest || request.Kind != "service" || request.GuardedDeploy == nil || !*request.GuardedDeploy || request.Environment["FIXTURE"] != "preserved" || request.Registry != nil || !strings.Contains(request.Compose, ":stable") {
		t.Fatal("ordinary immutable deploy request lost safety/configuration", request, err)
	}
	admission := func(want bool) {
		t.Helper()
		got, err := f.s.admitDeployment(ctx, op)
		if err != nil || got != want {
			t.Fatal("unexpected admission", got, want, err)
		}
	}
	admission(false)
	f.call("root", "default", "POST", "/api/operations/"+op+"/approve", map[string]any{}, 200)
	from, until := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	f.call("root", "default", "PUT", "/api/projects/project/deployment-policy", deploymentPolicy{FreezeFrom: &from, FreezeUntil: &until}, 200)
	admission(false)
	now := time.Now().UTC()
	f.call("root", "default", "PUT", "/api/projects/project/deployment-policy", deploymentPolicy{WindowStart: now.Add(time.Hour).Format("15:04"), WindowEnd: now.Add(2 * time.Hour).Format("15:04")}, 200)
	admission(false)
	f.call("root", "default", "PUT", "/api/projects/project/deployment-policy", deploymentPolicy{WindowStart: now.Add(-time.Hour).Format("15:04"), WindowEnd: now.Add(time.Hour).Format("15:04")}, 200)
	admission(true)
	f.send(distributionPush("first"), "distribution", "", 202)
	if f.status().Pending != nil {
		t.Fatal("duplicate rearmed consumed deployment")
	}
	f.send(distributionPush("second"), "distribution", "", 202)
	f.send(distributionPush("third"), "distribution", "", 202)
	if err := f.s.dispatchRegistryWebhooks(ctx); err != nil {
		t.Fatal(err)
	}
	if f.status().Pending == nil || f.status().LastOperation.ID != op {
		t.Fatal("busy deployment intent was lost or replaced")
	}
	f.sql("UPDATE operations SET state='failed' WHERE id=$1", op)
	if err := f.s.dispatchRegistryWebhooks(ctx); err != nil {
		t.Fatal(err)
	}
	if after := f.status(); after.Pending != nil || after.LastOperation == nil || after.LastOperation.ID == op {
		t.Fatal("coalesced follow-up was not queued", after)
	}
}

func TestRegistryNativeProvidersAndStableSignedReplayIdentity(t *testing.T) {
	for _, provider := range []string{"docker_hub", "ghcr"} {
		t.Run(provider, func(t *testing.T) {
			server := "docker.io"
			if provider == "ghcr" {
				server = "ghcr.io"
			}
			f := newRegistryHookFixture(t, provider, server)
			body := []byte(fmt.Sprintf(`{"push_data":{"pushed_at":%d,"tag":"stable"},"repository":{"repo_name":"team/app"},"callback_url":"http://127.0.0.1/do-not-fetch"}`, time.Now().Unix()))
			if provider == "ghcr" {
				body = []byte(`{"action":"published","package":{"id":1,"name":"app","ecosystem":"container","owner":{"login":"team"},"package_version":{"id":2,"container_metadata":{"tag":{"name":"stable"}}}}}`)
			}
			f.send(body, provider, "first", 202)
			if f.status().Pending == nil {
				t.Fatal("native event was not accepted")
			}
			if err := f.s.dispatchRegistryWebhooks(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.send(body, provider, "changed-unsigned-header", 202)
			if f.status().Pending != nil {
				t.Fatal("same signed/native event rearmed a consumed intent")
			}
			if provider == "ghcr" {
				f.send([]byte(strings.Replace(string(body), `"id":2`, `"id":3`, 1)), provider, "first", 409)
				f.send([]byte(strings.Replace(string(body), `"action":"published"`, `"action":"updated"`, 1)), provider, "new-signed-update", 202)
				if f.status().Pending == nil {
					t.Fatal("a distinct signed package update was suppressed as a retry")
				}
			}
			read := f.call("root", "default", "GET", f.path+"/registry-webhook", nil, 200).Body.String()
			if strings.Contains(read, f.token) || strings.Contains(read, "?secret=") || strings.Contains(read, `"token"`) {
				t.Fatal("native credential exposed in metadata")
			}
		})
	}
}

func TestRegistryRedeployPreservesManualPauseAndSleepIntent(t *testing.T) {
	for _, state := range []string{"stopped", "paused", "asleep", "unknown"} {
		t.Run(state, func(t *testing.T) {
			f := newRegistryHookFixture(t, "distribution", "registry.example.test")
			ctx := context.Background()
			switch state {
			case "stopped":
				f.sql("UPDATE workloads SET manually_stopped=true WHERE id=$1", f.id)
			case "paused":
				f.sql("UPDATE environments SET paused=true WHERE id=(SELECT environment_id FROM workloads WHERE id=$1)", f.id)
			case "asleep", "unknown":
				f.sql(`UPDATE workloads SET idle_policy='{"enabled":true}' WHERE id=$1`, f.id)
				f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
					if state == "unknown" {
						return worker.RPCResponse{Error: "fixture unavailable"}, nil
					}
					return worker.RPCResponse{Suspension: &worker.SuspensionStatus{Reason: "idle", Phase: "paused"}}, nil
				}
			}
			f.send(distributionPush("one"), "distribution", "", 202)
			if err := f.s.dispatchRegistryWebhooks(ctx); err != nil {
				t.Fatal(err)
			}
			s := f.status()
			if s.LastOperation != nil {
				t.Fatal("automation ran a stopped/paused/asleep/unknown resource", s)
			}
			if state == "unknown" {
				if s.Pending == nil || s.LastError == "" {
					t.Fatal("unknown worker state was treated as awake", s)
				}
			} else if s.Pending != nil || s.Ignored == "" {
				t.Fatal("stopped intent was retained for a later surprise run", s)
			}
		})
	}
}

func TestRegistryPendingRuntimeFailuresDoNotStarveOtherResources(t *testing.T) {
	f := newRegistryHookFixture(t, "distribution", "registry.example.test")
	fixtures := []*registryHookFixture{f}
	registry := f.status().Registry["id"]
	for i := 1; i < 5; i++ {
		id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: fmt.Sprintf("App%d", i), NodeID: "node", ProjectID: "project", EnvironmentID: "environment", Kind: "service", Compose: "services:\n  app:\n    image: registry.example.test/team/app:stable\n"}, 201), "id")
		path := "/api/workloads/" + id
		f.call("root", "default", "PUT", path+"/registry", map[string]string{"registry_id": registry}, 200)
		var status registryWebhookStatus
		if err := json.Unmarshal(f.call("root", "default", "PUT", path+"/registry-webhook", map[string]any{"provider": "distribution", "enabled": true, "filters": []registryWebhookFilter{{Repository: "team/app", Tag: "stable"}}}, 201).Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		endpoint, err := url.Parse(*status.Endpoint)
		if err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, &registryHookFixture{f.accountFixture, id, path, endpoint.Path, status.Token})
	}
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{Error: "fixture read unavailable"}, nil
	}
	for i, fixture := range fixtures {
		if i < 4 {
			f.sql(`UPDATE workloads SET idle_policy='{"enabled":true}' WHERE id=$1`, fixture.id)
		}
		fixture.send(distributionPush("one"), "distribution", "", 202)
	}
	for i := 0; i < 2; i++ {
		if err := f.s.dispatchRegistryWebhooks(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if healthy := fixtures[4].status(); healthy.LastOperation == nil || healthy.Pending != nil {
		t.Fatal("runtime failures monopolized dispatch", healthy)
	}
	for _, fixture := range fixtures[:4] {
		if failed := fixture.status(); failed.Pending == nil || failed.LastOperation != nil || failed.LastError == "" {
			t.Fatal("failed runtime read lost its pending/error state", failed)
		}
	}
}

func TestRegistryStopAndEnvironmentPauseDiscardUnqueuedPushes(t *testing.T) {
	for _, action := range []string{"stop", "pause"} {
		t.Run(action, func(t *testing.T) {
			f := newRegistryHookFixture(t, "distribution", "registry.example.test")
			f.send(distributionPush("one"), "distribution", "", 202)
			if f.status().Pending == nil {
				t.Fatal("fixture push was not pending")
			}
			if action == "stop" {
				f.call("root", "default", "POST", f.path+"/operations", map[string]string{"action": "stop"}, 202)
			} else {
				f.call("root", "default", "PUT", "/api/environments/environment/suspension", map[string]bool{"paused": true}, 202)
			}
			if after := f.status(); after.Pending != nil || after.Ignored == "" {
				t.Fatal("explicit lifecycle action retained a surprise deployment", after)
			}
		})
	}
}

func TestRegistryHookMustBeRemovedBeforeChangingToAnUnsupportedResource(t *testing.T) {
	f := newRegistryHookFixture(t, "distribution", "registry.example.test")
	guarded := false
	job := workloadInput{Name: "App", NodeID: "node", Kind: "job", JobService: "app", Compose: "services:\n  app:\n    image: busybox:1.37\n", GuardedDeploy: &guarded}
	f.call("root", "default", "PUT", f.path, job, 409)
	if got := f.status(); got.Configuration == nil || !got.Configuration.Enabled {
		t.Fatal("refused conversion lost hook configuration", got)
	}
	f.call("root", "default", "DELETE", f.path+"/registry-webhook", nil, 200)
	f.call("root", "default", "PUT", f.path, job, 200)
}

func TestRegistryDispatchSerializesWithEnvironmentPause(t *testing.T) {
	f := newRegistryHookFixture(t, "distribution", "registry.example.test")
	f.send(distributionPush("one"), "distribution", "", 202)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pause, err := f.s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pause.Rollback(context.Background())
	if _, err = pause.Exec(ctx, "UPDATE environments SET paused=true WHERE id='environment'"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.s.dispatchRegistryWebhook(ctx, f.id) }()
	for {
		select {
		case err := <-done:
			t.Fatal("deployment escaped the in-progress pause transaction", err)
		default:
		}
		var waiting bool
		if err = f.s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT e.paused FROM environments%FOR SHARE OF e')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("dispatcher did not reach the environment admission lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// This must not deadlock against a dispatcher holding the hook while waiting for the environment.
	if _, err = pause.Exec(ctx, "UPDATE registry_webhooks SET pending_at=NULL WHERE workload_id=$1", f.id); err != nil {
		t.Fatal(err)
	}
	if err = pause.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if after := f.status(); after.Pending != nil || after.LastOperation != nil {
		t.Fatal("paused resource acquired a surprise deployment", after)
	}
}
