package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestInfrastructureTeamIsolationAndSecrets(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("infra-owner", "infra-owner@example.com")
	viewer := f.user("infra-viewer", "infra-viewer@example.com")
	other := f.user("infra-other", "infra-other@example.com")
	f.sql("INSERT INTO teams(id,name) VALUES('infra','Infra'),('other','Other')")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('infra','infra-owner','owner','[]'),('infra','infra-viewer','member','[]'),('other','infra-other','owner','[]')`)
	registry := connectionConfig{Name: "Private images", Registry: &worker.Registry{Server: "ghcr.io", Username: "cove", Password: "test-registry-secret"}}
	id := jsonString(t, f.call(owner, "infra", "POST", "/api/registries", registry, 201), "id")
	list := f.call(viewer, "infra", "GET", "/api/registries", nil, 200)
	if strings.Contains(list.Body.String(), registry.Registry.Password) {
		t.Fatal("registry secret exposed")
	}
	if strings.Contains(f.call(other, "other", "GET", "/api/registries", nil, 200).Body.String(), id) {
		t.Fatal("registry leaked across teams")
	}
	f.call(viewer, "infra", "PUT", "/api/registries/"+id, registry, 403)
	f.call(other, "other", "PUT", "/api/registries/"+id, registry, 404)
	registry.Registry.Password = ""
	f.call(owner, "infra", "PUT", "/api/registries/"+id, registry, 200)
	saved, err := f.s.readConnection(context.Background(), id, "infra", "registry")
	if err != nil || saved.Registry.Password != "test-registry-secret" {
		t.Fatal("secret was not preserved", err)
	}
	registry.Registry.Server = "registry.example.com"
	f.call(owner, "infra", "PUT", "/api/registries/"+id, registry, 400)
	f.call(owner, "infra", "POST", "/api/terminal", map[string]any{"node_id": "missing", "columns": 80, "rows": 24}, 403)
	f.call(viewer, "infra", "GET", "/api/notifications", nil, 403)
	storage := connectionConfig{Name: "Backups", Storage: &worker.StorageLocation{Protocol: "s3", Endpoint: "https://storage.example.com", Path: "bucket/backups", AccessKey: "access", SecretKey: "storage-secret"}}
	sid := jsonString(t, f.call(owner, "infra", "POST", "/api/storage-locations", storage, 201), "id")
	if strings.Contains(f.call(owner, "infra", "GET", "/api/storage-locations", nil, 200).Body.String(), "storage-secret") {
		t.Fatal("storage secret exposed")
	}
	f.call(other, "other", "DELETE", "/api/storage-locations/"+sid, nil, 404)
	f.call(owner, "infra", "DELETE", "/api/storage-locations/"+sid, nil, 200)
}

func TestInfrastructureResolutionAndReferences(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('infra-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('infra-project','Project','default')")
	config, _ := seal(f.s.Key, []byte(`{"name":"Resource","node_id":"infra-node","kind":"service","compose":"services:\n  app:\n    image: busybox:1.37\n"}`))
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('infra-resource','infra-node','Resource',$1,'infra-project','default')", config)
	registry := connectionConfig{Name: "Private", Registry: &worker.Registry{Server: "ghcr.io", Username: "cove", Password: "private-secret"}}
	rid := jsonString(t, f.call("root", "default", "POST", "/api/registries", registry, 201), "id")
	f.call("root", "default", "PUT", "/api/workloads/infra-resource/registry", map[string]string{"registry_id": rid}, 200)
	f.call("root", "default", "DELETE", "/api/registries/"+rid, nil, 409)
	storage := connectionConfig{Name: "Remote", Storage: &worker.StorageLocation{Protocol: "webdav", Endpoint: "https://files.example.com/dav/", Path: "backups", Username: "cove", Password: "dav-secret"}}
	sid := jsonString(t, f.call("root", "default", "POST", "/api/storage-locations", storage, 201), "id")
	backup := &worker.BackupConfig{StorageID: sid, Password: "backup-encryption-secret", KeepLast: 7}
	f.call("root", "default", "PUT", "/api/workloads/infra-resource/backup-policy", map[string]any{"backup": backup}, 200)
	response := f.call("root", "default", "GET", "/api/workloads/infra-resource/backup-policy", nil, 200)
	if strings.Contains(response.Body.String(), "dav-secret") || strings.Contains(response.Body.String(), backup.Password) || !strings.Contains(response.Body.String(), "rclone:cove:backups/infra-resource") {
		t.Fatal("invalid redacted backup policy", response.Body.String())
	}
	f.call("root", "default", "DELETE", "/api/storage-locations/"+sid, nil, 409)
	storage.Storage.Path = "elsewhere"
	f.call("root", "default", "PUT", "/api/storage-locations/"+sid, storage, 409)
	payload, _ := seal(f.s.Key, []byte(`{}`))
	f.sql("INSERT INTO operations(id,workload_id,action,payload) VALUES('infra-op','infra-resource','deploy',$1)", payload)
	request := worker.Request{ID: "infra-op", Workload: "infra-resource", Action: "deploy", Backup: backup}
	if err := f.s.prepareInfrastructure(ctx, &request); err != nil {
		t.Fatal(err)
	}
	if request.Registry.Password != "private-secret" || request.Backup.Remote.Password != "dav-secret" {
		t.Fatal("credentials missing from worker request")
	}
	registry.Registry.Password = "rotated-secret"
	f.call("root", "default", "PUT", "/api/registries/"+rid, registry, 200)
	if err := f.s.prepareInfrastructure(ctx, &request); err != nil || request.Registry.Password != "private-secret" {
		t.Fatal("retry payload changed", err)
	}
}

type notificationRoundTrip func(*http.Request) (*http.Response, error)

func (f notificationRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNotificationOutboxRetriesAndRedaction(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	config := connectionConfig{Name: "Operations", Notification: &notificationConfig{Provider: "webhook", URL: "https://notify.example.com/private-token", Enabled: true, Failure: true}}
	id := jsonString(t, f.call("root", "default", "POST", "/api/notifications", config, 201), "id")
	if strings.Contains(f.call("root", "default", "GET", "/api/notifications", nil, 200).Body.String(), "private-token") {
		t.Fatal("webhook secret exposed")
	}
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES('notify-node','Node','localhost',22,'',$1)", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('notify-project','Project','default')")
	sealed, _ := seal(f.s.Key, []byte(`{}`))
	f.sql("INSERT INTO workloads(id,node_id,name,configuration,project_id,team_id) VALUES('notify-resource','notify-node','Resource',$1,'notify-project','default')", sealed)
	f.sql("INSERT INTO operations(id,workload_id,action,payload) VALUES('notify-operation','notify-resource','deploy',$1)", sealed)
	f.sql("UPDATE operations SET state='failed' WHERE id='notify-operation'")
	calls := 0
	f.s.NotificationTransport = notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("X-Cove-Delivery") != "notify-operation:"+id {
			t.Error("unstable delivery ID")
		}
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["event"] != "operation.failed" {
			t.Error("incorrect notification payload")
		}
		status := 503
		if calls > 1 {
			status = 204
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if err := f.s.dispatchNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	f.sql("UPDATE notification_deliveries SET next_attempt=now()")
	if err := f.s.dispatchNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.dispatchNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("unexpected delivery count", calls)
	}
	var state string
	var attempts int
	if err := f.s.DB.QueryRow(ctx, "SELECT state,attempts FROM notification_deliveries").Scan(&state, &attempts); err != nil || state != "sent" || attempts != 2 {
		t.Fatal(state, attempts, err)
	}
}

func TestNotificationDestinationAddresses(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "100.100.100.200", "::1", "::ffff:127.0.0.1", "fc00::1", "2001:db8::1"} {
		if publicWebhookIP(net.ParseIP(address)) {
			t.Fatal("accepted private or reserved destination", address)
		}
	}
	for _, address := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !publicWebhookIP(net.ParseIP(address)) {
			t.Fatal("rejected public destination", address)
		}
	}
}
