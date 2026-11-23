package control

import (
	"context"
	"cove/internal/worker"
	"strings"
	"testing"
)

func TestIngressExposureAPIIntegration(t *testing.T) {
	f := newAccountFixture(t)
	admin := f.user("listener-admin", "listener-admin@example.test")
	f.sql("UPDATE users SET is_admin=true WHERE id='listener-admin'")
	viewer := f.user("listener-viewer", "listener-viewer@example.test")
	var forwarded []worker.RPCRequest
	f.s.workerRPC = func(_ context.Context, node string, request worker.RPCRequest) (worker.RPCResponse, error) {
		if node != "local" {
			t.Fatal("wrong node", node)
		}
		forwarded = append(forwarded, request)
		if request.Method == "ingress_exposures_plan" {
			return worker.RPCResponse{ExposurePlan: &worker.IngressExposurePlan{Revision: strings.Repeat("a", 64), Digest: strings.Repeat("b", 64), Settings: request.Exposures.Settings}}, nil
		}
		return worker.RPCResponse{Exposures: &worker.IngressExposureStatus{ProxyPresent: true, Settings: worker.IngressExposureSettings{}}}, nil
	}
	settings := map[string]any{"http3": false, "listeners": []any{}}
	change := map[string]any{"settings": settings, "revision": strings.Repeat("a", 64), "digest": strings.Repeat("b", 64), "accept_interruption": true}
	for _, action := range []struct {
		method, path string
		body         any
	}{{"GET", "/api/nodes/local/ingress-listeners", nil}, {"POST", "/api/nodes/local/ingress-listeners/initialize", nil}, {"POST", "/api/nodes/local/ingress-listeners/plan", settings}, {"PUT", "/api/nodes/local/ingress-listeners", change}} {
		f.call(viewer, "", action.method, action.path, action.body, 403)
		f.call(admin, "", action.method, action.path, action.body, 200)
	}
	if len(forwarded) != 4 || forwarded[1].Method != "ingress_exposures_initialize" || forwarded[3].Exposures == nil || !forwarded[3].Exposures.AcceptInterruption {
		t.Fatal("listener requests were not forwarded exactly", forwarded)
	}
	change["accept_interruption"] = false
	f.call(admin, "", "PUT", "/api/nodes/local/ingress-listeners", change, 400)
	f.call(admin, "", "POST", "/api/nodes/local/ingress-listeners/plan", map[string]any{"unexpected": true}, 400)
	if len(forwarded) != 4 {
		t.Fatal("invalid input reached worker")
	}
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{}, &RemoteError{Message: "unsupported method"}
	}
	f.call(admin, "", "GET", "/api/nodes/local/ingress-listeners", nil, 409)
	var audited int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='PUT /api/nodes/{id}/ingress-listeners'").Scan(&audited); err != nil || audited == 0 {
		t.Fatal("listener change was not audited", err)
	}
}
