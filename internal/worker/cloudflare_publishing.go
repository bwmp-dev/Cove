package worker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

type cloudflareOriginSnapshot struct {
	Handler   http.Handler
	Revision  string
	CloseIdle func()
}

func loadCloudflareOrigin(path, revision string) (*cloudflareOriginSnapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("tunnel origin configuration unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 5*1024*1024+1))
	if err != nil {
		return nil, errors.New("tunnel origin configuration unavailable")
	}
	digest := sha256.Sum256(data)
	fingerprint := hex.EncodeToString(digest[:])
	if fingerprint == revision {
		return nil, nil
	}
	if len(data) > 5*1024*1024 {
		return nil, errors.New("tunnel origin configuration exceeds the node budget")
	}
	var config cloudflareOriginConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, errors.New("invalid tunnel origin configuration")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid tunnel origin configuration")
	}
	handler, closeIdle, err := newCloudflareOriginHandler(config)
	if err != nil {
		return nil, err
	}
	return &cloudflareOriginSnapshot{Handler: handler, Revision: fingerprint, CloseIdle: closeIdle}, nil
}

func RunCloudflareOrigin(ctx context.Context) error {
	initial, err := loadCloudflareOrigin("/config/relay.json", "")
	if err != nil {
		return err
	}
	var current atomic.Pointer[cloudflareOriginSnapshot]
	current.Store(initial)
	closed := &cloudflareOriginSnapshot{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "tunnel origin unavailable", http.StatusServiceUnavailable)
	}), CloseIdle: func() {}}
	health := &http.Server{Addr: "127.0.0.1:8081", ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 4096, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		state := current.Load()
		w.Header().Set("Cache-Control", "no-store")
		if state.Revision == "" {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(state.Revision))
	})}
	server := &http.Server{Addr: ":8443", ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 65536, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { current.Load().Handler.ServeHTTP(w, r) })}
	finished := make(chan error, 2)
	go func() { finished <- health.ListenAndServe() }()
	go func() { finished <- server.ListenAndServeTLS("/config/relay.crt", "/config/relay.key") }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer func() { current.Load().CloseIdle(); server.Close(); health.Close() }()
	for {
		select {
		case <-ctx.Done():
			shutdown, stop := context.WithTimeout(context.Background(), 15*time.Second)
			err := server.Shutdown(shutdown)
			stop()
			return err
		case err := <-finished:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ticker.C:
			next, err := loadCloudflareOrigin("/config/relay.json", current.Load().Revision)
			if err != nil {
				old := current.Swap(closed)
				if old.Revision != "" {
					fmt.Fprintln(os.Stderr, "Cloudflare Tunnel origin configuration unavailable; forwarding is closed.")
				}
				old.CloseIdle()
				continue
			}
			if next == nil {
				continue
			}
			old := current.Load()
			if old.Revision == "" {
				fmt.Fprintln(os.Stderr, "Cloudflare Tunnel origin configuration recovered.")
			}
			current.Store(next)
			old.CloseIdle()
		}
	}
}

type cloudflareOriginConfig struct {
	CAPEM string            `json:"system_ca_pem,omitempty"`
	Peer  string            `json:"peer"`
	Proxy string            `json:"proxy"`
	Roots map[string]string `json:"roots"`
}

