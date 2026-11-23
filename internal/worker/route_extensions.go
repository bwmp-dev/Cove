package worker

import (
	"cove/internal/netpolicy"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

type RouteMaintenance struct {
	Enabled bool   `json:"enabled"`
	HTML    string `json:"html,omitempty"`
}

type RouteForwardAuth struct {
	URL             string   `json:"url,omitempty"`
	Service         string   `json:"service,omitempty"`
	Port            int      `json:"port,omitempty"`
	Path            string   `json:"path,omitempty"`
	ResponseHeaders []string `json:"response_headers,omitempty"`
	ResponseCookies []string `json:"response_cookies,omitempty"`
}

func HasRouteExtensions(routes []Route) bool {
	for _, route := range routes {
		if p := route.Policy; p != nil && (p.Maintenance != nil || len(p.ErrorPages) > 0 || p.ForwardAuth != nil) {
			return true
		}
	}
	return false
}

func validateRouteExtensions(p *RoutePolicy) error {
	if p == nil {
		return nil
	}
	if p.Maintenance != nil && (len(p.Maintenance.HTML) > 16384 || strings.ContainsRune(p.Maintenance.HTML, 0)) {
		return errors.New("maintenance HTML must be at most 16 KiB without NUL characters")
	}
	if len(p.ErrorPages) > 8 {
		return errors.New("at most eight custom error pages are supported")
	}
	total := 0
	for code, body := range p.ErrorPages {
		status, err := strconv.Atoi(code)
		if err != nil || strconv.Itoa(status) != code || status < 400 || status > 599 || len(body) == 0 || len(body) > 16384 || strings.ContainsRune(body, 0) {
			return errors.New("custom error pages require an HTTP status from 400 to 599 and nonempty HTML up to 16 KiB")
		}
		total += len(body)
	}
	if total > 65536 {
		return errors.New("custom error pages total at most 64 KiB per route")
	}
	auth := p.ForwardAuth
	if auth == nil {
		return nil
	}
	if auth.URL != "" {
		endpoint, err := url.Parse(auth.URL)
		if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Fragment != "" || len(auth.URL) > 2048 || strings.ContainsFunc(auth.URL, func(ch rune) bool { return ch < 32 || ch == 127 }) || auth.Service != "" || auth.Port != 0 || auth.Path != "" {
			return errors.New("forward-auth requires a public HTTPS URL without credentials, or one owned Compose service and port")
		}
		if ip := net.ParseIP(endpoint.Hostname()); ip != nil && !netpolicy.PublicIP(ip) {
			return errors.New("forward-auth URLs cannot target private or reserved IP addresses; select an owned Compose service instead")
		}
	} else {
		if !serviceIdentifier.MatchString(auth.Service) || auth.Port < 1 || auth.Port > 65535 {
			return errors.New("forward-auth requires an owned Compose service and port")
		}
		if auth.Path != "" {
			path, err := url.ParseRequestURI(auth.Path)
			if err != nil || !strings.HasPrefix(auth.Path, "/") || strings.HasPrefix(auth.Path, "//") || path.IsAbs() || path.Fragment != "" || len(auth.Path) > 2048 || strings.ContainsFunc(auth.Path, func(ch rune) bool { return ch < 32 || ch == 127 }) {
				return errors.New("forward-auth path must be an absolute path with an optional query")
			}
		}
	}
	if len(auth.ResponseHeaders) > 32 || len(auth.ResponseCookies) > 16 {
		return errors.New("forward-auth supports at most 32 response headers and 16 response cookie names")
	}
	seen := map[string]bool{}
	for _, name := range auth.ResponseHeaders {
		key := strings.ToLower(name)
		if !headerName.MatchString(name) || len(name) > 128 || seen[key] || key == "set-cookie" || key == "cookie" || protectedRouteHeader(key) {
			return errors.New("invalid, duplicate or proxy-controlled forward-auth response header")
		}
		seen[key] = true
	}
	seen = map[string]bool{}
	for _, name := range auth.ResponseCookies {
		if !headerName.MatchString(name) || len(name) > 128 || seen[name] {
			return errors.New("invalid or duplicate forward-auth response cookie name")
		}
		seen[name] = true
	}
	return nil
}

func protectedRouteHeader(key string) bool {
	return key == "x-cove-auth-hop" || key == "host" || key == "forwarded" || strings.HasPrefix(key, "x-forwarded-") || key == "connection" || key == "keep-alive" || key == "proxy-connection" || key == "upgrade" || key == "transfer-encoding" || key == "content-length" || key == "te" || key == "trailer" || key == "proxy-authorization" || key == "proxy-authenticate"
}

func idleAuthEligibility(routes []Route) error {
	domains := map[string]bool{}
	for _, route := range routes {
		domains[route.Domain] = true
	}
	for _, route := range routes {
		if route.Policy == nil || route.Policy.ForwardAuth == nil {
			continue
		}
		auth := route.Policy.ForwardAuth
		if auth.Service != "" {
			return errors.New("idle sleep requires the forward-auth provider to run independently; use its public HTTPS endpoint")
		}
		endpoint, err := url.Parse(auth.URL)
		if err != nil || domains[endpoint.Hostname()] {
			return errors.New("an idle app cannot depend on itself for forward-auth")
		}
	}
	return nil
}
