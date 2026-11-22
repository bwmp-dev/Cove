package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestWorkloadConfigurationIntegration(t *testing.T) {
	database := os.Getenv("COVE_TEST_DATABASE_URL")
	if database == "" {
		t.Skip("COVE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db := openTestDatabase(t, database)
	var err error
	s := &Server{DB: db, Key: bytes.Repeat([]byte{5}, 32), AdminToken: strings.Repeat("c", 32), PublicURL: "https://cove.example.com"}
	if err = s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	node := resourceID()
	if _, err = db.Exec(ctx, "INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES($1,'fixture','127.0.0.1',22,'',$2,'ready')", node, []byte{}); err != nil {
		t.Fatal(err)
	}
	var id string
	defer func() {
		db.Exec(ctx, "DELETE FROM operations WHERE workload_id=$1", id)
		db.Exec(ctx, "DELETE FROM workloads WHERE id=$1", id)
		db.Exec(ctx, "DELETE FROM nodes WHERE id=$1", node)
	}()
	handler := s.Handler()
	call := func(method, path string, input any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(input)
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+s.AdminToken)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if _, err = db.Exec(ctx, "INSERT INTO projects(id,name,team_id) VALUES('p','Fixture','default')"); err != nil {
		t.Fatal(err)
	}
	input := workloadInput{ProjectID: "p", Name: "fixture", NodeID: node, Compose: "services:\n  app:\n    image: busybox:1.37\n", Environment: map[string]string{"APP_SECRET": "fixture-secret-value"}}
	response := call("POST", "/api/workloads", input)
	if response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	var created map[string]string
	json.Unmarshal(response.Body.Bytes(), &created)
	id = created["id"]
	defer db.Exec(ctx, "DELETE FROM api_tokens WHERE workload_id=$1", id)
	response = call("POST", "/api/tokens", map[string]any{"name": "test reader", "workload_id": id, "permission": "read", "days": 1})
	if response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	var issued struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	scoped := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+issued.Token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if scoped("GET", "/api/workloads/"+id).Code != 200 {
		t.Fatal("scoped workload read denied")
	}
	for _, path := range []string{"/api/nodes", "/api/tokens", "/api/audit", "/api/workloads/other", "/api/operations"} {
		if scoped("GET", path).Code != 403 {
			t.Fatal("scope escape", path)
		}
	}
	if scoped("POST", "/api/workloads/"+id+"/operations").Code != 403 {
		t.Fatal("read token allowed mutation")
	}
	response = call("GET", "/api/tokens", nil)
	if strings.Contains(response.Body.String(), issued.Token) {
		t.Fatal("token list leaked credential")
	}
	response = call("DELETE", "/api/tokens/"+issued.ID, nil)
	if response.Code != 200 || scoped("GET", "/api/workloads/"+id).Code != 401 {
		t.Fatal("revocation failed")
	}
	response = call("GET", "/api/workloads/"+id, nil)
	if response.Code != 200 || strings.Contains(response.Body.String(), "fixture-secret-value") || !strings.Contains(response.Body.String(), "APP_SECRET") {
		t.Fatal("detail leaked or lost secret metadata")
	}
	input.ProjectID = ""
	input.Name = "renamed"
	input.Environment = nil
	addedValue := "new-secret-value"
	input.EnvironmentPatch = map[string]*string{"SECOND_SECRET": &addedValue}
	response = call("PUT", "/api/workloads/"+id, input)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response = call("GET", "/api/workloads/"+id, nil)
	if !strings.Contains(response.Body.String(), "SECOND_SECRET") || strings.Contains(response.Body.String(), addedValue) {
		t.Fatal("per-key update leaked or lost value")
	}
	input.EnvironmentPatch = map[string]*string{"SECOND_SECRET": nil}
	response = call("PUT", "/api/workloads/"+id, input)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	input.EnvironmentPatch = nil
	response = call("GET", "/api/workloads/"+id, nil)
	if strings.Contains(response.Body.String(), "SECOND_SECRET") || !strings.Contains(response.Body.String(), "APP_SECRET") {
		t.Fatal("per-key removal changed unrelated secrets")
	}
	response = call("POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "deploy"})
	if response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	response = call("POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "deploy"})
	if response.Code != 409 {
		t.Fatal("duplicate active deployment accepted")
	}
	response = call("PUT", "/api/workloads/"+id, input)
	if response.Code != 409 {
		t.Fatal("configuration changed under active deployment")
	}
	var encrypted []byte
	if err = db.QueryRow(ctx, "SELECT payload FROM operations WHERE workload_id=$1", id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	plain, err := unseal(s.Key, encrypted)
	if err != nil || !strings.Contains(string(plain), "fixture-secret-value") {
		t.Fatal("editing erased existing secret")
	}
	if bytes.Contains(encrypted, []byte("fixture-secret-value")) {
		t.Fatal("operation secret stored unencrypted")
	}
	response = call("GET", "/api/operations", nil)
	if response.Code != 200 || strings.Contains(response.Body.String(), "fixture-secret-value") {
		t.Fatal("operation list leaked secret")
	}
	var operationID string
	if err = db.QueryRow(ctx, "SELECT id FROM operations WHERE workload_id=$1", id).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	response = call("POST", "/api/operations/"+operationID+"/cancel", nil)
	if response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	var cancelRequested bool
	if err = db.QueryRow(ctx, "SELECT cancel_requested FROM operations WHERE id=$1", operationID).Scan(&cancelRequested); err != nil || !cancelRequested {
		t.Fatal("cancellation was not durable", err)
	}
	response = call("GET", "/api/operations/"+operationID, nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"cancel_requested":true`) {
		t.Fatal("cancellation status missing")
	}
}
