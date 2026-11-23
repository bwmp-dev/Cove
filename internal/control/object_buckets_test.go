package control

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cove/internal/catalog"
	"cove/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestObjectBucketRetainsScopedCredentialsForRetryAndBindsOnlyAfterConfirmation(t *testing.T) {
	f, _ := managedDatabaseFixture(t, "")
	index, _, err := catalog.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	var definition catalog.Template
	for _, entry := range index.Templates {
		if entry.ID == "garage" {
			definition = entry
		}
	}
	draft, err := catalog.Draft(definition, nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	storage := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Garage", NodeID: "managed-node", ProjectID: "managed-project", Compose: draft.Compose, Environment: draft.Environment, Routes: []worker.Route{{Domain: "objects.example.com", Service: "garage", Port: 3900}}}, 201), "id")
	app := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Consumer", NodeID: "managed-node", ProjectID: "managed-project", Compose: "services:\n  app:\n    image: busybox:1.37\n", Environment: map[string]string{"MODE": "test"}}, 201), "id")
	operation := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+storage+"/operations", map[string]any{"action": "deploy"}, 202), "id")
	if _, err := f.s.DB.Exec(context.Background(), "UPDATE operations SET state='succeeded' WHERE id=$1", operation); err != nil {
		t.Fatal(err)
	}
	var captured worker.ObjectBucket
	fail := true
	failCheck := true
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		if r.Method != "object_bucket" || r.Bucket == nil {
			t.Fatal("unexpected RPC", r.Method)
		}
		if r.Bucket.CheckOnly {
			if failCheck {
				return worker.RPCResponse{}, errors.New("Garage authentication refused")
			}
			return worker.RPCResponse{}, nil
		}
		if captured.AccessKey != "" && captured.AccessKey != r.Bucket.AccessKey {
			t.Fatal("retry replaced scoped credentials")
		}
		captured = *r.Bucket
		if fail {
			return worker.RPCResponse{}, errors.New("ambiguous worker reply")
		}
		return worker.RPCResponse{}, nil
	}
	path := "/api/workloads/" + app + "/object-buckets"
	body := map[string]any{"storage_resource_id": storage, "domain": "objects.example.com", "confirm_same_node": true, "backup_destination": true}
	f.call("root", "default", "POST", path, body, 409)
	var count int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM object_buckets WHERE workload_id=$1", app).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed Garage preflight retained a locking bucket request", count, err)
	}
	failCheck = false
	f.call("root", "default", "POST", path, body, 502)
	if len(captured.SecretKey) != 64 || !strings.HasPrefix(captured.AccessKey, "GK") || captured.AccessKey == "GKcove00000000000000000000" {
		t.Fatal("scoped credentials not generated")
	}
	listing := f.call("root", "default", "GET", path, nil, 200)
	if strings.Contains(listing.Body.String(), captured.SecretKey) || !strings.Contains(listing.Body.String(), `"state":"pending"`) {
		t.Fatal("pending bucket exposed secrets or claimed success")
	}
	if strings.Contains(f.call("root", "default", "GET", "/api/storage-locations", nil, 200).Body.String(), "Garage bucket") {
		t.Fatal("unconfirmed destination was offered")
	}
	var bucket string
	if err = f.s.DB.QueryRow(context.Background(), "SELECT id FROM object_buckets WHERE workload_id=$1", app).Scan(&bucket); err != nil {
		t.Fatal(err)
	}
	fail = false
	f.call("root", "default", "POST", path+"/"+bucket+"/retry", map[string]any{}, 201)
	current := readDatabaseConfig(t, f, app)
	resolved, err := f.s.resolveSharedVariables(context.Background(), app, current.Environment)
	if err != nil {
		t.Fatal(err)
	}
	if resolved["S3_SECRET_ACCESS_KEY"] != captured.SecretKey || resolved["S3_BUCKET"] != captured.Bucket || resolved["S3_ENDPOINT"] != "https://objects.example.com" || resolved["S3_REGION"] != "garage" || resolved["MODE"] != "test" {
		t.Fatal("scoped bindings differ from provisioned bucket")
	}
	destinations := f.call("root", "default", "GET", "/api/storage-locations", nil, 200)
	if !strings.Contains(destinations.Body.String(), "not offsite") || strings.Contains(destinations.Body.String(), captured.SecretKey) {
		t.Fatal("destination omitted warning or disclosed credentials")
	}
	poolConfig := f.s.DB.Config()
	poolConfig.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.s.DB = pool
	rpc := f.s.workerRPC
	f.call("root", "default", "DELETE", path+"/"+bucket, map[string]any{"confirm": true}, 503)
	// HTTP reauthorization deliberately reserves three connections; the transport itself must still reuse its one held transaction.
	revoke := func(want int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, path+"/"+bucket, bytes.NewBufferString(`{"confirm":true}`))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", app)
		req.SetPathValue("bucket", bucket)
		req = req.WithContext(context.WithValue(req.Context(), identityKey{}, &identity{ID: "bootstrap", Team: "default", Admin: true}))
		response := httptest.NewRecorder()
		f.s.removeObjectBucket(response, req)
		if response.Code != want {
			t.Fatal("single-connection revocation", response.Code, response.Body.String())
		}
	}
	f.s.workerRPC = nil
	revoke(502)
	f.s.workerRPC = rpc
	revoke(200)
	if !captured.Revoke {
		t.Fatal("bucket record removed without revoking its key")
	}
	if _, exists := readDatabaseConfig(t, f, app).Environment["S3_SECRET_ACCESS_KEY"]; exists {
		t.Fatal("removed bucket left broken environment reference")
	}
}
