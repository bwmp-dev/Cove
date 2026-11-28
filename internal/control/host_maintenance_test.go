package control

import (
	"context"
	"cove/internal/worker"
	"errors"
	"testing"
)

func TestHostMaintenanceAdminScopeConfirmationAndAudit(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('host-policy-node','Node','localhost',22,'',$1,'ready','{"host_maintenance":true}')`, []byte{})
	member := f.user("host-policy-owner", "host-policy@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','host-policy-owner','owner')`)
	path := "/api/nodes/host-policy-node/host-maintenance"
	f.call(member, "default", "GET", path, nil, 403)
	f.call(member, "default", "PUT", path, map[string]any{"enabled": true, "confirm": true}, 403)
	f.call("root", "default", "PUT", path, map[string]bool{"enabled": true}, 400)
	f.call("root", "default", "PUT", path, map[string]bool{"confirm": true}, 400)
	calls := 0
	f.s.workerRPC = func(_ context.Context, node string, request worker.RPCRequest) (worker.RPCResponse, error) {
		calls++
		if node != "host-policy-node" || request.Method != "host_maintenance" || request.HostMaintenance == nil || request.HostMaintenance.Root != "" || request.HostMaintenance.Owner != "" {
			t.Fatal("host request crossed the trusted worker boundary", request)
		}
		if request.HostMaintenance.Action == "security_policy" && !request.HostMaintenance.Enabled {
			return worker.RPCResponse{}, errors.New("ambiguous transport failure")
		}
		return worker.RPCResponse{HostPatches: &worker.HostPatchStatus{Manager: "apt", PolicyEnabled: request.HostMaintenance.Enabled}}, nil
	}
	response := f.call("root", "default", "GET", path, nil, 200)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("host inspection cacheable")
	}
	f.call("root", "default", "PUT", path, map[string]bool{"enabled": true, "confirm": true}, 200)
	f.call("root", "default", "PUT", path, map[string]bool{"enabled": false, "confirm": true}, 502)
	if calls != 3 {
		t.Fatal("request unexpectedly retried", calls)
	}
	var audited int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action='PUT /api/nodes/{id}/host-maintenance' AND subject='host-policy-node' AND status IN (200,502)`).Scan(&audited); err != nil || audited != 2 {
		t.Fatal("schedule actions were not audited", audited, err)
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='host-policy-node'`)
	f.call("root", "default", "GET", path, nil, 409)
	f.call("root", "default", "GET", "/api/nodes/missing/host-maintenance", nil, 404)
}
