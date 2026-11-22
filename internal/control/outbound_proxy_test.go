package control

import (
	"context"
	"cove/internal/buildinfo"
	"cove/internal/outbound"
	"cove/internal/worker"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPanelProxyReusesConnectionsAndAppliesChangedSettings(t *testing.T) {
	var connections atomic.Int32
	first := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "first") }))
	first.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	first.Start()
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "second") }))
	defer second.Close()
	s := &Server{outboundState: &outboundProxyState{}}
	config := outbound.Config{HTTP: first.URL}
	s.outboundState.value.Store(&config)
	client := &http.Client{Transport: s.OutboundTransport(http.DefaultTransport.(*http.Transport))}
	defer client.CloseIdleConnections()
	read := func() string {
		response, err := client.Get("http://upstream.example/test")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	if read() != "first" || read() != "first" || connections.Load() != 1 {
		t.Fatal("configured proxy connection was not reused")
	}
	changed := outbound.Config{HTTP: second.URL}
	s.outboundState.value.Store(&changed)
	if read() != "second" {
		t.Fatal("changed settings did not replace the proxy")
	}
}

func TestProxyDoesNotBypassWebhookDestinationValidation(t *testing.T) {
	called := false
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(502) }))
	defer proxy.Close()
	transport := publicWebhookTransport(outbound.Config{HTTPS: proxy.URL})
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	if response, err := client.Get("https://127.0.0.1/private"); err == nil {
		response.Body.Close()
		t.Fatal("private webhook destination accepted")
	}
	if called {
		t.Fatal("private destination reached proxy")
	}
}

func TestFailedFleetScansDoNotStarveOtherResourcesIntegration(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('scan-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('scan-project','Project','default')")
	f.sql(`INSERT INTO workloads(id,node_id,name,configuration,team_id,project_id) SELECT 'scan-'||i,'scan-node','App',$1,'default','scan-project' FROM generate_series(1,17) i`, []byte{})
	f.s.workerRPC = func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error) {
		return worker.RPCResponse{}, errors.New("offline fixture")
	}
	for _, want := range []int{8, 16, 17} {
		if err := f.s.refreshPortExposure(context.Background()); err != nil {
			t.Fatal(err)
		}
		var attempted, observed int
		if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*),count(observed_at) FROM workload_security_observations").Scan(&attempted, &observed); err != nil || attempted != want || observed != 0 {
			t.Fatal(attempted, observed, err)
		}
	}
}

func TestSecurityObservationsAreMeasuredIntegration(t *testing.T) {
	f := newAccountFixture(t)
	w := f.call("root", "", "GET", "/api/settings/security-posture", nil, 200)
	if !strings.Contains(w.Body.String(), `"id":"panel_backup","status":"warn"`) {
		t.Fatal(w.Body.String())
	}
	f.sql("UPDATE control_settings SET panel_backup_at=now(),panel_update_checked_at=now(),latest_panel_version=$1,panel_update_channel=update_channel WHERE id=1", buildinfo.Version)
	w = f.call("root", "", "GET", "/api/settings/security-posture", nil, 200)
	for _, id := range []string{"panel_backup", "updates", "port_exposure"} {
		if !strings.Contains(w.Body.String(), `"id":"`+id+`","status":"pass"`) {
			t.Fatal(w.Body.String())
		}
	}
	f.sql("UPDATE control_settings SET panel_backup_at=now()-interval '3 days',panel_update_checked_at=now()-interval '3 hours' WHERE id=1")
	w = f.call("root", "", "GET", "/api/settings/security-posture", nil, 200)
	for _, id := range []string{"panel_backup", "updates"} {
		if !strings.Contains(w.Body.String(), `"id":"`+id+`","status":"warn"`) {
			t.Fatal(w.Body.String())
		}
	}
}

func TestOutboundProxySealedAndScopedIntegration(t *testing.T) {
	f := newAccountFixture(t)
	member := f.user("proxy-viewer", "viewer@example.com")
	f.call(member, "", "GET", "/api/settings/outbound-proxy", nil, 403)
	secret := "private-proxy-password"
	w := f.call("root", "", "PUT", "/api/settings/outbound-proxy", map[string]string{"http_proxy": "http://proxy.example:3128", "https_proxy": "http://user:" + secret + "@proxy.example:3128", "no_proxy": "localhost,10.0.0.0/8"}, 200)
	if strings.Contains(w.Body.String(), secret) || !strings.Contains(w.Body.String(), `"https_credentials_set":true`) {
		t.Fatal(w.Body.String())
	}
	var encrypted []byte
	if err := f.s.DB.QueryRow(context.Background(), "SELECT outbound_proxy FROM control_settings WHERE id=1").Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encrypted), secret) {
		t.Fatal("proxy credentials are plaintext")
	}
	f.call("root", "", "PUT", "/api/settings/outbound-proxy", map[string]string{"no_proxy": "*.example.com"}, 200)
	c, err := f.s.outboundConfig(context.Background())
	if err != nil || !strings.Contains(c.HTTPS, secret) {
		t.Fatal("partial update lost credentials", err)
	}
	f.call("root", "", "PUT", "/api/settings/outbound-proxy", map[string]string{"https_proxy": "socks5://proxy.example"}, 400)
}

func TestOldWorkerProxyNegotiationIntegration(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('old-proxy-node','Old worker','localhost',22,'',$1,'ready')", []byte{})
	config := outbound.Config{}
	f.s.outboundState.value.Store(&config)
	f.s.outboundState.saved.Store(false)
	request := worker.RPCRequest{Method: "preflight", Request: &worker.Request{}}
	_, err := f.s.nodeRPC(context.Background(), "old-proxy-node", request)
	if err == nil || err.Error() != "node is not enrolled" {
		t.Fatal("default settings must not require new-worker capabilities", err)
	}
	f.s.outboundState.saved.Store(true)
	_, err = f.s.nodeRPC(context.Background(), "old-proxy-node", request)
	if !errors.Is(err, errOutboundWorkerUnsupported) {
		t.Fatal("saved instance settings require an upgraded worker", err)
	}
	_, err = f.s.nodeRPC(context.Background(), "old-proxy-node", worker.RPCRequest{Method: "probe"})
	if err == nil || err.Error() != "node is not enrolled" {
		t.Fatal("capability discovery must remain available", err)
	}
	f.sql(`UPDATE nodes SET capabilities='{"outbound_proxy":true}' WHERE id='old-proxy-node'`)
	_, err = f.s.nodeRPC(context.Background(), "old-proxy-node", request)
	if err == nil || err.Error() != "node is not enrolled" {
		t.Fatal("supported workers must accept instance settings", err)
	}
}
