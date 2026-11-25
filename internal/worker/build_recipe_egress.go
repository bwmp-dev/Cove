package worker

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type recipeEgressConfig struct {
	Build      bool   `json:"build,omitempty"`
	Listen     string `json:"listen"`
	HTTPProxy  string `json:"http_proxy,omitempty"`
	HTTPSProxy string `json:"https_proxy,omitempty"`
	NoProxy    string `json:"no_proxy,omitempty"`
	Roots      string `json:"roots"`
}

var recipeDeniedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/3"), netip.MustParsePrefix("::/96"), netip.MustParsePrefix("::ffff:0:0/96"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"),
}

func publicRecipeAddress(ip netip.Addr) bool {
	if !ip.IsGlobalUnicast() || ip.Is4In6() || ip.String() == "168.63.129.16" {
		return false
	}
	for _, prefix := range recipeDeniedNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func recipeNoProxy(host, port, policy string) bool {
	ip, _ := netip.ParseAddr(host)
	for _, item := range strings.Split(policy, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "*" {
			return true
		}
		if prefix, err := netip.ParsePrefix(item); err == nil {
			if prefix.Contains(ip) {
				return true
			}
			continue
		}
		if h, p, err := net.SplitHostPort(item); err == nil {
			if p != port {
				continue
			}
			item = h
		}
		if item == "" {
			continue
		}
		if strings.HasPrefix(item, "*.") {
			item = item[1:]
		}
		if strings.HasPrefix(item, ".") {
			if strings.HasSuffix(strings.ToLower(host), item) {
				return true
			}
		} else if strings.EqualFold(host, item) || strings.HasSuffix(strings.ToLower(host), "."+item) {
			return true
		}
	}
	return false
}

type recipeEgress struct {
	config      recipeEgressConfig
	transport   *http.Transport
	lookup      func(context.Context, string) ([]netip.Addr, error)
	dial        func(context.Context, string, string) (net.Conn, error)
	semaphore   chan struct{}
	mu          sync.Mutex
	connections map[net.Conn]bool
	remaining   int64
}

func newRecipeEgress(config recipeEgressConfig) (*recipeEgress, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(config.Roots)) {
		return nil, errors.New("planner egress CA roots are unavailable")
	}
	for _, value := range []string{config.HTTPProxy, config.HTTPSProxy} {
		if value == "" {
			continue
		}
		proxy, err := url.Parse(value)
		if err != nil || proxy.Hostname() == "" || proxy.Scheme != "http" && proxy.Scheme != "https" || proxy.RawQuery != "" || proxy.Fragment != "" || proxy.Path != "" && proxy.Path != "/" {
			return nil, errors.New("confined builds require an HTTP or HTTPS outbound proxy")
		}
		if ip, err := netip.ParseAddr(proxy.Hostname()); err == nil && (ip.IsLoopback() || ip.IsLinkLocalUnicast() || !ip.IsGlobalUnicast() || ip.String() == "168.63.129.16" || netip.MustParsePrefix("100.64.0.0/10").Contains(ip)) {
			return nil, errors.New("planner outbound proxy cannot be a host-local or metadata endpoint")
		}
		if proxy.Port() == "2375" || proxy.Port() == "2376" {
			return nil, errors.New("planner outbound proxy cannot be a Docker endpoint")
		}
	}
	p := &recipeEgress{config: config, semaphore: make(chan struct{}, 8), connections: map[net.Conn]bool{}, remaining: 64 << 20}
	if config.Build {
		p.remaining = 3 << 30
	}
	p.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		if literal, err := netip.ParseAddr(host); err == nil {
			return []netip.Addr{literal}, nil
		}
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		// Resolver IPv4 answers can be represented as mapped IPv6 addresses.
		// Normalize those answers while keeping explicit mapped literals refused.
		for index := range addresses {
			addresses[index] = addresses[index].Unmap()
		}
		return addresses, err
	}
	p.dial = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}).DialContext
	p.transport = &http.Transport{Proxy: p.proxy, DialContext: p.safeDial, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, IdleConnTimeout: 20 * time.Second, MaxResponseHeaderBytes: 32 << 10, MaxIdleConns: 8, DisableCompression: true}
	return p, nil
}

