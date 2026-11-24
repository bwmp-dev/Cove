package control

import (
	"context"
	"cove/internal/worker"
	"errors"
	"testing"
)

func TestBuildCacheNodeAuthorizationAndExecutionAdmission(t *testing.T) {
	f := newAccountFixture(t)
	for _, node := range []string{"cache-node", "runtime-node"} {
		f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES($1,$1,'localhost',22,'',$2,'ready','{"build_cache_limits":true}')`, node, []byte{})
	}
	viewer := f.user("cache-viewer", "cache-viewer@example.test")
	f.sql(`INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','cache-viewer','member','[]')`)
	path := "/api/nodes/cache-node/build-cache"
	f.call(viewer, "default", "GET", path, nil, 403)
	f.call(viewer, "default", "PUT", path, map[string]string{"path": "/srv/cache"}, 403)
	calls := 0
	f.s.workerRPC = func(_ context.Context, node string, request worker.RPCRequest) (worker.RPCResponse, error) {
		calls++
		if node != "cache-node" {
			t.Fatal(node)
		}
		status := &worker.BuildCacheStatus{Supported: true, CanConfigure: true}
		if request.Method == "build_cache_configure" {
			if request.BuildCache == nil || request.BuildCache.Path != "/srv/cache" {
				t.Fatal("settings changed", request.BuildCache)
			}
			status.Configured, status.Verified, status.CapacityBytes = true, true, 64*1024*1024
		} else if request.Method != "build_cache" {
			t.Fatal(request.Method)
		}
		return worker.RPCResponse{BuildCache: status}, nil
	}
	f.call("root", "default", "GET", path, nil, 200)
	f.call("root", "default", "PUT", path, map[string]string{"path": "/srv/cache"}, 200)
	f.call("root", "default", "PUT", path, map[string]any{"path": "/srv/cache", "capacity_bytes": 1}, 400)
	f.sql(`INSERT INTO projects(id,name,team_id) VALUES('cache-project','Cache project','default')`)
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "Cache app", NodeID: "runtime-node", ProjectID: "cache-project", Kind: "service", Compose: "services:\n  app:\n    image: busybox:1.37\n"}, 201), "id")
	f.sql(`INSERT INTO operations(id,workload_id,action,payload,execution_node_id) VALUES('active-cache-build',$1,'build',$2,'cache-node')`, id, []byte{})
	response := f.call("root", "default", "GET", path, nil, 200)
	if jsonString(t, response, "active_operation_id") != "active-cache-build" {
		t.Fatal(response.Body.String())
	}
	before := calls
	f.call("root", "default", "PUT", path, map[string]string{"path": ""}, 409)
	if calls != before {
		t.Fatal("active execution node was configured")
	}
	f.sql(`UPDATE nodes SET capabilities='{}' WHERE id='cache-node'`)
	f.call("root", "default", "GET", path, nil, 409)
	if calls != before {
		t.Fatal("unsupported worker treated as verified")
	}
}

func TestBuildCacheDoesNotInventVerificationAfterWorkerFailure(t *testing.T) {
	f := newAccountFixture(t)
	f.sql(`INSERT INTO nodes(id,name,address,port,public_key,private_key,state,capabilities) VALUES('cache-node','Cache','localhost',22,'',$1,'ready','{"build_cache_limits":true}')`, []byte{})
	path := "/api/nodes/cache-node/build-cache"
	for _, test := range []struct {
		err    error
		status int
	}{{&RemoteError{Message: "unsupported method"}, 409}, {errors.New("fixture unavailable"), 502}, {nil, 409}} {
		f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
			return worker.RPCResponse{}, test.err
		}
		f.call("root", "default", "GET", path, nil, test.status)
	}
}
