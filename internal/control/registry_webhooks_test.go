package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRegistryWebhookAuthenticationCoalescingAndRotation(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('hook-node','Node','localhost',22,'',$1,'ready','{"image_lifecycle":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('hook-project','Project','default')`)
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "App", NodeID: "hook-node", ProjectID: "hook-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"}, 201), "id")
	path := "/api/workloads/" + id
	registry := jsonString(t, f.call("root", "default", "POST", "/api/registries", connectionConfig{Name: "Fixture", Registry: &worker.Registry{Server: "registry.example.test", Username: "fixture", Password: "fixture-only-registry-password"}}, 201), "id")
	f.call("root", "default", "PUT", path+"/registry", map[string]string{"registry_id": registry}, 200)
	viewer := f.user("hook-viewer", "hook-viewer@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','hook-viewer','member','[{"scope":"project","scope_id":"hook-project","role":"viewer"}]')`)
	body := map[string]any{"provider": "distribution", "enabled": true, "filters": []registryWebhookFilter{{Repository: "team/app", Tag: "stable"}, {Repository: "team/api", Tag: "*"}}}
	f.call(viewer, "default", "PUT", path+"/registry-webhook", body, 403)
	var created registryWebhookStatus
	response := f.call("root", "default", "PUT", path+"/registry-webhook", body, 201)
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || len(created.Token) != 43 || created.Endpoint == nil || !created.Matches || !created.Supported || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("hook creation metadata or token was invalid", err)
	}
	endpoint, err := url.Parse(*created.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	hook := endpoint.Path[strings.LastIndex(endpoint.Path, "/")+1:]
	read := f.call(viewer, "default", "GET", path+"/registry-webhook", nil, 200).Body.String()
	if strings.Contains(read, created.Token) || strings.Contains(read, `"token"`) || strings.Contains(read, "fixture-only-registry-password") {
		t.Fatal("secret exposed in hook status")
	}
	var sealed []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT secret FROM registry_webhooks WHERE workload_id=$1", id).Scan(&sealed); err != nil || bytes.Contains(sealed, []byte(created.Token)) {
		t.Fatal("hook token was not sealed", err)
	}
	push := func(event, repository, tag, token string, want int) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"events": []any{map[string]any{"id": event, "action": "push", "target": map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json", "repository": repository, "tag": tag, "digest": "sha256:" + strings.Repeat("f", 64), "url": "http://127.0.0.1/never-fetch"}}}})
		r := httptest.NewRequest("POST", endpoint.Path, bytes.NewReader(payload))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("registry notification got %d want %d: %s", w.Code, want, w.Body.String())
		}
	}
	checks := func(want int) {
		t.Helper()
		var count int
		if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM operations WHERE workload_id=$1 AND action='deploy'", id).Scan(&count); err != nil || count != want {
			t.Fatal("unexpected metadata queue count", count, want, err)
		}
	}
	next := func() time.Time {
		t.Helper()
		var at time.Time
		if err := f.s.DB.QueryRow(context.Background(), "SELECT next_image_check_at FROM workloads WHERE id=$1", id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	f.sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id=$1`, id)
	push("one", "team/app", "stable", strings.Repeat("x", 43), 401)
	push("other", "other/app", "stable", created.Token, 202)
	if !next().After(time.Now().Add(time.Hour)) {
		t.Fatal("unauthorized or nonmatching event requested a check")
	}
	push("one", "team/app", "stable", created.Token, 202)
	checks(0)
	if err := f.s.dispatchRegistryWebhooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	checks(1)
	queuedAt := next()
	push("one", "team/app", "stable", created.Token, 202)
	push("one", "team/api", "1.2.3", created.Token, 409)
	if !queuedAt.Equal(next()) {
		t.Fatal("duplicate or conflicting identity rearmed a consumed check")
	}
	push("two", "team/api", "1.2.3", created.Token, 202)
	push("three", "team/api", "1.2.4", created.Token, 202)
	if err := f.s.dispatchRegistryWebhooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	checks(1)
	f.sql(`UPDATE operations SET state='failed' WHERE workload_id=$1 AND state='queued'`, id)
	if err := f.s.dispatchRegistryWebhooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	checks(2)
	var rotated registryWebhookStatus
	if err := json.Unmarshal(f.call("root", "default", "POST", path+"/registry-webhook/rotate", nil, 200).Body.Bytes(), &rotated); err != nil || len(rotated.Token) != 43 || rotated.Token == created.Token {
		t.Fatal("rotation did not replace the token", err)
	}
	push("four", "team/app", "stable", created.Token, 401)
	push("four", "team/app", "stable", rotated.Token, 202)
	updated := f.call("root", "default", "PUT", path+"/registry-webhook", body, 200).Body.String()
	if strings.Contains(updated, `"token"`) {
		t.Fatal("ordinary save exposed or replaced the token")
	}
	body["enabled"] = false
	f.call("root", "default", "PUT", path+"/registry-webhook", body, 200)
	f.sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id=$1`, id)
	push("disabled", "team/app", "stable", rotated.Token, 202)
	if !next().After(time.Now().Add(time.Hour)) {
		t.Fatal("disabled hook requested a check")
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='hook-node'`)
	f.call("root", "default", "PUT", path+"/registry-webhook", body, 200)
	body["enabled"] = true
	f.call("root", "default", "PUT", path+"/registry-webhook", body, 409)
	f.sql(`UPDATE nodes SET capabilities='{"image_lifecycle":true}' WHERE id='hook-node'`)
	f.call("root", "default", "PUT", path+"/registry-webhook", body, 200)
	f.sql(`UPDATE operations SET state='failed' WHERE workload_id=$1 AND state='queued'`, id)
	f.call("root", "default", "PUT", "/api/registries/"+registry, connectionConfig{Name: "Changed", Registry: &worker.Registry{Server: "changed.example.test", Username: "fixture", Password: "new-fixture-password"}}, 200)
	f.sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id=$1`, id)
	push("changed", "team/app", "stable", rotated.Token, 202)
	if !next().After(time.Now().Add(time.Hour)) {
		t.Fatal("old registry sender controlled a replacement destination")
	}
	f.call("root", "default", "POST", path+"/registry-webhook/rotate", nil, 409)
	var rebound registryWebhookStatus
	if err := json.Unmarshal(f.call("root", "default", "PUT", path+"/registry-webhook", body, 200).Body.Bytes(), &rebound); err != nil || len(rebound.Token) != 43 || rebound.Token == rotated.Token {
		t.Fatal("registry destination change did not replace token", err)
	}
	var oldIdentities int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM registry_webhook_deliveries WHERE webhook_id=$1", hook).Scan(&oldIdentities); err != nil || oldIdentities != 0 {
		t.Fatal("new registry inherited a foreign delivery identity namespace", oldIdentities, err)
	}
	push("changed", "team/app", "stable", rotated.Token, 401)
	push("changed", "team/app", "stable", rebound.Token, 202)
	f.sql(`INSERT INTO registry_webhook_deliveries(webhook_id,event_key,source_key,image_key) SELECT $1,md5(i::text)||md5(i::text),md5(i::text)||md5(i::text),repeat('a',64) FROM generate_series(1,4096-(SELECT count(*)::int FROM registry_webhook_deliveries WHERE webhook_id=$1)) i`, hook)
	f.sql(`UPDATE workloads SET next_image_check_at=now()+interval '1 day' WHERE id=$1`, id)
	push("full", "team/app", "stable", rebound.Token, 429)
	if !next().After(time.Now().Add(time.Hour)) {
		t.Fatal("untracked event changed check intent")
	}
	f.sql(`UPDATE registry_webhook_deliveries SET received_at=now()-interval '8 days' WHERE webhook_id=$1`, hook)
	push("full", "team/app", "stable", rebound.Token, 202)
	f.call("root", "default", "DELETE", path+"/registry-webhook", nil, 200)
	push("after-delete", "team/app", "stable", rebound.Token, 401)
	var remaining int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM registry_webhook_deliveries WHERE webhook_id=$1", hook).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("hook deletion left replay records", remaining, err)
	}
}
