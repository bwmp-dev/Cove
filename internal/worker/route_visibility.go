package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const privateRouteDenialPriority = math.MaxInt - 2000

func validateRouteListener(protocol, visibility, id string, settings IngressExposureSettings) error {
	if visibility == "" {
		visibility = "public"
	}
	if protocol == "http" && visibility == "public" {
		if id != "" {
			return errors.New("public HTTP routes use the public HTTPS entrypoint")
		}
		return nil
	}
	if id == "" {
		return errors.New("this route requires an explicitly selected managed listener")
	}
	for _, listener := range settings.Listeners {
		if listener.ID != id {
			continue
		}
		kind := protocol
		if kind == "http" {
			kind = "https"
		}
		if listener.Protocol != kind || listener.Visibility != visibility {
			return errors.New("selected listener protocol or visibility no longer matches the route")
		}
		return nil
	}
	return errors.New("selected managed listener does not exist on this node")
}

func privateRouteDenial(domain string) ingressRouter {
	return ingressRouter{Rule: routeHostRule(domain), Priority: privateRouteDenialPriority, EntryPoints: []string{"websecure"}, Service: "noop@internal", TLS: map[string]string{}}
}

func privateRouteRanges(visibility string) []string {
	if visibility == "tailscale" {
		return []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"}
	}
	if visibility != "lan" {
		return nil
	}
	ranges := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	current := netip.MustParsePrefix("fc00::/7")
	excluded := netip.MustParsePrefix("fd7a:115c:a1e0::/48")
	// A LAN allowlist must not admit Tailscale's ULA range through the larger private IPv6 prefix.
	for current.Bits() < excluded.Bits() {
		bits := current.Bits() + 1
		first := netip.PrefixFrom(current.Addr(), bits)
		address := current.Addr().As16()
		address[(bits-1)/8] |= byte(1 << uint(7-(bits-1)%8))
		second := netip.PrefixFrom(netip.AddrFrom16(address), bits)
		if first.Contains(excluded.Addr()) {
			ranges = append(ranges, second.String())
			current = first
		} else {
			ranges = append(ranges, first.String())
			current = second
		}
	}
	return ranges
}

func (r Route) privateRoute() bool {
	return !r.IsTransportRoute() && r.Policy != nil && (r.Policy.Visibility == "lan" || r.Policy.Visibility == "tailscale")
}

func validatePrivateRoute(r Route) error {
	p := r.Policy
	if p == nil {
		return nil
	}
	if !slices.Contains([]string{"", "public", "lan", "tailscale"}, p.Visibility) {
		return errors.New("route visibility must be public, lan or tailscale")
	}
	if !r.privateRoute() {
		if p.Listener != "" {
			return errors.New("public HTTP routes cannot select a private listener")
		}
		return nil
	}
	if !workloadIdentifier.MatchString(p.Listener) || len(p.Listener) > 32 {
		return errors.New("private routes require an explicit managed listener")
	}
	if p.CertificateSource != "default" && p.CertificateSource != "uploaded" {
		return errors.New("private routes require a stored certificate or the proxy default; public ACME is disabled")
	}
	return nil
}

func validateRouteVisibilities(routes []Route) error {
	for i, route := range routes {
		if route.IsTransportRoute() {
			continue
		}
		for _, other := range routes[i+1:] {
			if other.IsTransportRoute() {
				continue
			}
			if !RouteDomainsOverlap(route.Domain, other.Domain) {
				continue
			}
			visibility, listener, otherVisibility, otherListener := "public", "", "public", ""
			if route.Policy != nil {
				visibility, listener = route.Policy.Visibility, route.Policy.Listener
			}
			if other.Policy != nil {
				otherVisibility, otherListener = other.Policy.Visibility, other.Policy.Listener
			}
			if visibility == "" {
				visibility = "public"
			}
			if otherVisibility == "" {
				otherVisibility = "public"
			}
			if visibility != otherVisibility || listener != otherListener {
				return errors.New("all paths and overlapping wildcard hostnames must use the same route visibility and listener")
			}
		}
	}
	return nil
}

func privateRouteGuards(routes map[string][]Route) map[string]ingressRouter {
	guards := map[string]ingressRouter{}
	for _, list := range routes {
		for _, route := range list {
			if !route.privateRoute() {
				continue
			}
			digest := sha256.Sum256([]byte(route.Domain))
			guards["cove-route-deny-"+hex.EncodeToString(digest[:16])] = privateRouteDenial(route.Domain)
			http := privateRouteDenial(route.Domain)
			http.EntryPoints, http.TLS = []string{"web"}, nil
			guards["cove-route-deny-http-"+hex.EncodeToString(digest[:16])] = http
		}
	}
	return guards
}

func (c *dynamicIngress) addPrivateDenials(routes map[string][]Route) {
	for name, guard := range privateRouteGuards(routes) {
		c.HTTP.Routers[name] = guard
	}
}

