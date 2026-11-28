package control

import (
	"context"
	"cove/internal/worker"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFirewallConfirmationUsesFreshProbeAndRevertsFailure(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('firewall-node','Node','localhost',2222,'',$1,'ready','{"firewall":true}')`, []byte{})
	user := f.user("firewall-owner", "firewall-owner@example.com")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','firewall-owner','owner')`)
	path := "/api/nodes/firewall-node/firewall"
	f.call(user, "default", "GET", path, nil, 403)
	f.call(user, "default", "POST", path+"/preview", map[string]any{"rules": []worker.FirewallRule{}}, 403)
	f.call("root", "default", "POST", path, map[string]any{"revision": strings.Repeat("a1", 32)}, 400)
	f.call("root", "default", "POST", path, map[string]any{"confirm": true, "revision": "stale"}, 400)
	f.call("root", "default", "POST", path+"/preview", map[string]any{"available": []worker.FirewallPort{{Protocol: "tcp", Port: 2222}}}, 400)
	intent := strings.Repeat("b2", 16)
	probeFails, revertFails := false, false
	calls := []string{}
	f.s.workerRPC = func(_ context.Context, node string, request worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "firewall-node" {
			t.Fatal("firewall targeted another node")
		}
		if request.Method == "probe" {
			calls = append(calls, "probe")
			if probeFails {
				return worker.RPCResponse{}, errors.New("synthetic disconnected management path")
			}
			return worker.RPCResponse{Probe: &worker.Probe{Protocol: 1, Ready: true}}, nil
		}
		if request.Method != "firewall" || request.Firewall == nil || len(request.Firewall.Available) != 0 {
			t.Fatal("incorrect firewall request", request)
		}
		control := request.Firewall
		calls = append(calls, control.Action)
		for _, port := range []uint16{22, 80, 443, 2222} {
			if !slices.Contains(control.Reserved, port) {
				t.Fatal("management port was not protected", port)
			}
		}
		state := &worker.FirewallResult{Supported: true, Rules: control.Rules, Revision: strings.Repeat("a1", 32)}
		if control.Action == "apply" {
			state.Active, state.Pending = true, &worker.FirewallPending{ID: intent, Deadline: time.Now().Add(2 * time.Minute)}
		}
		if control.Action == "confirm" || control.Action == "revert" {
			if control.Intent != intent {
				t.Fatal("connectivity receipt confirmed another intent")
			}
			if control.Action == "revert" && revertFails {
				return worker.RPCResponse{}, errors.New("synthetic rollback reply lost")
			}
		}
		return worker.RPCResponse{Firewall: state}, nil
	}
	response := f.call("root", "default", "GET", path, nil, 200)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("firewall inspection was cacheable")
	}
	f.call("root", "default", "POST", path+"/preview", map[string]any{"rules": []worker.FirewallRule{}}, 200)
	body := map[string]any{"confirm": true, "revision": strings.Repeat("a1", 32), "rules": []worker.FirewallRule{}}
	calls = nil
	f.call("root", "default", "POST", path, body, 200)
	if strings.Join(calls, ",") != "apply,probe,confirm" {
		t.Fatal("firewall was confirmed without a fresh connection", calls)
	}
	probeFails, calls = true, nil
	response = f.call("root", "default", "POST", path, body, 502)
	if strings.Join(calls, ",") != "apply,probe,revert" || !strings.Contains(response.Body.String(), "was reverted") {
		t.Fatal("connectivity failure was not reverted", calls, response.Body.String())
	}
	revertFails = true
	response = f.call("root", "default", "POST", path, body, 502)
	if !strings.Contains(response.Body.String(), "could not be confirmed") {
		t.Fatal("ambiguous recovery was presented as completed", response.Body.String())
	}
	var count int
	if err := f.s.DB.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action='POST /api/nodes/{id}/firewall' AND subject='firewall-node' AND status IN (200,502)`).Scan(&count); err != nil || count != 3 {
		t.Fatal("firewall mutations were not audited", count, err)
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='firewall-node'`)
	f.call("root", "default", "GET", path, nil, 409)
}