func newCloudflareOriginHandler(config cloudflareOriginConfig) (http.Handler, func(), error) {
	peer, err := netip.ParseAddr(config.Peer)
	if err != nil || !peer.IsPrivate() || peer.Is4In6() || peer.Zone() != "" {
		return nil, nil, errors.New("invalid tunnel connector peer")
	}
	proxy, err := netip.ParseAddr(config.Proxy)
	if err != nil || !proxy.IsPrivate() || proxy.Is4In6() || proxy.Zone() != "" {
		return nil, nil, errors.New("invalid managed tunnel proxy address")
	}
	if len(config.Roots) > 128 {
		return nil, nil, errors.New("invalid tunnel origin hostname count")
	}
	handlers := map[string]*httputil.ReverseProxy{}
	transports := []*http.Transport{}
	closeIdle := func() {
		for _, transport := range transports {
			transport.CloseIdleConnections()
		}
	}
	total := len(config.CAPEM)
	for host, publicRoots := range config.Roots {
		total += len(publicRoots)
		if total > 4*1024*1024 {
			closeIdle()
			return nil, nil, errors.New("tunnel origin roots exceed the node budget")
		}
		if strings.Contains(host, "*") || ValidateRoutes([]Route{{Domain: host, Service: "origin", Port: 443}}, "service") != nil {
			closeIdle()
			return nil, nil, errors.New("invalid tunnel origin hostname")
		}
		roots, err := cloudflareOriginRootPool(config)
		if err != nil {
			closeIdle()
			return nil, nil, errors.New("system certificate roots unavailable")
		}
		if publicRoots != "" && (len(publicRoots) > 262144 || !roots.AppendCertsFromPEM([]byte(publicRoots))) {
			closeIdle()
			return nil, nil, errors.New("invalid tunnel origin certificate roots")
		}
		transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 600 * time.Second, MaxIdleConns: 16, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second}
		transports = append(transports, transport)
		target := &url.URL{Scheme: "https", Host: net.JoinHostPort(proxy.String(), "443")}
		handlers[host] = &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target); r.Out.Host = host }, Transport: transport, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "tunnel origin unavailable", http.StatusBadGateway)
		}}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		address, _, err := net.SplitHostPort(r.RemoteAddr)
		actual, parseErr := netip.ParseAddr(address)
		if err != nil || parseErr != nil || actual.Unmap() != peer {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		host := r.Host
		if strings.Contains(host, ":") {
			host, _, err = net.SplitHostPort(host)
			if err != nil {
				http.NotFound(w, r)
				return
			}
		}
		handler := handlers[strings.ToLower(host)]
		if handler == nil || r.URL.IsAbs() {
			http.NotFound(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	}), closeIdle, nil
}

const DefaultCloudflaredImage = "docker.io/cloudflare/cloudflared:2026.11.1@sha256:e613aadbc2d9511785d2e67a86b0c8ac18dd63a3f9af0d06faf04cbfdcb35f2d"

var cloudflaredImage = regexp.MustCompile(`^(?:docker\.io/)?cloudflare/cloudflared:[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?@sha256:[a-f0-9]{64}$`)

type CloudflarePublishing struct {
	ProveOnly    bool     `json:"prove_only,omitempty"`
	Image        string   `json:"image"`
	ID           string   `json:"id"`
	TunnelID     string   `json:"tunnel_id"`
	AccountID    string   `json:"account_id"`
	TunnelSecret string   `json:"tunnel_secret"`
	Hostnames    []string `json:"hostnames"`
}

var cloudflareTunnelID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var cloudflareAccountID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func ValidateCloudflarePublishing(value CloudflarePublishing) error {
	if err := ValidateCloudflaredImage(value.Image); err != nil {
		return err
	}
	secret, err := base64.StdEncoding.DecodeString(value.TunnelSecret)
	if !certificateIdentifier.MatchString(value.ID) || !cloudflareTunnelID.MatchString(value.TunnelID) || !cloudflareAccountID.MatchString(value.AccountID) || err != nil || len(secret) != 32 {
		return errors.New("invalid managed tunnel identity or connector credential")
	}
	if len(value.Hostnames) == 0 || len(value.Hostnames) > 128 {
		return errors.New("a tunnel must publish between one and 128 managed hostnames")
	}
	seen := map[string]bool{}
	for _, host := range value.Hostnames {
		if seen[host] || strings.Contains(host, "*") || ValidateRoutes([]Route{{Domain: host, Service: "origin", Port: 8443}}, "service") != nil {
			return errors.New("tunnel hostnames must be unique exact lowercase DNS names")
		}
		seen[host] = true
	}
	return nil
}

func ValidatePublishedRoutes(hostnames []string, routes map[string][]Route) error {
	for _, host := range hostnames {
		matched := false
		for _, list := range routes {
			for _, route := range list {
				if route.IsTransportRoute() || !RouteDomainsOverlap(host, route.Domain) {
					continue
				}
				if route.privateRoute() {
					return errors.New("private hostnames cannot be published through a Cloudflare Tunnel")
				}
				if route.Policy != nil && len(route.Policy.IPAllowList) > 0 {
					return errors.New("tunnel publishing cannot preserve direct-source IP allowlists; keep this hostname outside the tunnel")
				}
				if route.Domain == host {
					matched = true
				}
			}
		}
		if !matched {
			return errors.New("each published hostname must have an exact public HTTP route on this node")
		}
	}
	return nil
}

