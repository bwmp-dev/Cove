package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdleGatewayServerPersistsAndReloadsWakeTargets(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("worker helper uses Linux Unix sockets")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	upstream := func(body string) *httptest.Server {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.2:0")
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/hello" || r.Host != "app.example.test" {
				t.Error("request identity changed", r.URL.Path, r.Host)
			}
			io.WriteString(w, body)
		}))
		server.Listener = listener
		server.Start()
		t.Cleanup(server.Close)
		return server
	}
	one, two := upstream("one"), upstream("two")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	dir := t.TempDir()
	configPath, statePath, controlPath, wakePath := filepath.Join(dir, "config.json"), filepath.Join(dir, "state.json"), filepath.Join(dir, "control.sock"), filepath.Join(dir, "wake.sock")
	config := idleGatewayConfig{Listen: address, Peer: "127.0.0.1", FrontSubnet: "127.0.0.1/32", BackendSubnet: "127.0.0.2/32", Workload: "app", Token: strings.Repeat("a", 64), Mode: "blocked", Targets: map[string]string{"app": one.URL}}
	config.Revision = config.digest()
	if err = writeJSON(configPath, config); err != nil {
		t.Fatal(err)
	}
	controlTransport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", controlPath)
	}}
	defer controlTransport.CloseIdleConnections()
	controlClient := &http.Client{Transport: controlTransport, Timeout: 3 * time.Second}
	control := func(method, path string, body []byte) (int, []byte, error) {
		if method == "POST" && path == "/reload" && body == nil {
			current, err := loadIdleGatewayConfig(configPath)
			if err != nil {
				return 0, nil, err
			}
			body, _ = json.Marshal(map[string]string{"revision": current.Revision})
		}
		r, err := http.NewRequestWithContext(ctx, method, "http://gateway"+path, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		response, err := controlClient.Do(r)
		if err != nil {
			return 0, nil, err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		return response.StatusCode, data, err
	}
	var cfgMu sync.Mutex
	var wakes atomic.Int32
	wakeFinished := make(chan struct{})
	wakeListener, err := net.Listen("unix", wakePath)
	if err != nil {
		t.Fatal(err)
	}
	wakeServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(wakeFinished)
		if r.Method != "POST" || r.URL.Path != "/wake" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 64) {
			t.Error("wake identity changed")
			w.WriteHeader(403)
			return
		}
		var body struct {
			Workload string
			Episode  string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Workload != "app" || body.Episode != "sleep-one" {
			t.Error("invalid wake input", body, err)
			w.WriteHeader(400)
			return
		}
		wakes.Add(1)
		cfgMu.Lock()
		config.Mode = "awake"
		config.Episode = ""
		config.Targets = map[string]string{"app": two.URL}
		config.Revision = config.digest()
		err := writeJSON(configPath, config)
		cfgMu.Unlock()
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		code, _, err := control("POST", "/reload", nil)
		if err != nil || code != 204 {
			t.Error("wake target reload failed", code, err)
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	})}
	go wakeServer.Serve(wakeListener)
	defer wakeServer.Close()
	finished := make(chan error, 1)
	go func() { finished <- serveIdleGateway(ctx, configPath, statePath, controlPath, wakePath) }()
	for deadline := time.Now().Add(5 * time.Second); ; {
		code, _, err := control("GET", "/status", nil)
		if err == nil && code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control socket did not start", code, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	request := func() (int, string) {
		t.Helper()
		r, _ := http.NewRequestWithContext(ctx, "GET", "http://"+address+"/cove/app/hello", nil)
		r.Host = "app.example.test"
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, string(data)
	}
	if code, _ := request(); code != 503 {
		t.Fatal("blocked helper forwarded traffic", code)
	}
	cfgMu.Lock()
	config.Mode = "awake"
	config.Revision = config.digest()
	err = writeJSON(configPath, config)
	cfgMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if code, _, err := control("POST", "/reload", nil); err != nil || code != 204 {
		t.Fatal(code, err)
	}
	if code, body := request(); code != 200 || body != "one" {
		t.Fatal(code, body)
	}

	readActivity := func() time.Time {
		t.Helper()
		code, data, err := control("GET", "/status", nil)
		var state idleGatewayState
		if err != nil || code != 200 || json.Unmarshal(data, &state) != nil {
			t.Fatal(code, err)
		}
		return state.LastActivity
	}
	beforeActivity := readActivity()
	if code, _, err := control("POST", "/reload", nil); err != nil || code != 204 {
		t.Fatal(code, err)
	}
	if after := readActivity(); !after.Equal(beforeActivity) {
		t.Fatal("unrelated reload reset activity", beforeActivity, after)
	}
	cfgMu.Lock()
	config.ActivityEpoch = "resume-one"
	config.Revision = config.digest()
	err = writeJSON(configPath, config)
	cfgMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if code, _, err := control("POST", "/reload", nil); err != nil || code != 204 {
		t.Fatal(code, err)
	}
	if after := readActivity(); !after.After(beforeActivity) {
		t.Fatal("resumed resource kept its old idle timer", beforeActivity, after)
	}
	if code, _, err := control("POST", "/quiesce", []byte(`{"seconds":86400,"episode":"sleep-one"}`)); err != nil || code != 409 {
		t.Fatal("recent requests allowed sleep", code, err)
	}
	cfgMu.Lock()
	config.Mode = "asleep"
	config.Episode = "sleep-one"
	config.Revision = config.digest()
	err = writeJSON(configPath, config)
	cfgMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if code, _, err := control("POST", "/reload", nil); err != nil || code != 204 {
		t.Fatal(code, err)
	}
	if code, body := request(); code != 200 || body != "two" || wakes.Load() != 1 {
		t.Fatal("first waking request used the old endpoint", code, body, wakes.Load())
	}
	// A file reload can release the request before the fake worker finishes its acknowledgement.
	select {
	case <-wakeFinished:
	case <-ctx.Done():
		t.Fatal("wake handler did not finish", ctx.Err())
	}
	data, err := os.ReadFile(statePath)
	var saved idleGatewaySaved
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.State.Mode != "awake" {
		t.Fatal("wake mode was not persisted", saved, err)
	}
	info, err := os.Stat(controlPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("control socket permissions", info, err)
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not stop")
	}
}

