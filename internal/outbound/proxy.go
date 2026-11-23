package outbound

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	HTTP    string `json:"http_proxy"`
	HTTPS   string `json:"https_proxy"`
	NoProxy string `json:"no_proxy"`
}

func Environment() Config {
	get := func(key string) string {
		if value, ok := os.LookupEnv(key); ok {
			return value
		}
		return os.Getenv(strings.ToLower(key))
	}
	return Config{get("HTTP_PROXY"), get("HTTPS_PROXY"), get("NO_PROXY")}
}

func (c Config) Validate() error {
	for _, value := range []string{c.HTTP, c.HTTPS} {
		if value == "" {
			continue
		}
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Fragment != "" || u.RawQuery != "" || (u.Path != "" && u.Path != "/") || len(value) > 8192 || strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("proxy must be an HTTP(S) origin")
		}
		if u.Port() != "" {
			if _, err := net.LookupPort("tcp", u.Port()); err != nil {
				return errors.New("invalid proxy port")
			}
		}
	}
	if len(c.NoProxy) > 8192 || strings.ContainsAny(c.NoProxy, "\r\n\x00") {
		return errors.New("invalid NO_PROXY list")
	}
	return nil
}

func (c Config) Env() []string {
	return []string{"HTTP_PROXY=" + c.HTTP, "http_proxy=" + c.HTTP, "HTTPS_PROXY=" + c.HTTPS, "https_proxy=" + c.HTTPS, "NO_PROXY=" + c.NoProxy, "no_proxy=" + c.NoProxy}
}

func (c Config) Secrets() []string {
	values := []string{c.HTTP, c.HTTPS}
	for _, value := range []string{c.HTTP, c.HTTPS} {
		u, err := url.Parse(value)
		if err == nil && u.User != nil {
			password, _ := u.User.Password()
			if password != "" {
				values = append(values, password, url.QueryEscape(password), url.PathEscape(password))
			}
		}
	}
	return values
}

func (c Config) For(scheme, hostport string) (*url.URL, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
		port = ""
	}
	host = strings.Trim(host, "[]")
	ip, _ := netip.ParseAddr(host)
	if strings.EqualFold(host, "localhost") || ip.IsValid() && ip.IsLoopback() {
		return nil, nil
	}
	for _, entry := range strings.Split(c.NoProxy, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == "*" {
			return nil, nil
		}
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			if ip.IsValid() && prefix.Contains(ip.Unmap()) {
				return nil, nil
			}
			continue
		}
		name, p, e := net.SplitHostPort(entry)
		if e == nil {
			if p != port {
				continue
			}
		} else {
			name = strings.Trim(entry, "[]")
		}
		if addr, e := netip.ParseAddr(name); e == nil {
			if ip.IsValid() && addr.Unmap() == ip.Unmap() {
				return nil, nil
			}
			continue
		}
		name = strings.ToLower(name)
		h := strings.ToLower(host)
		subOnly := strings.HasPrefix(name, ".") || strings.HasPrefix(name, "*.")
		name = strings.TrimPrefix(strings.TrimPrefix(name, "*"), ".")
		if (!subOnly && h == name) || strings.HasSuffix(h, "."+name) {
			return nil, nil
		}
	}
	value := c.HTTP
	if scheme == "https" {
		value = c.HTTPS
	}
	if value == "" {
		return nil, nil
	}
	return url.Parse(value)
}

// Tunnel receives an already validated destination IP for public-only callers.
// CONNECT carries that IP rather than letting the proxy resolve a different address.
func Tunnel(ctx context.Context, proxy *url.URL, network, target string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if proxy == nil {
		return dialer.DialContext(ctx, network, target)
	}
	port := proxy.Port()
	if port == "" {
		port = "80"
		if proxy.Scheme == "https" {
			port = "443"
		}
	}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(proxy.Hostname(), port))
	if err != nil {
		return nil, errors.New("proxy connection failed")
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	if d, yes := ctx.Deadline(); yes && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	if proxy.Scheme == "https" {
		secure := tls.Client(conn, &tls.Config{ServerName: proxy.Hostname(), MinVersion: tls.VersionTLS12})
		if err := secure.HandshakeContext(ctx); err != nil {
			return nil, errors.New("proxy TLS failed")
		}
		conn = secure
	}
	req := &http.Request{Method: "CONNECT", URL: &url.URL{Opaque: target}, Host: target, Header: make(http.Header)}
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(proxy.User.Username()+":"+password)))
	}
	if req.Write(conn) != nil {
		return nil, errors.New("proxy CONNECT failed")
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, req)
	if err != nil || response.StatusCode != 200 {
		return nil, errors.New("proxy refused CONNECT")
	}
	if reader.Buffered() != 0 {
		return nil, errors.New("unexpected proxy CONNECT data")
	}
	conn.SetDeadline(time.Time{})
	ok = true
	return conn, nil
}
