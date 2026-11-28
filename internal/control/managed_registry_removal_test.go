package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func registryRemovalFixture(t *testing.T) (*accountFixture, string) {
	t.Helper()
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('registry-node','Registry','localhost',22,'',$1,'ready','{"managed_registry":true,"file_secrets":true}')`, []byte{})
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('registry-project','Registry project','default')`)
	created := f.call("root", "default", "POST", "/api/managed-registries", managedRegistryInput{Name: "Registry", Node: "registry-node", Project: "registry-project", Hostname: "registry.example.test"}, 202)
	id := jsonString(t, created, "id")
	f.sql(`UPDATE operations SET state='succeeded' WHERE workload_id=$1`, id)
	f.sql(`UPDATE managed_registries SET state='ready' WHERE id=$1`, id)
	return f, id
}

func addRegistryRemovalWorkload(t *testing.T, f *accountFixture, id, node string) {
	t.Helper()
	config, err := f.s.sealJSON(workloadInput{Name: id, NodeID: node, Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(`INSERT INTO workloads(id,name,node_id,configuration,kind,team_id,project_id) VALUES($1,$1,$2,$3,'service','default','registry-project')`, id, node, config)
}

func TestManagedRegistryRemoveKeepingDataWithUnavailableNode(t *testing.T) {
	f, id := registryRemovalFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,host_key,state) VALUES('offline-node','Offline builder','localhost',22,'',$1,'pinned-fixture-identity','unreachable')`, []byte{})
	addRegistryRemovalWorkload(t, f, "consumer", "offline-node")
	inventories := 0
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		inventories++
		return worker.RPCResponse{}, errors.New("offline fixture")
	}
	response := f.call("root", "default", "DELETE", "/api/managed-registries/"+id, map[string]bool{"delete_data": false}, 202)
	if inventories != 0 {
		t.Fatal("keep-data removal requested a reference inventory")
	}
	var encrypted []byte
	if err := f.s.DB.QueryRow(t.Context(), `SELECT payload FROM operations WHERE id=$1`, jsonString(t, response, "operation_id")).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	var request worker.Request
	if err := f.s.unsealJSON(encrypted, &request); err != nil || request.ManagedRegistry == nil || request.ManagedRegistry.Mode != "remove" || request.ManagedRegistry.DeleteData {
		t.Fatal("keep-data removal changed its safety boundary", err)
	}
}

func TestManagedRegistryRemovalRequiresExactUnavailableNodeSet(t *testing.T) {
	f, id := registryRemovalFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,host_key,state) VALUES('offline-node','Offline builder','localhost',22,'',$1,'pinned-fixture-identity','unreachable')`, []byte{})
	addRegistryRemovalWorkload(t, f, "consumer", "offline-node")
	preview := f.call("root", "default", "GET", "/api/managed-registries/"+id+"/removal-inventory", nil, 200)
	if !strings.Contains(preview.Body.String(), `"name":"Offline builder"`) {
		t.Fatal("preview omitted node name", preview.Body.String())
	}
	refused := f.call("root", "default", "DELETE", "/api/managed-registries/"+id, map[string]any{"delete_data": true}, 409)
	if !strings.Contains(refused.Body.String(), `"error":"registry_inventory_changed"`) || !strings.Contains(refused.Body.String(), "Offline builder") {
		t.Fatal("refusal omitted current node identity", refused.Body.String())
	}
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,host_key,state) VALUES('new-offline','Other runner','localhost',22,'',$1,'another-pinned-identity','unreachable')`, []byte{})
	addRegistryRemovalWorkload(t, f, "second-consumer", "new-offline")
	stale := f.call("root", "default", "DELETE", "/api/managed-registries/"+id, map[string]any{"delete_data": true, "acknowledge_unavailable_nodes": []string{"offline-node"}}, 409)
	if !strings.Contains(stale.Body.String(), "Other runner") {
		t.Fatal("changed inventory did not name added node", stale.Body.String())
	}
	accepted := f.call("root", "default", "DELETE", "/api/managed-registries/"+id, map[string]any{"delete_data": true, "acknowledge_unavailable_nodes": []string{"offline-node", "new-offline"}}, 202)
	operation := jsonString(t, accepted, "operation_id")
	var reason string
	var audit []byte
	if err := f.s.DB.QueryRow(t.Context(), `SELECT reason FROM operations WHERE id=$1`, operation).Scan(&reason); err != nil || !strings.Contains(reason, "2 unavailable nodes") {
		t.Fatal("acknowledgement not recorded on operation", reason, err)
	}
	if err := f.s.DB.QueryRow(t.Context(), `SELECT details FROM audit_events WHERE action='registry data deletion acknowledgement' AND subject=$1`, id).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Offline builder", "Other runner", "offline-node", "new-offline"} {
		if !strings.Contains(string(audit), name) {
			t.Fatal("audit lost acknowledged node", name)
		}
	}
}

func TestManagedRegistryNeverEnrolledNodesDoNotMakeInventoryIncomplete(t *testing.T) {
	for _, state := range []string{"pending", "unreachable"} {
		t.Run(state, func(t *testing.T) {
			f, id := registryRemovalFixture(t)
			f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('new-node','Never enrolled','localhost',22,'',$1,$2,'null')`, []byte{}, state)
			addRegistryRemovalWorkload(t, f, "new-resource", "new-node")
			f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
				t.Fatal("RPC attempted without a pinned worker identity")
				return worker.RPCResponse{}, nil
			}
			preview := f.call("root", "default", "GET", "/api/managed-registries/"+id+"/removal-inventory", nil, 200)
			if !strings.Contains(preview.Body.String(), `"unavailable_nodes":[]`) {
				t.Fatal("never-enrolled node required acknowledgement", preview.Body.String())
			}
			gc := f.call("root", "default", "POST", "/api/managed-registries/"+id+"/gc", nil, 202)
			f.sql(`UPDATE operations SET state='succeeded' WHERE id=$1`, jsonString(t, gc, "operation_id"))
			f.sql(`UPDATE managed_registries SET state='ready' WHERE id=$1`, id)
			f.call("root", "default", "DELETE", "/api/managed-registries/"+id, map[string]bool{"delete_data": true}, 202)
		})
	}
}

