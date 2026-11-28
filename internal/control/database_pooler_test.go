package control

import (
	"context"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestDatabasePoolingRequiresReviewAndPreservesBoundCredentialsOnRotation(t *testing.T) {
	f, id := managedDatabaseFixture(t, "")
	f.sql(`UPDATE nodes SET capabilities=capabilities || '{"private_links":true}'::jsonb WHERE id='managed-node'`)
	path := "/api/workloads/" + id + "/database/pooler"
	revision := func() string {
		return jsonString(t, f.call("root", "default", "GET", "/api/workloads/"+id+"/database", nil, 200), "revision")
	}
	f.call("root", "default", "POST", path, map[string]any{"enabled": true, "revision": revision()}, 400)
	f.call("root", "default", "POST", path, map[string]any{"enabled": true, "confirm": true, "revision": "old"}, 409)
	task := jsonString(t, f.call("root", "default", "POST", path, map[string]any{"enabled": true, "confirm": true, "revision": revision()}, 202), "id")
	config := readDatabaseConfig(t, f, id)
	if !config.Database.Pooler || config.Database.SettingsOperation != task || !strings.Contains(config.Compose, "scram-sha-256") || strings.Contains(config.Compose, config.Environment["POSTGRES_PASSWORD"]) {
		t.Fatal("pooler state, authentication or secret isolation incorrect")
	}
	values, err := databaseExports(config, nil)
	if err != nil || values["DB_POOL_PORT"] != "6432" || values["DB_POOL_PASSWORD"] != values["DB_PASSWORD"] || values["DB_POOL_HOST"] == values["DB_HOST"] {
		t.Fatal("pooled credentials differ from database", err)
	}
	f.sql("UPDATE operations SET state='succeeded' WHERE id=$1", task)
	app := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Pooled consumer", NodeID: "managed-node", ProjectID: "managed-project", Compose: "services:\n  app:\n    image: busybox:1.37\n"}, 201), "id")
	f.call("root", "default", "PUT", "/api/workloads/"+app+"/variable-bindings", resourceVariableBinding{Name: "DATABASE", SourceID: id, SourceKey: "DB_POOL_URL"}, 200)
	link := map[string]any{"source_id": app, "source_service": "app", "target_id": id, "target_service": "pgbouncer", "port": 6432, "alias": values["DB_POOL_HOST"]}
	connection := jsonString(t, f.call("root", "default", "POST", "/api/private-connections", link, 201), "id")
	link["alias"] = "wrong.internal"
	f.call("root", "default", "PUT", "/api/private-connections/"+connection, link, 400)
	f.call("root", "default", "POST", path, map[string]any{"enabled": false, "confirm": true, "revision": revision()}, 409)
	maintenance := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/database/maintenance", map[string]any{"kind": "rotate", "revision": revision(), "confirm": true}, 202), "id")
	var encrypted []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT payload FROM operations WHERE id=$1", maintenance).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	var request worker.Request
	if err := f.s.unsealJSON(encrypted, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.FileSecrets) != 1 || !strings.Contains(request.Environment["COVE_POOLER_AUTH"], request.Environment["POSTGRES_PASSWORD"]) || request.Environment["POSTGRES_PASSWORD"] == config.Environment["POSTGRES_PASSWORD"] {
		t.Fatal("rotation did not update the pooler authentication file")
	}
	if err := worker.ValidateRequest(request); err != nil {
		t.Fatal(err)
	}
}