func cloudflareConnectorFiles(value CloudflarePublishing, origin string) ([]byte, []byte, error) {
	if err := ValidateCloudflarePublishing(value); err != nil {
		return nil, nil, err
	}
	address, err := netip.ParseAddr(origin)
	if err != nil || address.Zone() != "" || address.Is4In6() || !address.IsPrivate() {
		return nil, nil, errors.New("the tunnel origin must be an owned private container address")
	}
	credentials, err := json.Marshal(struct {
		AccountTag   string
		TunnelSecret string
		TunnelID     string
	}{value.AccountID, value.TunnelSecret, value.TunnelID})
	if err != nil {
		return nil, nil, err
	}
	entries := []map[string]any{}
	hosts := slices.Clone(value.Hostnames)
	slices.Sort(hosts)
	for _, host := range hosts {
		entries = append(entries, map[string]any{"hostname": host, "service": "https://" + net.JoinHostPort(address.String(), "8443"), "originRequest": map[string]any{"originServerName": "cove-tunnel-origin.internal", "caPool": "/config/origin.pem", "httpHostHeader": host}})
	}
	entries = append(entries, map[string]any{"service": "http_status:404"})
	config, err := json.Marshal(map[string]any{"tunnel": value.TunnelID, "credentials-file": "/config/credentials.json", "metrics": "127.0.0.1:20241", "edge-ip-version": "auto", "loglevel": "warn", "no-autoupdate": true, "ingress": entries})
	return config, credentials, err
}

func (e *Engine) cloudflareOriginRoots(hostnames []string, routes map[string][]Route) (map[string]string, error) {
	if err := ValidatePublishedRoutes(hostnames, routes); err != nil {
		return nil, err
	}
	roots := map[string]string{}
	for _, host := range hostnames {
		for workload, list := range routes {
			for _, route := range list {
				if route.IsTransportRoute() || route.Domain != host {
					continue
				}
				if route.Policy == nil || route.Policy.CertificateSource == "" || route.Policy.CertificateSource == "acme" {
					roots[host] = ""
					continue
				}
				if route.Policy.CertificateSource != "uploaded" || route.Policy.CertificateID == "" {
					return nil, errors.New("tunnel origins require a trusted ACME or selected stored certificate; the proxy default is not an origin identity")
				}
				certificate, err := e.readRouteCertificate(workload, route.Policy.CertificateID)
				if err != nil {
					return nil, errors.New("the selected tunnel origin certificate is unavailable")
				}
				if _, err := ParseRouteCertificate(certificate, time.Now()); err != nil || CertificateCoversRoute(certificate, host) != nil {
					return nil, errors.New("the selected tunnel origin certificate must be currently valid and cover its hostname")
				}
				block, _ := pem.Decode([]byte(certificate.CertificatePEM))
				// Trust the selected leaf, not an uploaded issuer that could authorize unrelated origin keys.
				roots[host] = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}))
			}
		}
	}
	return roots, nil
}

type cloudflareOriginIdentity struct {
	CertificatePEM string `json:"certificate_pem"`
	PrivateKeyPEM  string `json:"private_key_pem"`
}

