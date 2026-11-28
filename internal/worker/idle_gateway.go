package worker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

type idleGatewayState struct {
	Mode         string    `json:"mode"`
	Episode      string    `json:"episode,omitempty"`
	Active       int       `json:"active"`
	LastActivity time.Time `json:"last_activity"`
}

type idleGateway struct {
	mu        sync.Mutex
	ctx       context.Context
	peer      netip.Addr
	backend   netip.Prefix
	targets   map[string]*url.URL
	state     idleGatewayState
	changed   chan struct{}
	waking    chan struct{}
	nextWake  time.Time
	wake      func(context.Context, string) error
	persist   func(idleGatewayState) error
	transport http.RoundTripper
}

func newIdleGateway(ctx context.Context, peer netip.Addr, targets map[string]string, backend netip.Prefix, wake func(context.Context, string) error, persist func(idleGatewayState) error) (*idleGateway, error) {
	if !peer.IsValid() || !backend.IsValid() || wake == nil || persist == nil {
		return nil, errors.New("invalid idle gateway identity")
	}
	resolved, err := idleGatewayTargets(targets, backend)
	if err != nil {
		return nil, err
	}
	return &idleGateway{ctx: ctx, peer: peer.Unmap(), backend: backend, targets: resolved, state: idleGatewayState{Mode: "blocked", LastActivity: time.Now().UTC()}, changed: make(chan struct{}), wake: wake, persist: persist, transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ResponseHeaderTimeout: 2 * time.Minute, IdleConnTimeout: 90 * time.Second, MaxIdleConns: 64, MaxIdleConnsPerHost: 16}}, nil
}

func idleGatewayTargets(targets map[string]string, backend netip.Prefix) (map[string]*url.URL, error) {
	if len(targets) > 2048 {
		return nil, errors.New("too many idle gateway targets")
	}
	resolved := map[string]*url.URL{}
	for key, address := range targets {
		endpoint, err := url.Parse(address)
		if err != nil || !identifier.MatchString(key) || endpoint.Scheme != "http" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return nil, errors.New("invalid idle gateway target")
		}
		ip, err := netip.ParseAddr(endpoint.Hostname())
		if err != nil || !backend.Contains(ip) {
			return nil, errors.New("idle gateway target is outside its owned network")
		}
		if address, err := netip.ParseAddrPort(endpoint.Host); err != nil || address.Port() == 0 {
			return nil, errors.New("idle gateway target requires a literal address and port")
		}
		resolved[key] = endpoint
	}
	return resolved, nil
}

func (g *idleGateway) snapshot() idleGatewayState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

func (g *idleGateway) setMode(mode, episode string) error {
	if mode != "awake" && mode != "asleep" && mode != "blocked" {
		return errors.New("invalid idle gateway mode")
	}
	if (mode == "asleep" || episode != "") && !identifier.MatchString(episode) {
		return errors.New("sleep episode required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	next := g.state
	next.Mode, next.Episode = mode, episode
	if mode == "awake" && g.state.Mode != "awake" {
		next.LastActivity = time.Now().UTC()
	}
	if err := g.persist(next); err != nil {
		return err
	}
	g.state = next
	close(g.changed)
	g.changed = make(chan struct{})
	return nil
}

func (g *idleGateway) quiesce(idle time.Duration, episode string) (bool, error) {
	if idle < time.Second || !identifier.MatchString(episode) {
		return false, errors.New("invalid idle interval or sleep episode")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.Mode != "awake" || g.state.Active != 0 || time.Since(g.state.LastActivity) < idle {
		return false, nil
	}
	next := g.state
	next.Mode, next.Episode = "quiescing", episode
	if err := g.persist(next); err != nil {
		return false, err
	}
	g.state = next
	close(g.changed)
	g.changed = make(chan struct{})
	return true, nil
}

func (g *idleGateway) begin(ctx context.Context) error {
	for {
		g.mu.Lock()
		if g.state.Mode == "blocked" {
			g.mu.Unlock()
			return errors.New("resource is stopped")
		}
		if g.state.Mode == "awake" {
			if g.state.Active >= 512 {
				g.mu.Unlock()
				return errors.New("idle gateway request limit reached")
			}
			g.state.Active++
			g.state.LastActivity = time.Now().UTC()
			g.mu.Unlock()
			return nil
		}
		if g.waking == nil {
			if time.Now().Before(g.nextWake) {
				g.mu.Unlock()
				return errors.New("resource wake is temporarily unavailable")
			}
			g.waking = make(chan struct{})
			episode := g.state.Episode
			go g.requestWake(episode)
		}
		changed, waking := g.changed, g.waking
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.ctx.Done():
			return g.ctx.Err()
		case <-changed:
		case <-waking:
		}
	}
}

func (g *idleGateway) requestWake(episode string) {
	ctx, cancel := context.WithTimeout(g.ctx, 3*time.Minute)
	err := g.wake(ctx, episode)
	cancel()
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil || g.state.Mode != "awake" {
		g.nextWake = time.Now().Add(5 * time.Second)
	}
	close(g.waking)
	g.waking = nil
}

func (g *idleGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	g.mu.Lock()
	allowed := err == nil && peer.Addr().Unmap() == g.peer
	g.mu.Unlock()
	if !allowed {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	key, rawPath, ok := strings.Cut(strings.TrimPrefix(r.URL.EscapedPath(), "/cove/"), "/")
	if !ok || !strings.HasPrefix(r.URL.Path, "/cove/") {
		http.NotFound(w, r)
		return
	}
	g.mu.Lock()
	target := g.targets[key]
	g.mu.Unlock()
	if target == nil {
		http.NotFound(w, r)
		return
	}
	path, err := url.PathUnescape("/" + rawPath)
	if err != nil {
		http.Error(w, "Invalid path", 400)
		return
	}
	if err = g.begin(r.Context()); err != nil {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { g.mu.Lock(); g.state.Active--; g.state.LastActivity = time.Now().UTC(); g.mu.Unlock() }()
	g.mu.Lock()
	target = g.targets[key]
	g.mu.Unlock()
	if target == nil {
		http.Error(w, "Upstream unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(g.ctx, cancel)
	defer func() { stop(); cancel() }()
	request := r.Clone(ctx)
	request.URL.Path, request.URL.RawPath = path, "/"+rawPath
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = g.transport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "Upstream unavailable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, request)
}
