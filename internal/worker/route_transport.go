package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"reflect"
	"slices"
	"strconv"
)

func (r Route) IsTransportRoute() bool { return r.Protocol == "tcp" || r.Protocol == "udp" }

func HasTransportRoutes(routes []Route) bool {
	return slices.ContainsFunc(routes, Route.IsTransportRoute)
}

func (r Route) protocol() string {
	if r.Protocol == "" {
		return "http"
	}
	return r.Protocol
}

func (r Route) listener() string {
	if r.Policy == nil {
		return ""
	}
	return r.Policy.Listener
}

func validateTransportRoute(r Route) error {
	if !r.IsTransportRoute() || r.Domain != "" || r.Auth != "" || !serviceIdentifier.MatchString(r.Service) || r.Port < 1 || r.Port > 65535 || r.Policy == nil {
		return errors.New("TCP and UDP routes require a service, port and dedicated listener without a hostname or HTTP authentication")
	}
	p := r.Policy
	if !workloadIdentifier.MatchString(p.Listener) || len(p.Listener) > 32 || !slices.Contains([]string{"public", "lan", "tailscale"}, p.Visibility) {
		return errors.New("TCP and UDP routes require explicit public, lan or tailscale visibility and a managed listener")
	}
	allowed := *p
	allowed.Listener, allowed.Visibility = "", ""
	allowed.Upstreams = nil
	if r.Protocol == "tcp" {
		allowed.IPAllowList = nil
	}
	if !reflect.DeepEqual(allowed, RoutePolicy{}) {
		return errors.New("raw routes cannot use HTTP paths, authentication, headers, redirects, certificates, maintenance pages, timeouts or sticky cookies")
	}
	if err := validateRouteUpstreams(r); err != nil {
		return err
	}
	if r.Protocol == "tcp" {
		return validateRouteAllowList(p.IPAllowList)
	}
	return nil
}

type ingressTransportRouter struct {
	EntryPoints []string `json:"entryPoints"`
	Rule        string   `json:"rule,omitempty"`
	Service     string   `json:"service"`
	Middlewares []string `json:"middlewares,omitempty"`
}

type ingressTransportBalancer struct {
	Servers []map[string]string `json:"servers"`
}

type ingressTransportService struct {
	LoadBalancer *ingressTransportBalancer `json:"loadBalancer,omitempty"`
}

type ingressTransportConfig struct {
	Routers     map[string]ingressTransportRouter  `json:"routers,omitempty"`
	Services    map[string]ingressTransportService `json:"services,omitempty"`
	Middlewares map[string]ingressMiddleware       `json:"middlewares,omitempty"`
}

type ingressHTTP struct {
	Routers           map[string]ingressRouter           `json:"routers,omitempty"`
	Services          map[string]ingressService          `json:"services,omitempty"`
	Middlewares       map[string]ingressMiddleware       `json:"middlewares,omitempty"`
	ServersTransports map[string]ingressServersTransport `json:"serversTransports,omitempty"`
}

func (c dynamicIngress) MarshalJSON() ([]byte, error) {
	var http *ingressHTTP
	if len(c.HTTP.Routers)+len(c.HTTP.Services)+len(c.HTTP.Middlewares)+len(c.HTTP.ServersTransports) > 0 {
		http = &c.HTTP
	}
	return json.Marshal(struct {
		TLS  *ingressTLS             `json:"tls,omitempty"`
		HTTP *ingressHTTP            `json:"http,omitempty"`
		TCP  *ingressTransportConfig `json:"tcp,omitempty"`
		UDP  *ingressTransportConfig `json:"udp,omitempty"`
	}{c.TLS, http, c.TCP, c.UDP})
}

func (c dynamicIngress) empty() bool {
	return len(c.HTTP.Routers) == 0 && (c.TCP == nil || len(c.TCP.Routers) == 0) && (c.UDP == nil || len(c.UDP.Routers) == 0)
}

func validateTransportOwners(routes map[string][]Route) error {
	owners := map[string]string{}
	for workload, list := range routes {
		for _, route := range list {
			if !route.IsTransportRoute() {
				continue
			}
			if previous := owners[route.listener()]; previous != "" && previous != workload {
				return errors.New("dedicated transport listener already belongs to another resource")
			}
			owners[route.listener()] = workload
		}
	}
	return nil
}

func (c *dynamicIngress) transport(protocol string) *ingressTransportConfig {
	target := &c.TCP
	if protocol == "udp" {
		target = &c.UDP
	}
	if *target == nil {
		*target = &ingressTransportConfig{Routers: map[string]ingressTransportRouter{}, Services: map[string]ingressTransportService{}}
	}
	return *target
}

func (c *dynamicIngress) addTransportRoute(workload string, route Route) {
	servers := []map[string]string{}
	for _, target := range routeTargets([]Route{route}) {
		servers = append(servers, map[string]string{"address": net.JoinHostPort(routeAlias(workload, target.Service), strconv.Itoa(target.Port))})
	}
	c.setTransportRoute(route, servers)
}

func (c *dynamicIngress) setTransportRoute(route Route, servers []map[string]string) {
	data, _ := json.Marshal(struct {
		Route   Route
		Servers []map[string]string
	}{route, servers})
	digest := sha256.Sum256(data)
	name := "cove-route-" + route.Protocol + "-" + hex.EncodeToString(digest[:16])
	config := c.transport(route.Protocol)
	router := ingressTransportRouter{EntryPoints: []string{ingressListenerName(route.listener())}, Service: name}
	if route.Protocol == "tcp" {
		router.Rule = "HostSNI(`*`)"
		add := func(suffix string, ranges []string) {
			if len(ranges) == 0 {
				return
			}
			if config.Middlewares == nil {
				config.Middlewares = map[string]ingressMiddleware{}
			}
			key := name + suffix
			config.Middlewares[key] = ingressMiddleware{IPAllowList: map[string]any{"sourceRange": ranges}}
			router.Middlewares = append(router.Middlewares, key)
		}
		add("-visibility", privateRouteRanges(route.Policy.Visibility))
		add("-allow", route.Policy.IPAllowList)
	}
	config.Routers[name] = router
	config.Services[name] = ingressTransportService{LoadBalancer: &ingressTransportBalancer{Servers: servers}}
}

func (e *Engine) addTransportTraffic(ctx context.Context, workload string, route Route, traffic trafficState, paused bool, config *dynamicIngress, out io.Writer) error {
	if paused {
		return nil
	}
	servers := []map[string]string{}
	seen := map[string]bool{}
	for _, target := range routeTargets([]Route{route}) {
		generations := traffic[target.Service]
		if len(generations) > 1 || len(generations) == 1 && generations[0].Weight != 100 {
			return errors.New("promote or abort the staged HTTP rollout before exposing raw traffic")
		}
		containers, err := e.trafficContainers(ctx, workload, target.Service, true)
		if err != nil {
			return err
		}
		for _, id := range containers {
			if !containerIdentity.MatchString(id) {
				return errors.New("invalid transport container identity")
			}
			address, err := e.attachTrafficContainer(ctx, workload, id, out, routeAlias(workload, target.Service))
			if err != nil {
				return err
			}
			endpoint := net.JoinHostPort(address, strconv.Itoa(target.Port))
			if seen[endpoint] {
				continue
			}
			seen[endpoint] = true
			servers = append(servers, map[string]string{"address": endpoint})
		}
	}
	if len(servers) > 0 {
		config.setTransportRoute(route, servers)
	}
	return nil
}
