package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

type RouteUpstream struct {
	Service string `json:"service"`
	Port    int    `json:"port"`
}

type RoutePolicy struct {
	PathPrefix        string            `json:"path_prefix,omitempty"`
	RequestHeaders    map[string]string `json:"request_headers,omitempty"`
	ResponseHeaders   map[string]string `json:"response_headers,omitempty"`
	SecurityHeaders   bool              `json:"security_headers,omitempty"`
	IPAllowList       []string          `json:"ip_allow_list,omitempty"`
	RedirectHost      string            `json:"redirect_host,omitempty"`
	RedirectPermanent bool              `json:"redirect_permanent,omitempty"`
	CertificateSource string            `json:"certificate_source,omitempty"`
	Upstreams         []RouteUpstream   `json:"upstreams,omitempty"`
	Sticky            bool              `json:"sticky,omitempty"`
}

func HasRoutePolicies(routes []Route) bool {
	return slices.ContainsFunc(routes, func(r Route) bool { return r.Policy != nil })
}

func RoutesEqual(a, b []Route) bool {
	return slices.EqualFunc(a, b, func(x, y Route) bool { return reflect.DeepEqual(x, y) })
}

func (r Route) pathPrefix() string {
	if r.Policy == nil {
		return ""
	}
	return r.Policy.PathPrefix
}

var routePath = regexp.MustCompile(`^/[a-zA-Z0-9/_.~%+@:-]*$`)
var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func (r Route) validatePolicy() error {
	p := r.Policy
	if p == nil {
		return nil
	}
	if len(p.PathPrefix) > 1024 || (p.PathPrefix != "" && !routePath.MatchString(p.PathPrefix)) {
		return errors.New("route path prefix must be an absolute URL path without query or rule syntax")
	}
	if p.CertificateSource != "" && p.CertificateSource != "acme" && p.CertificateSource != "default" {
		return errors.New("route certificate source must be acme or default")
	}
	if p.RedirectHost != "" {
		if p.RedirectHost == r.Domain || ValidateRoutes([]Route{{Domain: p.RedirectHost, Service: r.Service, Port: r.Port}}, "service") != nil {
			return errors.New("redirect target must be a different lowercase DNS hostname")
		}
	} else if p.RedirectPermanent {
		return errors.New("permanent redirect requires a target hostname")
	}
	for _, headers := range []map[string]string{p.RequestHeaders, p.ResponseHeaders} {
		if len(headers) > 32 {
			return errors.New("at most 32 custom headers are allowed")
		}
		names := map[string]bool{}
		for name, value := range headers {
			key := strings.ToLower(name)
			if !headerName.MatchString(name) || len(name) > 128 || len(value) > 4096 || strings.ContainsFunc(value, func(r rune) bool { return r < 32 || r == 127 }) || names[key] {
				return errors.New("invalid or duplicate custom header")
			}
			// Proxy identity and hop-by-hop headers must remain under Traefik's control.
			if key == "host" || key == "forwarded" || strings.HasPrefix(key, "x-forwarded-") || slices.Contains([]string{"connection", "upgrade", "transfer-encoding", "content-length", "te", "trailer", "proxy-authorization", "proxy-authenticate"}, key) {
				return errors.New("custom headers cannot override proxy identity or connection headers")
			}
			if p.SecurityHeaders && slices.Contains([]string{"strict-transport-security", "x-content-type-options", "x-frame-options", "referrer-policy"}, key) {
				return errors.New("custom headers conflict with security preset")
			}
			names[key] = true
		}
	}
	if len(p.IPAllowList) > 64 {
		return errors.New("at most 64 source CIDRs are allowed")
	}
	for _, cidr := range p.IPAllowList {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return errors.New("IP allowlist entries must be IPv4 or IPv6 CIDRs")
		}
	}
	if len(p.Upstreams) > 15 {
		return errors.New("at most 16 route upstreams are allowed including the primary service")
	}
	seen := map[RouteUpstream]bool{{Service: r.Service, Port: r.Port}: true}
	for _, target := range p.Upstreams {
		if !serviceIdentifier.MatchString(target.Service) || target.Port < 1 || target.Port > 65535 || seen[target] {
			return errors.New("invalid or duplicate route upstream")
		}
		seen[target] = true
	}
	return nil
}

func routeTargets(routes []Route) []Route {
	targets := []Route{}
	for _, r := range routes {
		targets = append(targets, Route{Service: r.Service, Port: r.Port})
		if r.Policy != nil {
			for _, target := range r.Policy.Upstreams {
				targets = append(targets, Route{Service: target.Service, Port: target.Port})
			}
		}
	}
	return targets
}

func (c *dynamicIngress) addRoute(workload string, r Route) {
	upstream := fmt.Sprintf("http://%s:%d", routeAlias(workload, r.Service), r.Port)
	if r.Policy == nil {
		c.add(r.Domain, upstream, r.Auth)
		return
	}
	// Include the complete policy in object identities so live confirmation cannot accept a stale middleware or upstream.
	data, _ := json.Marshal(r)
	sum := sha256.Sum256(data)
	next := "cove-route-" + hex.EncodeToString(sum[:16])
	c.addNamed(r.Domain, upstream, r.Auth, next)
	router, service := c.HTTP.Routers[next], c.HTTP.Services[next]
	p := r.Policy
	if p.PathPrefix != "" {
		router.Rule += " && PathPrefix(`" + p.PathPrefix + "`)"
	}
	if p.CertificateSource == "default" {
		router.TLS = map[string]string{}
	}
	if c.HTTP.Middlewares == nil {
		c.HTTP.Middlewares = map[string]ingressMiddleware{}
	}
	add := func(suffix string, middleware ingressMiddleware) {
		key := next + suffix
		c.HTTP.Middlewares[key] = middleware
		router.Middlewares = append(router.Middlewares, key)
	}
	if len(p.IPAllowList) > 0 {
		add("-allow", ingressMiddleware{IPAllowList: map[string]any{"sourceRange": p.IPAllowList}})
	}
	if len(p.RequestHeaders) > 0 || len(p.ResponseHeaders) > 0 || p.SecurityHeaders {
		headers := map[string]any{}
		if len(p.RequestHeaders) > 0 {
			headers["customRequestHeaders"] = p.RequestHeaders
		}
		if len(p.ResponseHeaders) > 0 {
			headers["customResponseHeaders"] = p.ResponseHeaders
		}
		if p.SecurityHeaders {
			headers["stsSeconds"] = 31536000
			headers["contentTypeNosniff"] = true
			headers["frameDeny"] = true
			headers["referrerPolicy"] = "strict-origin-when-cross-origin"
		}
		add("-headers", ingressMiddleware{Headers: headers})
	}
	if p.RedirectHost != "" {
		add("-redirect", ingressMiddleware{RedirectRegex: map[string]any{"regex": "^https?://" + regexp.QuoteMeta(r.Domain) + "(?::[0-9]+)?(.*)$", "replacement": "https://" + p.RedirectHost + "${1}", "permanent": p.RedirectPermanent}})
	}
	for _, target := range p.Upstreams {
		service.LoadBalancer.Servers = append(service.LoadBalancer.Servers, map[string]any{"url": fmt.Sprintf("http://%s:%d", routeAlias(workload, target.Service), target.Port)})
	}
	if p.Sticky {
		service.LoadBalancer.Sticky = map[string]any{"cookie": map[string]any{"name": "cove_affinity_" + hex.EncodeToString(sum[:8]), "secure": true, "httpOnly": true, "sameSite": "lax"}}
	}
	c.HTTP.Routers[next], c.HTTP.Services[next] = router, service
}
