package control

import (
	"context"
	"cove/internal/worker"
	"strings"
	"testing"
	"time"
)

func TestFirewallRemovalRequiresCapabilityAndFreshConfirmation(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('remove-firewall','Node','localhost',2222,'',$1,'ready','{"firewall":true}')`, []byte{})
	path := "/api/nodes/remove-firewall/firewall"
	preview := map[string]any{"remove": true}
	apply := map[string]any{"remove": true, "confirm": true, "revision": strings.Repeat("a", 64)}
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		t.Fatal("older worker was asked to remove its firewall")
		return worker.RPCResponse{}, nil
	}
	f.call("root", "default", "POST", path+"/preview", preview, 409)
	f.call("root", "default", "POST", path, apply, 409)
	f.sql(`UPDATE nodes SET capabilities='{"firewall":true,"firewall_remove":true}' WHERE id='remove-firewall'`)
	f.call("root", "default", "POST", path+"/preview", map[string]any{"remove": true, "rules": []worker.FirewallRule{{Protocol: "tcp", Port: 8080}}}, 400)
	f.call("root", "default", "POST", path, map[string]any{"remove": true, "revert": true, "confirm": true}, 400)
	probeReady := true
	calls := []string{}
	f.s.workerRPC = func(_ context.Context, node string, request worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "remove-firewall" {
			t.Fatal(node)
		}
		if request.Method == "probe" {
			calls = append(calls, "probe")
			return worker.RPCResponse{Probe: &worker.Probe{Protocol: 1, Ready: probeReady}}, nil
		}
		control := request.Firewall
		if control == nil || !control.Remove || len(control.Rules) != 0 {
			t.Fatal("removal intent lost", request)
		}
		calls = append(calls, control.Action)
		state := &worker.FirewallResult{Supported: true, RemoveSupported: true, Rules: []worker.FirewallRule{}, Revision: strings.Repeat("a", 64)}
		if control.Action == "apply" {
			state.Managed = true
			state.Pending = &worker.FirewallPending{ID: strings.Repeat("b", 32), Deadline: time.Now().Add(2 * time.Minute), Remove: true}
		}
		if (control.Action == "confirm" || control.Action == "revert") && control.Intent != strings.Repeat("b", 32) {
			t.Fatal("removal confirmed another attempt", control.Intent)
		}
		return worker.RPCResponse{Firewall: state}, nil
	}
	f.call("root", "default", "POST", path+"/preview", preview, 200)
	calls = nil
	f.call("root", "default", "POST", path, apply, 200)
	if strings.Join(calls, ",") != "apply,probe,confirm" {
		t.Fatal("removal lacked a fresh connection confirmation", calls)
	}
	probeReady, calls = false, nil
	f.call("root", "default", "POST", path, apply, 502)
	if strings.Join(calls, ",") != "apply,probe,revert" {
		t.Fatal("failed connectivity did not restore the policy", calls)
	}
}
