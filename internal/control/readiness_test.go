package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestManagementAndAutomationIntegration(t *testing.T) {
	database := os.Getenv("COVE_TEST_DATABASE_URL")
	if database == "" {
		t.Skip("COVE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db := openTestDatabase(t, database)
	s := &Server{DB: db, Key: bytes.Repeat([]byte{7}, 32), AdminToken: strings.Repeat("a", 32), PublicURL: "https://cove.example.com"}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	node := resourceID()
	if _, err := db.Exec(ctx, "INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES($1,'fixture','127.0.0.1',22,'',$2,'ready')", node, []byte{}); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	call := func(method, path string, input any, status int) map[string]json.RawMessage {
		t.Helper()
		body, _ := json.Marshal(input)
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+s.AdminToken)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		out := map[string]json.RawMessage{}
		if status < 400 {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	value := func(data map[string]json.RawMessage, key string) string {
		var value string
		if err := json.Unmarshal(data[key], &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	project := value(call("POST", "/api/projects", map[string]string{"name": "Production"}, 201), "id")
	id := value(call("POST", "/api/workloads", workloadInput{ProjectID: project, Name: "app", NodeID: node, Kind: "service", Source: &worker.GitSource{Repository: "https://github.com/owner/private.git", Branch: "main", ComposePath: "compose.yaml", WatchPaths: []string{"src/**"}}}, 201), "id")
	env := value(call("POST", "/api/projects/"+project+"/environments", map[string]string{"name": "Live"}, 201), "id")
	compose := "services:\n  app:\n    image: busybox:1.37\n"
	placed := value(call("POST", "/api/workloads", workloadInput{Name: "placed", NodeID: node, Compose: compose, EnvironmentID: env}, 201), "id")
	if value(call("GET", "/api/workloads/"+placed, nil, 200), "environment_id") != env {
		t.Fatal("environment placement on create lost")
	}
	call("PUT", "/api/workloads/"+placed, workloadInput{Name: "placed", NodeID: node, Compose: compose, EnvironmentID: env}, 400)
	if _, err := db.Exec(ctx, "DELETE FROM workloads WHERE id=$1", placed); err != nil {
		t.Fatal(err)
	}
	call("POST", "/api/workloads", workloadInput{Name: "orphan", NodeID: node, Compose: compose, EnvironmentID: resourceID()}, 404)
	move := resourceMoveInput{TeamID: "default", ProjectID: project, EnvironmentID: env}
	move.Version = value(call("POST", "/api/workloads/"+id+"/move/preview", move, 200), "version")
	move.Acknowledge = true
	call("POST", "/api/workloads/"+id+"/move", move, 200)
	call("DELETE", "/api/projects/"+project, nil, 409)
	if value(call("GET", "/api/workloads/"+id, nil, 200), "environment_id") != env {
		t.Fatal("environment assignment lost")
	}
	policy := &worker.BackupConfig{Repository: "local", Password: "backup-password-for-test", KeepLast: 3, IntervalHours: 24}
	call("PUT", "/api/workloads/"+id+"/backup-policy", map[string]any{"backup": policy}, 200)
	got := call("GET", "/api/workloads/"+id+"/backup-policy", nil, 200)
	if bytes.Contains(got["backup"], []byte(policy.Password)) {
		t.Fatal("backup policy leaked password")
	}
	if _, err := db.Exec(ctx, "INSERT INTO operations(id,workload_id,action,state,payload) VALUES($1,$2,'deploy','succeeded',$3)", resourceID(), id, []byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "UPDATE workloads SET next_backup_at=now()-interval '1 minute' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if err := s.scheduleBackups(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM operations WHERE workload_id=$1 AND action='backup'", id).Scan(&count); err != nil || count != 1 {
		t.Fatal("scheduled backup", count, err)
	}
	if err := s.scheduleBackups(ctx); err != nil {
		t.Fatal(err)
	}
	secret := value(call("POST", "/api/workloads/"+id+"/webhook", nil, 201), "secret")
	commit := strings.Repeat("b", 40)
	body, _ := json.Marshal(map[string]any{"ref": "refs/heads/main", "after": commit, "repository": map[string]string{"clone_url": "https://github.com/owner/private.git"}})
	hook := func(signature, delivery string, status int) {
		t.Helper()
		r := httptest.NewRequest("POST", "/hooks/github/"+id, bytes.NewReader(body))
		r.Header.Set("X-GitHub-Event", "push")
		r.Header.Set("X-GitHub-Delivery", delivery)
		r.Header.Set("X-Hub-Signature-256", signature)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("hook: %d %s", w.Code, w.Body.String())
		}
	}
	hook("sha256=bad", "delivery-1", 401)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	originalBody := body
	body, _ = json.Marshal(map[string]any{"ref": "refs/heads/main", "after": commit, "repository": map[string]string{"clone_url": "https://github.com/owner/private.git"}, "commits": []map[string]any{{"added": []string{"README.md"}, "modified": []string{}, "removed": []string{}}}})
	filteredMAC := hmac.New(sha256.New, []byte(secret))
	filteredMAC.Write(body)
	hook("sha256="+hex.EncodeToString(filteredMAC.Sum(nil)), "unwatched", 200)
	body = originalBody
	hook(signature, "delivery-1", 202)
	hook(signature, "delivery-1", 202)
	if err := s.dispatchWebhooks(ctx); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM webhook_deliveries WHERE workload_id=$1 AND operation_id IS NULL", id).Scan(&pending); err != nil || pending != 1 {
		t.Fatal("replay or busy-queue failure", pending, err)
	}
	if _, err := db.Exec(ctx, "UPDATE operations SET state='succeeded' WHERE workload_id=$1", id); err != nil {
		t.Fatal(err)
	}
	if err := s.dispatchWebhooks(ctx); err != nil {
		t.Fatal(err)
	}
	var encrypted []byte
	if err := db.QueryRow(ctx, "SELECT payload FROM operations WHERE workload_id=$1 AND state='queued'", id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	data, err := unseal(s.Key, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	var request worker.Request
	if err = json.Unmarshal(data, &request); err != nil || request.Source.Revision != commit {
		t.Fatal("webhook commit not pinned", err)
	}
	if _, err := db.Exec(ctx, "UPDATE operations SET state='succeeded' WHERE workload_id=$1", id); err != nil {
		t.Fatal(err)
	}
	call("POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "stop"}, 202)
	hook(signature, "stopped-delivery", 200)
	hook("sha256=bad", "stopped-invalid-signature", 401)
	if err := db.QueryRow(ctx, "SELECT count(*) FROM webhook_deliveries WHERE workload_id=$1 AND operation_id IS NULL", id).Scan(&pending); err != nil || pending != 0 {
		t.Fatal("stopped resource accepted an automatic deployment", pending, err)
	}
	call("DELETE", "/api/workloads/"+id+"/webhook", nil, 200)
	hook(signature, "delivery-2", 404)
	call("PUT", "/api/workloads/"+id+"/environment", map[string]string{"project_id": project, "environment_id": ""}, 409)
	move.EnvironmentID = ""
	move.Version = value(call("POST", "/api/workloads/"+id+"/move/preview", move, 200), "version")
	call("POST", "/api/workloads/"+id+"/move", move, 409)
	if _, err := db.Exec(ctx, "UPDATE operations SET state='succeeded' WHERE workload_id=$1", id); err != nil {
		t.Fatal(err)
	}
	move.Version = value(call("POST", "/api/workloads/"+id+"/move/preview", move, 200), "version")
	call("POST", "/api/workloads/"+id+"/move", move, 200)
	call("DELETE", "/api/projects/"+project, nil, 409)
}
