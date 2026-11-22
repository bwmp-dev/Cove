package worker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdleGatewayCountsStreamsAndCoalescesWake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/a%2Fb" || r.URL.RawQuery != "q=one%20two" || r.Host != "app.example.test" {
			t.Errorf("proxy changed request identity: %s %s %s", r.URL.EscapedPath(), r.URL.RawQuery, r.Host)
		}
		if requests.Add(1) == 1 {
			entered <- struct{}{}
			<-release
		}
		fmt.Fprint(w, "ready")
	}))
	defer upstream.Close()
	wakeStarted, wakeRelease := make(chan struct{}, 1), make(chan struct{})
	var wakes atomic.Int32
	var gateway *idleGateway
	wake := func(ctx context.Context, episode string) error {
		if episode != "sleep-one" {
			t.Error("wrong sleep episode", episode)
		}
		wakes.Add(1)
		wakeStarted <- struct{}{}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wakeRelease:
		}
		return gateway.setMode("awake", "")
	}
	var err error
	gateway, err = newIdleGateway(ctx, netip.MustParseAddr("127.0.0.1"), map[string]string{"app": upstream.URL}, netip.MustParsePrefix("127.0.0.0/8"), wake, func(idleGatewayState) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = gateway.setMode("awake", ""); err != nil {
		t.Fatal(err)
	}
	request := func() int {
		r := httptest.NewRequest("GET", "http://app.example.test/cove/app/a%2Fb?q=one%20two", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		recorder := httptest.NewRecorder()
		gateway.ServeHTTP(recorder, r)
		return recorder.Code
	}
	first := make(chan int, 1)
	go func() { first <- request() }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream was not reached")
	}
	if gateway.snapshot().Active != 1 {
		t.Fatal("stream not counted")
	}
	if ready, err := gateway.quiesce(time.Second, "sleep-one"); err != nil || ready {
		t.Fatal("inflight response allowed sleep", ready, err)
	}
	close(release)
	if code := <-first; code != 200 {
		t.Fatal(code)
	}
	if gateway.snapshot().Active != 0 {
		t.Fatal("completed stream was retained")
	}
	gateway.mu.Lock()
	gateway.state.LastActivity = time.Now().Add(-2 * time.Second)
	gateway.mu.Unlock()
	if ready, err := gateway.quiesce(time.Second, "sleep-one"); err != nil || !ready {
		t.Fatal(ready, err)
	}
	var pending sync.WaitGroup
	for i := 0; i < 12; i++ {
		pending.Go(func() {
			if code := request(); code != 200 {
				t.Error(code)
			}
		})
	}
	select {
	case <-wakeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not ask to wake")
	}
	if requests.Load() != 1 || gateway.snapshot().Active != 0 {
		t.Fatal("quiescing forwarded a request before wake", requests.Load(), gateway.snapshot())
	}
	if err = gateway.setMode("asleep", "sleep-one"); err != nil {
		t.Fatal(err)
	}
	close(wakeRelease)
	pending.Wait()
	if wakes.Load() != 1 || requests.Load() != 13 || gateway.snapshot().Active != 0 {
		t.Fatal("wake was not shared", wakes.Load(), requests.Load(), gateway.snapshot())
	}
}

func TestIdleGatewayTracksWebSocketUntilClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if _, err = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"); err != nil {
			t.Error(err)
			return
		}
		io.Copy(io.Discard, conn)
	}))
	defer upstream.Close()
	gateway, err := newIdleGateway(ctx, netip.MustParseAddr("127.0.0.1"), map[string]string{"app": upstream.URL}, netip.MustParsePrefix("127.0.0.0/8"), func(context.Context, string) error { return errors.New("unexpected wake") }, func(idleGatewayState) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = gateway.setMode("awake", ""); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(gateway)
	defer server.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "GET /cove/app/socket HTTP/1.1\r\nHost: app.example.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 101 || gateway.snapshot().Active != 1 {
		t.Fatal(response.StatusCode, gateway.snapshot())
	}
	if ready, err := gateway.quiesce(time.Second, "sleep"); err != nil || ready {
		t.Fatal("websocket allowed sleep", ready, err)
	}
	cancel()
	for deadline := time.Now().Add(5 * time.Second); gateway.snapshot().Active != 0; {
		if time.Now().After(deadline) {
			t.Fatal("closed websocket remained active")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestIdleGatewayRejectsUnownedTargetsAndFailsClosed(t *testing.T) {
	peer, subnet := netip.MustParseAddr("127.0.0.1"), netip.MustParsePrefix("127.0.0.0/8")
	wake := func(context.Context, string) error {
		t.Fatal("a stopped or unauthenticated request asked to wake")
		return nil
	}
	var fail bool
	persist := func(idleGatewayState) error {
		if fail {
			return errors.New("disk unavailable")
		}
		return nil
	}
	for _, target := range []string{"http://metadata.example.test:80", "http://169.254.169.254:80", "http://user:pass@127.0.0.1:80", "https://127.0.0.1:80", "http://127.0.0.1:80/path", "http://127.0.0.1:80?query", "http://127.0.0.1:0"} {
		if _, err := newIdleGateway(context.Background(), peer, map[string]string{"app": target}, subnet, wake, persist); err == nil {
			t.Fatal("unowned target accepted", target)
		}
	}
	gateway, err := newIdleGateway(context.Background(), peer, map[string]string{"app": "http://127.0.0.1:80"}, subnet, wake, persist)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://app.test/cove/app/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	gateway.ServeHTTP(recorder, r)
	if recorder.Code != 503 {
		t.Fatal("unknown startup state forwarded traffic", recorder.Code)
	}
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	recorder = httptest.NewRecorder()
	gateway.ServeHTTP(recorder, r)
	if recorder.Code != 403 {
		t.Fatal("forwarded headers bypassed peer check", recorder.Code)
	}
	if err = gateway.setMode("awake", ""); err != nil {
		t.Fatal(err)
	}
	gateway.mu.Lock()
	gateway.state.LastActivity = time.Now().Add(-2 * time.Second)
	gateway.mu.Unlock()
	fail = true
	if ready, err := gateway.quiesce(time.Second, "sleep"); err == nil || ready || gateway.snapshot().Mode != "awake" {
		t.Fatal("failed persistence committed sleep", ready, err, gateway.snapshot())
	}
}

func TestIdleGatewayAwakeRefreshPreservesTimer(t *testing.T) {
	gateway, err := newIdleGateway(context.Background(), netip.MustParseAddr("127.0.0.1"), map[string]string{}, netip.MustParsePrefix("127.0.0.0/8"), func(context.Context, string) error { return nil }, func(idleGatewayState) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = gateway.setMode("awake", ""); err != nil {
		t.Fatal(err)
	}
	before := gateway.snapshot().LastActivity
	time.Sleep(time.Millisecond)
	if err = gateway.setMode("awake", ""); err != nil {
		t.Fatal(err)
	}
	if !gateway.snapshot().LastActivity.Equal(before) {
		t.Fatal("unrelated route refresh reset the idle timer")
	}
	if err = gateway.setMode("blocked", ""); err != nil {
		t.Fatal(err)
	}
	if err = gateway.setMode("awake", ""); err != nil {
		t.Fatal(err)
	}
	if !gateway.snapshot().LastActivity.After(before) {
		t.Fatal("waking did not reset the idle timer")
	}
}