func TestIdleWakeSocketRejectsInvalidCredentialsAndLongRootWorks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("worker wake endpoint uses Linux Unix sockets")
	}
	root := filepath.Join(t.TempDir(), strings.Repeat("x", 80))
	e, err := NewEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "workloads", "app"), 0700); err != nil {
		t.Fatal(err)
	}
	config := idleGatewayConfig{Workload: "app", Listen: "10.250.0.2:8080", Peer: "10.250.0.3", FrontSubnet: "10.250.0.0/29", BackendSubnet: "10.250.1.0/24", Token: strings.Repeat("a", 64), Mode: "asleep", Episode: "idle-current", Targets: map[string]string{}}
	config.Revision = config.digest()
	if err = e.writeIdle("app", &idleWorkloadState{Policy: IdlePolicy{Enabled: true, AfterSeconds: 60, RequestDriven: true}, Config: config}); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(root, "workloads", "app", "suspension.json"), workloadSuspension{Operation: "idle-current", Options: SuspensionOptions{Reason: "idle", Scope: "app"}, Phase: "paused", Created: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	closeServer, err := e.startIdleWake(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer()
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		address, closeAddress, err := idleSocketAddress(filepath.Join(root, "idle-wake", "worker.sock"))
		if err != nil {
			return nil, err
		}
		defer closeAddress()
		return (&net.Dialer{}).DialContext(ctx, "unix", address)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	for _, test := range []struct {
		credential, body string
		code             int
	}{
		{"", `{"workload":"app","episode":"idle-current"}`, 403},
		{"Bearer " + strings.Repeat("b", 64), `{"workload":"app","episode":"idle-current"}`, 403},
		{"Bearer " + config.Token, `{"workload":"app","episode":"idle-old"}`, 503},
		{"Bearer " + config.Token, `{"workload":"missing","episode":"idle-current"}`, 403},
		{"Bearer " + config.Token, `{"workload":"../app","episode":"idle-current"}`, 400},
	} {
		request, err := http.NewRequestWithContext(ctx, "POST", "http://worker/wake", strings.NewReader(test.body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", test.credential)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.code {
			t.Fatal(response.StatusCode, test.code)
		}
	}
	if len(e.pending) != 0 {
		t.Fatal("rejected wake queued work")
	}
	info, err := os.Stat(filepath.Join(root, "idle-wake", "worker.sock"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("wake endpoint permissions", info, err)
	}
}