func (p *recipeEgress) proxy(request *http.Request) (*url.URL, error) {
	port := request.URL.Port()
	if port == "" {
		if request.URL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if recipeNoProxy(request.URL.Hostname(), port, p.config.NoProxy) {
		return nil, nil
	}
	value := p.config.HTTPProxy
	if request.URL.Scheme == "https" {
		value = p.config.HTTPSProxy
	}
	if value == "" {
		return nil, nil
	}
	return url.Parse(value)
}

func (p *recipeEgress) safeDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid planner egress address")
	}
	proxyAddress := false
	for _, raw := range []string{p.config.HTTPProxy, p.config.HTTPSProxy} {
		if raw == "" {
			continue
		}
		proxy, _ := url.Parse(raw)
		proxyPort := proxy.Port()
		if proxyPort == "" {
			if proxy.Scheme == "https" {
				proxyPort = "443"
			} else {
				proxyPort = "80"
			}
		}
		if host == proxy.Hostname() && port == proxyPort {
			proxyAddress = true
		}
	}
	ips, err := p.lookup(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("planner metadata DNS lookup failed")
	}
	for _, ip := range ips {
		if !publicRecipeAddress(ip) && (!proxyAddress || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.String() == "168.63.129.16" || netip.MustParsePrefix("100.64.0.0/10").Contains(ip)) {
			return nil, errors.New("planner egress denied a private, host-local or metadata address")
		}
	}
	return p.dial(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

func (p *recipeEgress) checkTarget(ctx context.Context, target *url.URL) error {
	if target.User != nil || target.Hostname() == "" || target.Scheme != "http" && target.Scheme != "https" {
		return errors.New("invalid planner metadata target")
	}
	port := target.Port()
	if port != "" && port != "80" && port != "443" {
		return errors.New("planner metadata ports are restricted to HTTP and HTTPS")
	}
	ips, err := p.lookup(ctx, target.Hostname())
	if err != nil || len(ips) == 0 {
		return errors.New("planner metadata DNS lookup failed")
	}
	for _, ip := range ips {
		if !publicRecipeAddress(ip) {
			return errors.New("planner egress denied a private, host-local or metadata target")
		}
	}
	return nil
}

func (p *recipeEgress) copy(destination io.Writer, source io.Reader) error {
	buffer := make([]byte, 32<<10)
	var total int64
	limit := int64(8 << 20)
	if p.config.Build {
		limit = 2 << 30
	}
	for {
		n, err := source.Read(buffer)
		if n > 0 {
			p.mu.Lock()
			permitted := p.remaining >= int64(n) && total+int64(n) <= limit
			if permitted {
				p.remaining -= int64(n)
			}
			p.mu.Unlock()
			if !permitted {
				return errors.New("confined build egress transfer limit exceeded")
			}
			total += int64(n)
			if _, err := destination.Write(buffer[:n]); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (p *recipeEgress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case p.semaphore <- struct{}{}:
		defer func() { <-p.semaphore }()
	default:
		http.Error(w, "planner metadata concurrency limit", 503)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	target := r.URL
	if r.Method == "CONNECT" {
		target = &url.URL{Scheme: "https", Host: r.Host}
	}
	if err := p.checkTarget(ctx, target); err != nil {
		http.Error(w, "planner metadata target refused", 403)
		return
	}
	if r.Method == "CONNECT" {
		p.tunnel(ctx, w, r, target)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "planner metadata supports GET, HEAD and CONNECT", 405)
		return
	}
	request := r.Clone(ctx)
	request.RequestURI = ""
	request.Header.Del("Proxy-Authorization")
	request.Header.Del("Proxy-Connection")
	request.Header.Del("Connection")
	response, err := p.transport.RoundTrip(request)
	if err != nil {
		http.Error(w, "planner metadata request failed", 502)
		return
	}
	defer response.Body.Close()
	for name, values := range response.Header {
		if strings.EqualFold(name, "Connection") || strings.EqualFold(name, "Proxy-Authenticate") {
			continue
		}
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_ = p.copy(w, response.Body)
}

func (p *recipeEgress) tunnel(ctx context.Context, w http.ResponseWriter, r *http.Request, target *url.URL) {
	// Transport owns proxy authentication and verifies HTTPS proxies; CONNECT reaches only the already checked target.
	proxy, err := p.proxy(&http.Request{URL: target})
	if err != nil {
		http.Error(w, "planner proxy unavailable", 502)
		return
	}
	address := target.Host
	if target.Port() == "" {
		address = net.JoinHostPort(target.Hostname(), "443")
	}
	var remote net.Conn
	if proxy == nil {
		remote, err = p.safeDial(ctx, "tcp", address)
	} else {
		remote, err = p.connectProxy(ctx, proxy, address)
	}
	if err != nil {
		http.Error(w, "planner metadata connection failed", 502)
		return
	}
	defer remote.Close()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "planner tunnel unavailable", 500)
		return
	}
	local, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer local.Close()
	p.mu.Lock()
	p.connections[local] = true
	p.connections[remote] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.connections, local); delete(p.connections, remote); p.mu.Unlock() }()
	deadline := time.Now().Add(90 * time.Second)
	if p.config.Build {
		deadline = time.Now().Add(20 * time.Minute)
	}
	_ = local.SetDeadline(deadline)
	_ = remote.SetDeadline(deadline)
	if _, err = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if buffered.Flush() != nil {
		return
	}
	done := make(chan struct{})
	go func() { _ = p.copy(remote, buffered); _ = remote.Close(); close(done) }()
	_ = p.copy(local, remote)
	_ = local.Close()
	<-done
}

