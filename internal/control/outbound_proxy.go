package control

import (
	"context"
	"cove/internal/outbound"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
)

type outboundProxyState struct {
	value atomic.Pointer[outbound.Config]
	saved atomic.Bool
	mu    sync.Mutex
}

func (s *Server) outboundConfig(ctx context.Context) (outbound.Config, error) {
	if saved := s.outboundState.value.Load(); saved != nil {
		return *saved, nil
	}
	var encrypted []byte
	if err := s.DB.QueryRow(ctx, "SELECT outbound_proxy FROM control_settings WHERE id=1").Scan(&encrypted); err != nil {
		return outbound.Config{}, err
	}
	if encrypted == nil {
		c := outbound.Environment()
		if err := c.Validate(); err != nil {
			return c, err
		}
		s.outboundState.value.Store(&c)
		return c, nil
	}
	plain, err := unseal(s.Key, encrypted)
	if err != nil {
		return outbound.Config{}, err
	}
	var c outbound.Config
	err = json.Unmarshal(plain, &c)
	if err == nil {
		err = c.Validate()
	}
	if err == nil {
		s.outboundState.saved.Store(true)
		s.outboundState.value.Store(&c)
	}
	return c, err
}

func (s *Server) outboundProxy(w http.ResponseWriter, r *http.Request) {
	s.outboundState.mu.Lock()
	defer s.outboundState.mu.Unlock()
	c, err := s.outboundConfig(r.Context())
	if err != nil {
		http.Error(w, "outbound settings unavailable", 503)
		return
	}
	if r.Method == "PUT" {
		var in struct {
			HTTP    *string `json:"http_proxy"`
			HTTPS   *string `json:"https_proxy"`
			NoProxy *string `json:"no_proxy"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.HTTP != nil {
			c.HTTP = *in.HTTP
		}
		if in.HTTPS != nil {
			c.HTTPS = *in.HTTPS
		}
		if in.NoProxy != nil {
			c.NoProxy = *in.NoProxy
		}
		if c.Validate() != nil {
			http.Error(w, "valid HTTP(S) proxy origins and NO_PROXY list required", 400)
			return
		}
		if c.HTTP != "" || c.HTTPS != "" {
			var publishing bool
			if err := s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM node_cloudflare_tunnels)").Scan(&publishing); err != nil {
				http.Error(w, "tunnel cleanup status unavailable", 503)
				return
			}
			if publishing {
				http.Error(w, "disable and finish Cloudflare Tunnel cleanup before enabling an HTTP outbound proxy", 409)
				return
			}
		}
		plain, e := json.Marshal(c)
		if e == nil {
			plain, e = seal(s.Key, plain)
		}
		if e == nil {
			_, e = s.DB.Exec(r.Context(), "UPDATE control_settings SET outbound_proxy=$1 WHERE id=1", plain)
		}
		if e != nil {
			http.Error(w, "could not save outbound settings", 503)
			return
		}
		s.outboundState.saved.Store(true)
		s.outboundState.value.Store(&c)
	}
	redact := func(value string) (string, bool) {
		u, e := url.Parse(value)
		if e != nil {
			return "", false
		}
		credentials := u.User != nil
		u.User = nil
		return u.String(), credentials
	}
	h, hc := redact(c.HTTP)
	t, tc := redact(c.HTTPS)
	respond(w, 200, map[string]any{"http_proxy": h, "https_proxy": t, "no_proxy": c.NoProxy, "http_credentials_set": hc, "https_credentials_set": tc})
}

type panelOutbound struct {
	server    *Server
	base      *http.Transport
	mu        sync.Mutex
	config    outbound.Config
	transport *http.Transport
}

func (p *panelOutbound) RoundTrip(r *http.Request) (*http.Response, error) {
	c, err := p.server.outboundConfig(r.Context())
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.transport == nil || p.config != c {
		transport := p.base.Clone()
		transport.Proxy = func(request *http.Request) (*url.URL, error) {
			host := request.URL.Host
			if request.URL.Port() == "" {
				port := "80"
				if request.URL.Scheme == "https" {
					port = "443"
				}
				host = net.JoinHostPort(request.URL.Hostname(), port)
			}
			return c.For(request.URL.Scheme, host)
		}
		if p.transport != nil {
			p.transport.CloseIdleConnections()
		}
		p.transport = transport
		p.config = c
	}
	transport := p.transport
	p.mu.Unlock()
	return transport.RoundTrip(r)
}

func (s *Server) OutboundTransport(base *http.Transport) http.RoundTripper {
	return &panelOutbound{server: s, base: base}
}

func (p *panelOutbound) CloseIdleConnections() {
	p.mu.Lock()
	transport := p.transport
	p.mu.Unlock()
	if transport != nil {
		transport.CloseIdleConnections()
	}
}

func (s *Server) hostProxyEnvironment(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare(tokenHash(bearer(r)), tokenHash(s.AdminToken)) != 1 {
		http.Error(w, "host administrator token required", 403)
		return
	}
	c, err := s.outboundConfig(r.Context())
	if err != nil {
		http.Error(w, "outbound configuration unavailable", 503)
		return
	}
	s.recordAudit(r, "host outbound configuration read", 200)
	respond(w, 200, c)
}

var errOutboundWorkerUnsupported = errors.New("upgrade and verify this worker before using instance outbound proxy settings")

func (s *Server) propagateOutbound(c outbound.Config) bool {
	return s.outboundState.saved.Load() || c.HTTP != "" || c.HTTPS != ""
}
