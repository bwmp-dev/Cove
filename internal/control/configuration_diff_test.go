package control

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cove/internal/worker"
)

func TestConfigurationDiffUsesImmutableDeclarationAndRollbackTarget(t *testing.T) {
	f, id := variablesFixture(t)
	path := "/api/workloads/" + id
	initial := f.call("root", "default", "GET", path+"/configuration-diff", nil, 200)
	if !strings.Contains(initial.Body.String(), `"has_baseline":false`) {
		t.Fatal("undeployed resource claimed a baseline")
	}
	operation := jsonString(t, f.call("root", "default", "POST", path+"/operations", map[string]string{"action": "deploy"}, 202), "id")
	var encrypted []byte
	if err := f.s.DB.QueryRow(context.Background(), `SELECT payload FROM operations WHERE id=$1`, operation).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	var prepared worker.Request
	if err := f.s.unsealJSON(encrypted, &prepared); err != nil {
		t.Fatal(err)
	}
	prepared.Environment["SECRET"] = "resolved-private-value"
	encrypted, err := f.s.sealJSON(prepared)
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`UPDATE operations SET payload=$2,state='succeeded' WHERE id=$1`, operation, encrypted)
	unchanged := f.call("root", "default", "GET", path+"/configuration-diff", nil, 200)
	if !strings.Contains(unchanged.Body.String(), `"fields":[]`) || !strings.Contains(unchanged.Body.String(), `"has_baseline":true`) {
		t.Fatal("prepared secret snapshot changed declared configuration diff", unchanged.Body.String())
	}
	f.call("root", "default", "POST", path+"/variables/import", map[string]string{"dotenv": "SECRET=next-private-value"}, 200)
	rollback := jsonString(t, f.call("root", "default", "POST", path+"/operations", map[string]string{"action": "rollback", "release": operation}, 202), "id")
	f.sql(`UPDATE operations SET state='succeeded',updated_at=now()+interval '1 second' WHERE id=$1`, rollback)
	diff := f.call("root", "default", "GET", path+"/configuration-diff", nil, 200)
	var result map[string]json.RawMessage
	if json.Unmarshal(diff.Body.Bytes(), &result) != nil || !strings.Contains(diff.Body.String(), "environment.SECRET") || strings.Contains(diff.Body.String(), "private-value") || strings.Contains(diff.Body.String(), "saved-secret") {
		t.Fatal("rollback diff did not use its original declaration or exposed values", diff.Body.String())
	}
}

func TestDriftCapabilityPreparationRemainsImmutableAcrossRetries(t *testing.T) {
	f, id := variablesFixture(t)
	op := jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "deploy"}, 202), "id")
	load := func() worker.Request {
		t.Helper()
		var encrypted []byte
		var request worker.Request
		if err := f.s.DB.QueryRow(context.Background(), `SELECT payload FROM operations WHERE id=$1`, op).Scan(&encrypted); err != nil {
			t.Fatal(err)
		}
		if err := f.s.unsealJSON(encrypted, &request); err != nil {
			t.Fatal(err)
		}
		return request
	}
	request := load()
	if err := f.s.prepareInfrastructure(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(request)
	if request.RecordDrift || bytes.Contains(first, []byte("record_drift")) {
		t.Fatal("old worker received an unsupported request field")
	}
	f.sql(`UPDATE nodes SET capabilities=capabilities||'{"drift":true}'::jsonb WHERE id='variable-node'`)
	retry := load()
	if err := f.s.prepareInfrastructure(context.Background(), &retry); err != nil {
		t.Fatal(err)
	}
	second, _ := json.Marshal(retry)
	if !bytes.Equal(first, second) {
		t.Fatal("capability refresh changed the immutable worker request")
	}
	f.sql(`UPDATE operations SET state='succeeded' WHERE id=$1`, op)
	op = jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "deploy"}, 202), "id")
	request = load()
	if err := f.s.prepareInfrastructure(context.Background(), &request); err != nil || !request.RecordDrift {
		t.Fatal("new operation did not opt in after a worker upgrade", err)
	}
	f.sql(`UPDATE operations SET state='succeeded' WHERE id=$1`, op)
	for _, action := range []string{"deploy", "restart"} {
		op = resourceID()
		legacy := worker.Request{ID: op, Workload: id, Action: action, Compose: "services: {}", InfrastructurePrepared: action == "deploy"}
		payload, err := f.s.sealJSON(legacy)
		if err != nil {
			t.Fatal(err)
		}
		f.sql(`INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,$3,$4)`, op, id, action, payload)
		before, _ := json.Marshal(legacy)
		if err = f.s.prepareInfrastructure(context.Background(), &legacy); err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(legacy)
		if !bytes.Equal(before, after) {
			t.Fatal("upgrade changed a legacy prepared worker request")
		}
		f.sql(`UPDATE operations SET state='succeeded' WHERE id=$1`, op)
	}
	op = jsonString(t, f.call("root", "default", "POST", "/api/workloads/"+id+"/operations", map[string]string{"action": "restart"}, 202), "id")
	request = load()
	if !request.RecordDrift {
		t.Fatal("new restart did not opt in at creation")
	}
}