func (e *Engine) ensureCloudflareOriginIdentity() (cloudflareOriginIdentity, error) {
	dir := filepath.Join(e.Root, "cloudflare-publishing")
	path := filepath.Join(dir, "identity.json")
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		var identity cloudflareOriginIdentity
		decoder := json.NewDecoder(io.LimitReader(file, 32769))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&identity); err != nil {
			return cloudflareOriginIdentity{}, errors.New("stored tunnel origin identity is unreadable")
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return cloudflareOriginIdentity{}, errors.New("stored tunnel origin identity is unreadable")
		}
		pair, err := tls.X509KeyPair([]byte(identity.CertificatePEM), []byte(identity.PrivateKeyPEM))
		if err != nil || len(pair.Certificate) != 1 {
			return cloudflareOriginIdentity{}, errors.New("stored tunnel origin identity is invalid")
		}
		certificate, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil || certificate.VerifyHostname("cove-tunnel-origin.internal") != nil || time.Now().Before(certificate.NotBefore) || !time.Now().Before(certificate.NotAfter) {
			return cloudflareOriginIdentity{}, errors.New("stored tunnel origin identity is not currently valid")
		}
		return identity, nil
	}
	if !os.IsNotExist(err) {
		return cloudflareOriginIdentity{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return cloudflareOriginIdentity{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return cloudflareOriginIdentity{}, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "cove-tunnel-origin.internal"}, DNSNames: []string{"cove-tunnel-origin.internal"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return cloudflareOriginIdentity{}, err
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return cloudflareOriginIdentity{}, err
	}
	identity := cloudflareOriginIdentity{CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return cloudflareOriginIdentity{}, err
	}
	if err := writeJSON(path, identity); err != nil {
		return cloudflareOriginIdentity{}, err
	}
	return identity, nil
}

func RunCloudflareOriginProbe(ctx context.Context) error {
	data, err := os.ReadFile("/config/relay.json")
	if err != nil || len(data) > 5*1024*1024 {
		return errors.New("tunnel origin configuration unavailable")
	}
	var config cloudflareOriginConfig
	if json.Unmarshal(data, &config) != nil {
		return errors.New("invalid tunnel origin configuration")
	}
	_, closeIdle, err := newCloudflareOriginHandler(config)
	if err != nil {
		return err
	}
	defer closeIdle()
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for host, public := range config.Roots {
		roots, err := cloudflareOriginRootPool(config)
		if err != nil {
			return errors.New("system certificate roots unavailable")
		}
		if public != "" && !roots.AppendCertsFromPEM([]byte(public)) {
			return errors.New("invalid origin identity")
		}
		dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}}
		connection, err := dialer.DialContext(probe, "tcp", net.JoinHostPort(config.Proxy, "443"))
		if err != nil {
			return errors.New("managed proxy origin certificate is not verified")
		}
		connection.Close()
	}
	return nil
}

func cloudflareOriginRootPool(config cloudflareOriginConfig) (*x509.CertPool, error) {
	if config.CAPEM == "" {
		return x509.SystemCertPool()
	}
	roots := x509.NewCertPool()
	if len(config.CAPEM) > 1024*1024 || !roots.AppendCertsFromPEM([]byte(config.CAPEM)) {
		return nil, errors.New("invalid public system certificate roots")
	}
	return roots, nil
}

func ValidateCloudflaredImage(image string) error {
	if image != "" && !cloudflaredImage.MatchString(image) {
		return errors.New("connector image must select an official cloudflare/cloudflared version pinned by SHA-256 digest")
	}
	return nil
}

func RunCloudflareConnectorOriginProbe(ctx context.Context) error {
	data, err := os.ReadFile("/config/config.json")
	if err != nil || len(data) > 512*1024 {
		return errors.New("connector origin configuration unavailable")
	}
	var config struct {
		Ingress []struct {
			Hostname string `json:"hostname"`
			Service  string `json:"service"`
			Origin   struct {
				ServerName string `json:"originServerName"`
				CAPool     string `json:"caPool"`
			} `json:"originRequest"`
		} `json:"ingress"`
	}
	if json.Unmarshal(data, &config) != nil {
		return errors.New("connector origin configuration invalid")
	}
	root, err := os.ReadFile("/config/origin.pem")
	if err != nil || len(root) > 16384 {
		return errors.New("connector origin identity unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(root) {
		return errors.New("connector origin identity invalid")
	}
	for _, entry := range config.Ingress {
		if entry.Hostname == "" {
			continue
		}
		target, err := url.Parse(entry.Service)
		if err != nil || target.Scheme != "https" || target.Port() != "8443" || target.User != nil || target.Path != "" || entry.Origin.ServerName != "cove-tunnel-origin.internal" || entry.Origin.CAPool != "/config/origin.pem" {
			return errors.New("invalid connector TLS origin")
		}
		address, err := netip.ParseAddr(target.Hostname())
		if err != nil || !address.IsPrivate() || address.Zone() != "" || address.Is4In6() {
			return errors.New("invalid connector TLS origin")
		}
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: &tls.Config{RootCAs: roots, ServerName: entry.Origin.ServerName, MinVersion: tls.VersionTLS13}}
		connection, err := dialer.DialContext(probe, "tcp", target.Host)
		if err != nil {
			return errors.New("the connector has not verified its local origin TLS identity")
		}
		connection.Close()
		return nil
	}
	return errors.New("the connector has no managed TLS origin")
}
