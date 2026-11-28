package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeployHookAuthenticationQueueAndRevocation(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('n','Node','localhost',22,'',$1,'ready')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('p','Project','default')`)
	config := workloadInput{Name: "App", NodeID: "n", ProjectID: "p", Compose: "services:\n  app:\n    image: nginx:alpine\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", config, 201), "id")
	path := "/api/workloads/" + id + "/deploy-hook"
	editor := f.user("hook-editor", "hook-editor@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','hook-editor','member','[{"scope":"team","role":"editor"}]')`)
	created := f.call(editor, "default", "POST", path, nil, 201)
	hook, token := jsonString(t, created, "id"), jsonString(t, created, "token")
	if strings.Contains(f.call("root", "default", "GET", path, nil, 200).Body.String(), token) {
		t.Fatal("saved token exposed")
	}
	call := func(method, delivery, credential string, want int) *httptest.ResponseRecorder {
		t.Helper()
		endpoint := "/hooks/deploy/" + hook
		var body []byte
		if method == "POST" {
			body, _ = json.Marshal(map[string]string{"delivery": delivery})
		} else {
			endpoint += "/deliveries/" + delivery
		}
		request := httptest.NewRequest(method, endpoint, bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+credential)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s %s got %d want %d: %s", method, endpoint, response.Code, want, response.Body)
		}
		return response
	}
	call("POST", "delivery-1", "wrong", 401)
	first := call("POST", "delivery-1", token, 202)
	operation := jsonString(t, first, "operation_id")
	if jsonString(t, call("POST", "delivery-1", token, 200), "operation_id") != operation {
		t.Fatal("retry changed operation ID")
	}
	if jsonString(t, call("GET", "delivery-1", token, 200), "state") != "pending" {
		t.Fatal("queued receipt missing")
	}
	if err := f.s.dispatchWebhook(ctx, id); err != nil {
		t.Fatal(err)
	}
	if jsonString(t, call("GET", "delivery-1", token, 200), "state") != "queued" {
		t.Fatal("operation not dispatched")
	}
	f.sql(`UPDATE operations SET state='succeeded' WHERE id=$1`, operation)
	if jsonString(t, call("GET", "delivery-1", token, 200), "state") != "succeeded" {
		t.Fatal("terminal state not available")
	}
	for i := 2; i <= 10; i++ {
		call("POST", "delivery-"+string(rune('a'+i)), token, 202)
	}
	limited := call("POST", "limited", token, 429)
	if limited.Header().Get("Retry-After") != "60" {
		t.Fatal("rate retry missing")
	}
	// Duplicate retries remain observable without consuming another delivery slot.
	call("POST", "delivery-1", token, 200)
	f.sql(`UPDATE team_members SET grants='[{"scope":"team","role":"viewer"}]' WHERE user_id='hook-editor'`)
	call("POST", "revoked", token, 401)
	call("GET", "delivery-1", token, 401)
	// Another enabled trigger cannot bypass this hook creator's revoked grants.
	f.sql(`UPDATE workloads SET webhook_secret=$2 WHERE id=$1`, id, []byte("test-only-enabled-flag"))
	if err := f.s.dispatchWebhook(ctx, id); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM operations WHERE workload_id=$1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("revoked creator deployed: %d %v", count, err)
	}
	f.sql(`UPDATE team_members SET grants='[{"scope":"team","role":"editor"}]' WHERE user_id='hook-editor'`)
	rotated := f.call(editor, "default", "POST", path, nil, 201)
	if jsonString(t, rotated, "id") == hook || jsonString(t, rotated, "token") == token {
		t.Fatal("rotation did not revoke old identity")
	}
	call("POST", "old", token, 401)
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE workload_id=$1 AND operation_id IS NULL`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("rotation retained pending deliveries")
	}
	f.call(editor, "default", "DELETE", path, nil, 200)
	if err := f.s.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE actor='deploy hook' AND subject=$1`, hook).Scan(&count); err != nil || count < 5 {
		t.Fatalf("hook actions not audited %d %v", count, err)
	}
}
