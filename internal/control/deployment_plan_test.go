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

func TestDeploymentPlanOrderingAndFailure(t *testing.T) {
	database := os.Getenv("COVE_TEST_DATABASE_URL")
	if database == "" {
		t.Skip("COVE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db := openTestDatabase(t, database)
	s := &Server{DB: db, Key: bytes.Repeat([]byte{9}, 32), AdminToken: strings.Repeat("a", 32), PublicURL: "https://cove.example.com"}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	node := resourceID()
	if _, err := db.Exec(ctx, "INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES($1,'fixture','127.0.0.1',22,'',$2,'ready')", node, []byte{}); err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, body any, status int) map[string]json.RawMessage {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+s.AdminToken)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var result map[string]json.RawMessage
		if status < 400 {
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	id := func(result map[string]json.RawMessage) string {
		var v string
		if err := json.Unmarshal(result["id"], &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	project := id(call("POST", "/api/projects", map[string]string{"name": "Ordered"}, 201))
	resources := []string{}
	for _, name := range []string{"migration", "api", "frontend"} {
		kind := "service"
		job := ""
		if name == "migration" {
			kind = "job"
			job = "app"
		}
		resources = append(resources, id(call("POST", "/api/workloads", workloadInput{ProjectID: project, Name: name, NodeID: node, Compose: "services:\n  app:\n    image: busybox:1.37\n", Kind: kind, JobService: job}, 201)))
	}
	path := "/api/projects/" + project + "/deployments"
	call("POST", path, map[string]any{"workload_ids": []string{resources[0], resources[0]}}, 400)
	queued := call("POST", path, map[string]any{"workload_ids": resources}, 202)
	var ops []string
	if err := json.Unmarshal(queued["operation_ids"], &ops); err != nil || len(ops) != 3 {
		t.Fatal("missing plan operations", err)
	}
	call("POST", path, map[string]any{"workload_ids": resources}, 409)
	if ready, err := s.dependencyReady(ctx, ops[0]); err != nil || !ready {
		t.Fatal("first blocked", err)
	}
	if ready, err := s.dependencyReady(ctx, ops[1]); err != nil || ready {
		t.Fatal("second ran early", err)
	}
	if _, err := db.Exec(ctx, "UPDATE operations SET state='succeeded' WHERE id=$1", ops[0]); err != nil {
		t.Fatal(err)
	}
	call("DELETE", "/api/workloads/"+resources[0], nil, 409)
	if ready, err := s.dependencyReady(ctx, ops[1]); err != nil || !ready {
		t.Fatal("success did not release dependency", err)
	}
	if _, err := db.Exec(ctx, "UPDATE operations SET state='failed' WHERE id=$1", ops[1]); err != nil {
		t.Fatal(err)
	}
	if ready, err := s.dependencyReady(ctx, ops[2]); err != nil || ready {
		t.Fatal("failed prerequisite released dependency", err)
	}
	var state string
	if err := db.QueryRow(ctx, "SELECT state FROM operations WHERE id=$1", ops[2]).Scan(&state); err != nil || state != "cancelled" {
		t.Fatal("dependent not cancelled", err, state)
	}
}