func TestManagedRegistryAcknowledgementCannotBypassCompleteInventoryRules(t *testing.T) {
	for _, scenario := range []string{"gc", "ready-worker-error", "known-release", "known-build", "malformed-ack"} {
		t.Run(scenario, func(t *testing.T) {
			f, id := registryRemovalFixture(t)
			f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,host_key,state,capabilities) VALUES('offline-node','Offline builder','localhost',22,'',$1,'pinned-fixture-identity','unreachable','null')`, []byte{})
			addRegistryRemovalWorkload(t, f, "consumer", "offline-node")
			f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
				return worker.RPCResponse{}, nil
			}
			input := map[string]any{"delete_data": true, "acknowledge_unavailable_nodes": []string{"offline-node"}}
			status := 409
			switch scenario {
			case "gc":
				refused := f.call("root", "default", "POST", "/api/managed-registries/"+id+"/gc", nil, 409)
				if strings.Contains(refused.Body.String(), "registry_inventory_changed") || !strings.Contains(refused.Body.String(), "inventory is incomplete") {
					t.Fatal("GC incorrectly offered a data-deletion acknowledgement", refused.Body.String())
				}
				return
			case "ready-worker-error":
				f.sql(`UPDATE nodes SET state='ready',capabilities='{}' WHERE id='offline-node'`)
				input["acknowledge_unavailable_nodes"] = []string{}
				f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
					return worker.RPCResponse{}, errors.New("retained image unreadable")
				}
			case "known-release":
				f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('ready-node','Ready worker','localhost',22,'',$1,'ready','{}')`, []byte{})
				addRegistryRemovalWorkload(t, f, "known-consumer", "ready-node")
				f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
					return worker.RPCResponse{RegistryReferences: []string{"app@sha256:" + strings.Repeat("a", 64)}}, nil
				}
			case "known-build":
				report, _ := json.Marshal(worker.Operation{Build: &worker.BuildResult{Images: []worker.BuiltImage{{Reference: "registry.example.test/app@sha256:" + strings.Repeat("a", 64)}}}})
				f.sql(`INSERT INTO operations(id,workload_id,action,state,payload,result) VALUES('old-build','consumer','build','succeeded',$1,$2)`, []byte{}, report)
			case "malformed-ack":
				input["acknowledge_unavailable_nodes"] = []string{"offline-node", "offline-node"}
				status = 400
			}
			f.call("root", "default", "DELETE", "/api/managed-registries/"+id, input, status)
		})
	}
}

func TestManagedRegistryRemovalInventoryScopesAndAuthority(t *testing.T) {
	f, id := registryRemovalFixture(t)
	owner := f.user("removal-owner", "removal-owner@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','removal-owner','owner')`)
	_, read := mintPublic(t, f, owner, "user", "workloads:read")
	_, write := mintPublic(t, f, owner, "user", "workloads:write")
	path := "/api/v1/managed-registries/" + id + "/removal-inventory"
	publicCall(t, f, read, "default", "GET", path, nil, 403)
	publicCall(t, f, write, "default", "GET", path, nil, 200)
	publicCall(t, f, read, "default", "DELETE", "/api/v1/managed-registries/"+id, map[string]bool{"delete_data": true}, 403)
	member := f.user("removal-member", "removal-member@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role) VALUES('default','removal-member','member')`)
	f.call(member, "default", "GET", "/api/managed-registries/"+id+"/removal-inventory", nil, 403)
	f.sql(`INSERT INTO teams(id,name) VALUES('foreign-registry-team','Other team')`)
	f.call("root", "foreign-registry-team", "GET", "/api/managed-registries/"+id+"/removal-inventory", nil, 404)
}

func TestManagedRegistryInventoryDoesNotLockPublisherWorkload(t *testing.T) {
	f, id := registryRemovalFixture(t)
	addRegistryRemovalWorkload(t, f, "consumer", "registry-node")
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{RegistryReferences: []string{}}, nil
	}
	inventory, err := f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer inventory.Rollback(t.Context())
	var locked string
	if err = inventory.QueryRow(t.Context(), `SELECT id FROM managed_registries WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	publisher, err := f.s.DB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Rollback(t.Context())
	if err = publisher.QueryRow(t.Context(), `SELECT id FROM workloads WHERE id='consumer' FOR UPDATE`).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, err = f.s.managedRegistryReferences(ctx, inventory, "default", "registry.example.test", time.Now()); err != nil {
		t.Fatal("read-only registry inventory waited for a publisher workload row", err)
	}
}