func (p *recipeEgress) connectProxy(ctx context.Context, proxy *url.URL, address string) (net.Conn, error) {
	port := proxy.Port()
	if port == "" {
		if proxy.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	connection, err := p.safeDial(ctx, "tcp", net.JoinHostPort(proxy.Hostname(), port))
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = connection.Close()
		}
	}()
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	if proxy.Scheme == "https" {
		tlsConnection := tls.Client(connection, &tls.Config{RootCAs: p.transport.TLSClientConfig.RootCAs, ServerName: proxy.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConnection.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		connection = tlsConnection
	}
	request := &http.Request{Method: "CONNECT", URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(proxy.User.Username()+":"+password)))
	}
	if err := request.Write(connection); err != nil {
		return nil, err
	}
	headerLimit := &io.LimitedReader{R: connection, N: 32<<10 + 1}
	buffer := bufio.NewReaderSize(headerLimit, 32<<10)
	response, err := http.ReadResponse(buffer, request)
	if err != nil || headerLimit.N == 0 || response.StatusCode != 200 {
		return nil, errors.New("planner outbound proxy refused CONNECT")
	}
	if buffer.Buffered() != 0 {
		return nil, errors.New("planner outbound proxy sent unexpected tunnel bytes")
	}
	ok = true
	return connection, nil
}

func RunBuildRecipeEgress(ctx context.Context) error {
	file, err := os.Open("/config/egress.json")
	if err != nil {
		return err
	}
	defer file.Close()
	var config recipeEgressConfig
	if err := json.NewDecoder(io.LimitReader(file, 2<<20)).Decode(&config); err != nil {
		return errors.New("invalid planner egress configuration")
	}
	p, err := newRecipeEgress(config)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 90 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 32 << 10}
	if config.Build {
		server.ReadTimeout, server.WriteTimeout = 20*time.Minute, 20*time.Minute
	}
	stop := context.AfterFunc(ctx, func() {
		_ = server.Close()
		p.transport.CloseIdleConnections()
		p.mu.Lock()
		defer p.mu.Unlock()
		for c := range p.connections {
			_ = c.Close()
		}
	})
	defer stop()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
