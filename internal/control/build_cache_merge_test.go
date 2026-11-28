package control

import (
	"context"
	"cove/internal/outbound"
	"cove/internal/worker"
	"encoding/json"
	"testing"
)

func TestBuildCacheMergeScopesAndProxyAdmission(t *testing.T) {
	f, _, cookie := publicFixture(t)
	f.sql(`UPDATE users SET is_admin=true WHERE id='public-owner'`)
	f.sql(`UPDATE nodes SET capabilities='{"build_cache_limits":true}' WHERE id='variable-node'`)
	_, read := mintPublic(t, f, cookie, "user", "workloads:read")
	_, manage := mintPublic(t, f, cookie, "user", "nodes:manage")
	path := "/api/v1/nodes/variable-node/build-cache"
	calls := 0
	f.s.workerRPC = func(_ context.Context, _ string, r worker.RPCRequest) (worker.RPCResponse, error) {
		calls++
		if r.Method == "build_cache_configure" && (r.BuildCache == nil || r.BuildCache.Path != "") {
			t.Fatal("proxy-blocked verification reached worker", r.Method)
		}
		status := &worker.BuildCacheStatus{Supported: true, CanConfigure: true, Configured: true, Verified: true, CapacityBytes: 64 * 1024 * 1024}
		if r.Method == "build_cache_configure" {
			status.Configured, status.Verified, status.CapacityBytes = false, false, 0
		}
		return worker.RPCResponse{BuildCache: status}, nil
	}
	publicCall(t, f, read, "default", "GET", path, nil, 403)
	publicCall(t, f, manage, "default", "GET", path, nil, 200)
	f.s.outboundState.value.Store(&outbound.Config{HTTPS: "http://proxy.example.test:8080"})
	f.s.outboundState.saved.Store(true)
	before := calls
	publicCall(t, f, manage, "default", "PUT", path, worker.BuildCacheSettings{Path: "/srv/cache"}, 409)
	if calls != before {
		t.Fatal("unsupported proxy policy was forwarded to the worker")
	}
	response := publicCall(t, f, manage, "default", "GET", path, nil, 200)
	var status worker.BuildCacheStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || status.CanConfigure || status.Verified || status.Reason == "" {
		t.Fatal("old worker verification hid the current proxy restriction", status, err)
	}
	response = publicCall(t, f, manage, "default", "PUT", path, worker.BuildCacheSettings{}, 200)
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || status.Configured {
		t.Fatal("explicit disable did not remain available under proxy policy", status, err)
	}
	f.sql(`UPDATE users SET is_admin=false WHERE id='public-owner'`)
	publicCall(t, f, manage, "default", "GET", path, nil, 403)
}