func ValidateRouteListeners(routes []Route, settings IngressExposureSettings) error {
	for _, route := range routes {
		visibility, listener := "", ""
		if route.Policy != nil {
			visibility, listener = route.Policy.Visibility, route.Policy.Listener
		}
		if err := validateRouteListener(route.protocol(), visibility, listener, settings); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) validateManagedRouteIngress(ctx context.Context, routes map[string][]Route) error {
	if len(privateRouteGuards(routes)) == 0 && len(transportRouteReferences(routes)) == 0 {
		return nil
	}
	settings, err := e.loadIngressExposures()
	if err != nil {
		return err
	}
	for _, list := range routes {
		if err := ValidateRouteListeners(list, settings.Settings); err != nil {
			return err
		}
	}
	client := e.ingressDockerClient()
	defer client.CloseIdleConnections()
	snapshot, err := captureIngressProxy(ctx, client)
	if err != nil {
		return err
	}
	var source []byte
	if snapshot.Launch.Kind == "file" {
		out := &limitedBuffer{Limit: 1 << 20}
		if err := copyIngressProxyFile(ctx, client, snapshot.ID, snapshot.Launch.StaticFile, out, 1<<20); err != nil {
			return errors.New("private routes require readable managed static configuration")
		}
		source = out.data.Bytes()
	}
	if err := validateCoveFileProvider(snapshot, source); err != nil {
		return err
	}
	entries, err := ingressStaticEntrypoints(snapshot, source)
	if err != nil {
		return err
	}
	verifiedSettings := settings.Settings
	if verifiedSettings.Public == nil {
		public, err := proxyPublicBindings(snapshot)
		if err != nil {
			return err
		}
		verifiedSettings.Public = &public
	}
	if _, err := allocateIngressListenerPorts(verifiedSettings, settings.Ports, entries); err != nil {
		return err
	}
	var host struct {
		PortBindings map[string][]ingressDockerBinding
	}
	if json.Unmarshal(snapshot.HostConfig, &host) != nil {
		return errors.New("private listener publications are unreadable")
	}
	for _, list := range routes {
		for _, route := range list {
			if !route.privateRoute() && !route.IsTransportRoute() {
				continue
			}
			for _, listener := range settings.Settings.Listeners {
				if listener.ID != route.Policy.Listener {
					continue
				}
				protocol := "tcp"
				if listener.Protocol == "udp" {
					protocol = "udp"
				}
				current := host.PortBindings[strconv.Itoa(settings.Ports[listener.ID])+"/"+protocol]
				expected := map[string]bool{}
				for _, binding := range listener.Bindings {
					expected[binding.Address+"/"+strconv.Itoa(binding.Port)] = true
				}
				if len(current) != len(expected) {
					return errors.New("private listener publications changed outside the reviewed proxy settings")
				}
				for _, binding := range current {
					address, err := netip.ParseAddr(binding.HostIP)
					if err != nil || !expected[address.String()+"/"+binding.HostPort] {
						return errors.New("private listener binding does not match its reviewed address")
					}
				}
			}
		}
	}

	if !snapshot.Running {
		if err := e.Run(ctx, e.Root, []string{"docker", "start", "cove-ingress"}, nil, io.Discard); err != nil {
			return err
		}
	}
	var data []byte
	for attempt := 0; attempt < 20; attempt++ {
		data, err = e.proxyAPI(ctx, "cove-ingress", "/api/rawdata")
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if err != nil {
		return errors.New("private routes require readable proxy runtime configuration")
	}
	previous, err := e.loadIngress()
	if err != nil {
		return err
	}
	owned := map[string]ingressRouter{}
	var previousConfig dynamicIngress
	guards := privateRouteGuards(previous)
	for name, guard := range privateRouteGuards(routes) {
		guards[name] = guard
	}
	// ingress.json can already hold a candidate while the last generated file still serves its predecessor.
	file, err := os.Open(filepath.Join(e.Root, "ingress.traefik.yaml"))
	if err == nil {
		defer file.Close()
		if err := json.NewDecoder(file).Decode(&previousConfig); err != nil {
			return errors.New("previous private-host reservations are unreadable")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("previous private-host reservations are unavailable")
	}
	checkpoint, err := e.loadIngressTransition()
	if err != nil {
		return err
	}
	mergeIngressOwnership(&previousConfig, checkpoint)
	for name, router := range previousConfig.HTTP.Routers {
		owned[name] = router
		if strings.HasPrefix(name, "cove-route-deny-") {
			guards[name] = router
		}
	}
	if len(privateRouteGuards(routes)) > 0 {
		if err := validatePrivateLiveData(data, guards, owned); err != nil {
			return err
		}
	}
	return validateTransportLiveData(data, transportRouteReferences(routes), previousConfig)
}

func (e *Engine) validatePendingRouteListeners(settings IngressExposureSettings) error {
	e.mu.Lock()
	pending := slices.Clone(e.pending)
	e.mu.Unlock()
	for _, operation := range pending {
		record, err := e.read(operation.ID)
		if err != nil {
			return err
		}
		if record.Operation.State != "queued" && record.Operation.State != "running" {
			continue
		}
		if err := ValidateRouteListeners(record.Request.Routes, settings); err != nil {
			return err
		}
	}
	return nil
}

func (r Route) IsPublicHTTPRoute() bool {
	return !r.IsTransportRoute() && (r.Protocol == "" || r.Protocol == "http") && (r.Policy == nil || r.Policy.Visibility == "" || r.Policy.Visibility == "public")
}

func PublicHTTPRouteDomain(routes []Route, hostname string) bool {
	if strings.Contains(hostname, "*") {
		return false
	}
	found := false
	for _, route := range routes {
		if !route.IsTransportRoute() && RouteDomainsOverlap(route.Domain, hostname) {
			if !route.IsPublicHTTPRoute() {
				return false
			}
			found = true
		}
	}
	return found
}
