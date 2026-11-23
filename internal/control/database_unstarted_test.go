package control

import "testing"

func TestDatabaseMaintenanceRefusesUndeployedAndStoppedResourcesWithoutLocking(t *testing.T) {
	f, id := managedDatabaseFixture(t, "")
	f.sql("UPDATE operations SET state='failed' WHERE workload_id=$1", id)
	revision := jsonString(t, f.call("root", "default", "GET", "/api/workloads/"+id+"/database", nil, 200), "revision")
	body := map[string]any{"kind": "rotate", "revision": revision, "confirm": true}
	f.call("root", "default", "POST", "/api/workloads/"+id+"/database/maintenance", body, 409)
	if f.s.databaseLocked(t.Context(), id) {
		t.Fatal("undeployed database was locked by refused maintenance")
	}
	f.sql("UPDATE operations SET state='succeeded' WHERE workload_id=$1", id)
	f.sql("UPDATE workloads SET manually_stopped=true WHERE id=$1", id)
	f.call("root", "default", "POST", "/api/workloads/"+id+"/database/maintenance", body, 409)
	if f.s.databaseLocked(t.Context(), id) {
		t.Fatal("stopped database was locked by refused maintenance")
	}
}
