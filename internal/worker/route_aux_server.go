package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"cove/internal/netpolicy"
)

type routeAuxEntry struct {
	Maintenance *RouteMaintenance `json:"maintenance,omitempty"`
	ErrorPages  map[string]string `json:"error_pages,omitempty"`
	Auth        *RouteForwardAuth `json:"auth,omitempty"`
	Targets     []routeAuxTarget  `json:"targets,omitempty"`
}

type routeAuxTarget struct {
	URL    string `json:"url"`
	Weight int    `json:"weight"`
}

type routeAuxConfig struct {
	Peer    string                   `json:"peer"`
	CAPEM   string                   `json:"ca_pem,omitempty"`
	Entries map[string]routeAuxEntry `json:"entries"`
}

func publicAuthTransport(roots *x509.CertPool) *http.Transport {
	return &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(addresses) == 0 {
			return nil, errors.New("authentication hostname has no addresses")
		}
		for _, address := range addresses {
			if !netpolicy.PublicIP(address.IP) {
				return nil, errors.New("authentication hostname resolves to a non-public address")
			}
		}
		var failure error
		for _, address := range addresses {
			connection, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(address.IP.String(), port))
			if err == nil {
				return connection, nil
			}
			failure = err
		}
		return nil, failure
	}, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 65536, MaxIdleConns: 32, MaxIdleConnsPerHost: 8, IdleConnTimeout: 30 * time.Second}
}

func newRouteAuxHandler(config routeAuxConfig, public http.RoundTripper) (http.Handler, error) {
	peer, err := netip.ParseAddr(config.Peer)
	if err != nil || peer.IsUnspecified() {
		return nil, errors.New("invalid route helper proxy peer")
	}
	if len(config.Entries) > 2048 {
		return nil, errors.New("too many route helper entries")
	}
	for key, entry := range config.Entries {
		if len(key) != 32 {
			return nil, errors.New("invalid route helper entry identity")
		}
		if err := validateRouteExtensions(&RoutePolicy{Maintenance: entry.Maintenance, ErrorPages: entry.ErrorPages, ForwardAuth: entry.Auth}); err != nil {
			return nil, err
		}
		if len(entry.Targets) > 64 {
			return nil, errors.New("too many authentication replicas")
		}
		for _, target := range entry.Targets {
			endpoint, err := url.Parse(target.URL)
			if err != nil || endpoint.Scheme != "http" || endpoint.User != nil || net.ParseIP(endpoint.Hostname()) == nil || endpoint.Port() == "" || endpoint.Fragment != "" || target.Weight < 1 || target.Weight > 10000 {
				return nil, errors.New("route helper target is not an owned literal HTTP endpoint")
			}
		}
	}
	if public == nil {
		roots := x509.NewCertPool()
		if config.CAPEM != "" && !roots.AppendCertsFromPEM([]byte(config.CAPEM)) {
			return nil, errors.New("route helper CA bundle is invalid")
		}
		public = publicAuthTransport(roots)
	}
	publicClient := &http.Client{Transport: public, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ownedTransport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 65536, MaxIdleConns: 32, MaxIdleConnsPerHost: 8, IdleConnTimeout: 30 * time.Second}
	ownedClient := &http.Client{Transport: ownedTransport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	active := make(chan struct{}, 128)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		address, parseErr := netip.ParseAddr(host)
		if r.URL.Path == "/health" && parseErr == nil && address.IsLoopback() {
			w.WriteHeader(204)
			return
		}
		if err != nil || parseErr != nil || address.Unmap() != peer.Unmap() {
			http.Error(w, "Forbidden", 403)
			return
		}
		select {
		case active <- struct{}{}:
			defer func() { <-active }()
		default:
			http.Error(w, "Route helper busy", 503)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 3 || parts[0] != "routes" {
			http.NotFound(w, r)
			return
		}
		entry, ok := config.Entries[parts[1]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch parts[2] {
		case "maintenance":
			if entry.Maintenance == nil || !entry.Maintenance.Enabled {
				http.NotFound(w, r)
				return
			}
			body := entry.Maintenance.HTML
			if body == "" {
				body = "<!doctype html><html lang=\"en\"><meta charset=\"utf-8\"><title>Maintenance</title><h1>Temporarily unavailable</h1><p>This application is undergoing maintenance. Please try again later.</p></html>"
			}
			w.Header().Set("Retry-After", "60")
			writeRoutePage(w, 503, body)
		case "error":
			if len(parts) != 4 {
				http.NotFound(w, r)
				return
			}
			body, ok := entry.ErrorPages[parts[3]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			status, _ := strconv.Atoi(parts[3])
			writeRoutePage(w, status, body)
		case "auth":
			if entry.Auth == nil {
				http.NotFound(w, r)
				return
			}
			if r.Header.Get("X-Cove-Auth-Hop") != "" {
				http.Error(w, "Authentication endpoint cannot depend on another forward-auth check", 503)
				return
			}
			endpoint, client := entry.Auth.URL, publicClient
			if endpoint == "" {
				if len(entry.Targets) == 0 {
					http.Error(w, "Authentication service unavailable", 503)
					return
				}
				total := 0
				for _, target := range entry.Targets {
					total += target.Weight
				}
				selected := rand.IntN(total)
				for _, target := range entry.Targets {
					selected -= target.Weight
					if selected < 0 {
						endpoint = target.URL
						break
					}
				}
				client = ownedClient
			}
			request, err := http.NewRequestWithContext(r.Context(), "GET", endpoint, nil)
			if err != nil {
				http.Error(w, "Authentication service unavailable", 503)
				return
			}
			for _, name := range []string{"Cookie", "Authorization", "Accept", "User-Agent", "X-Forwarded-Method", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Forwarded-Uri", "X-Forwarded-For"} {
				if value := r.Header.Values(name); len(value) > 0 {
					request.Header[name] = append([]string{}, value...)
				}
			}
			request.Header.Set("X-Cove-Auth-Hop", "1")
			response, err := client.Do(request)
			if err != nil {
				http.Error(w, "Authentication service unavailable", 503)
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(io.LimitReader(response.Body, 32769))
			if err != nil || len(body) > 32768 {
				http.Error(w, "Authentication response unavailable", 503)
				return
			}
			for _, name := range append(append([]string{}, entry.Auth.ResponseHeaders...), "Location", "Content-Type", "Set-Cookie", "WWW-Authenticate") {
				for _, value := range response.Header.Values(name) {
					w.Header().Add(name, value)
				}
			}
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(response.StatusCode)
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}), nil
}

func writeRoutePage(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func RunRouteAuxiliary(ctx context.Context) error {
	data, err := os.ReadFile("/config/config.json")
	if err != nil {
		return err
	}
	var config routeAuxConfig
	if err = json.Unmarshal(data, &config); err != nil {
		return err
	}
	handler, err := newRouteAuxHandler(config, nil)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: ":8080", Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, WriteTimeout: 20 * time.Second, MaxHeaderBytes: 65536}
	lifetime, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() {
		<-lifetime.Done()
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		done <- server.Shutdown(cleanup)
	}()
	err = server.ListenAndServe()
	stop()
	shutdownErr := <-done
	if errors.Is(err, http.ErrServerClosed) {
		return shutdownErr
	}
	return errors.Join(err, shutdownErr)
}
