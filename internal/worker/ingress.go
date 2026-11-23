package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Route struct {
	Protocol string `json:"protocol,omitempty"`
	Domain   string `json:"domain"`
	Service  string `json:"service"`
	Port     int    `json:"port"`
	// Auth is an htpasswd bcrypt entry; when set the proxy requires it before forwarding.
	Auth   string       `json:"auth,omitempty"`
	Policy *RoutePolicy `json:"policy,omitempty"`
}

var routeAuth = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}:\$2[aby]\$1[0-4]\$[./A-Za-z0-9]{53}$`)

var domainLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func ValidateRoutes(routes []Route, kind string) error {
	if len(routes) > 32 || (kind == "job" && len(routes) > 0) {
		return errors.New("only services can have routes, with at most 32 routes")
	}
	seen := map[string]bool{}
	certificates := map[string]string{}
	for _, route := range routes {
		if route.protocol() != "http" {
			if err := validateTransportRoute(route); err != nil {
				return err
			}
			key := "listener/" + route.listener()
			if seen[key] {
				return errors.New("a dedicated listener can have only one route")
			}
			seen[key] = true
			continue
		}
		key := route.Domain + "/" + route.pathPrefix()
		if len(route.Domain) > 253 || !strings.Contains(route.Domain, ".") || seen[key] || !serviceIdentifier.MatchString(route.Service) || route.Port < 1 || route.Port > 65535 || (route.Auth != "" && !routeAuth.MatchString(route.Auth)) {
			return errors.New("invalid or duplicate route hostname, service or port")
		}
		base := strings.TrimPrefix(route.Domain, "*.")
		if !strings.Contains(base, ".") {
			return errors.New("route hostnames require a complete DNS name")
		}
		for _, label := range strings.Split(base, ".") {
			if !domainLabel.MatchString(label) {
				return errors.New("route hostnames must be lowercase DNS names, optionally beginning with *.")
			}
		}
		if strings.HasPrefix(route.Domain, "*.") && (route.Policy == nil || route.Policy.CertificateSource != "uploaded" || route.Policy.CertificateID == "") {
			return errors.New("wildcard routes require an assigned wildcard certificate; HTTP-01 cannot issue wildcard certificates")
		}
		if err := route.validatePolicy(); err != nil {
			return err
		}
		selection := "acme"
		if p := route.Policy; p != nil {
			if p.CertificateSource == "default" {
				selection = "default"
			} else if p.CertificateSource == "uploaded" {
				selection = "uploaded/" + p.CertificateID
			}
		}
		if previous := certificates[route.Domain]; previous != "" && previous != selection {
			return errors.New("all paths on a hostname must select the same certificate source and version")
		}
		certificates[route.Domain] = selection
		seen[key] = true
	}
	if err := validateRouteVisibilities(routes); err != nil {
		return err
	}
	for _, wildcard := range routes {
		if !strings.HasPrefix(wildcard.Domain, "*.") {
			continue
		}
		for _, exact := range routes {
			if !strings.HasPrefix(exact.Domain, "*.") && RouteDomainsOverlap(wildcard.Domain, exact.Domain) && (exact.Policy == nil || exact.Policy.CertificateSource != "uploaded") {
				return errors.New("exact hostnames covered by a managed wildcard must also select a stored certificate")
			}
		}
	}
	return nil
}

func routeAlias(workload, service string) string {
	sum := sha256.Sum256([]byte(workload + "/" + service))
	return "cove-" + hex.EncodeToString(sum[:16])
}

func ingressConfig(routes map[string][]Route) (string, error) {
	config := newIngressConfig()
	owners := map[string]string{}
	for workload, list := range routes {
		if err := ValidateRoutes(list, "service"); err != nil {
			return "", err
		}
		for _, route := range list {
			if route.IsTransportRoute() {
				config.addRoute(workload, route)
				continue
			}
			if owner := owners[route.Domain]; owner != "" && owner != workload {
				return "", errors.New("hostname already belongs to another workload")
			}
			owners[route.Domain] = workload
			config.addRoute(workload, route)
		}
	}
	for domain, owner := range owners {
		if _, base, ok := strings.Cut(domain, "."); ok && !strings.HasPrefix(domain, "*.") {
			if other := owners["*."+base]; other != "" && other != owner {
				return "", errors.New("wildcard hostname overlaps another workload")
			}
		}
	}
	if err := validateTransportOwners(routes); err != nil {
		return "", err
	}
	config.addPrivateDenials(routes)
	// Traefik rejects empty nested maps as standalone configuration elements.
	if config.empty() {
		return "{}", nil
	}
	data, err := json.Marshal(config)
	return string(data), err
}

type ingressRouter struct {
	Rule        string            `json:"rule"`
	Priority    int               `json:"priority,omitempty"`
	EntryPoints []string          `json:"entryPoints"`
	Middlewares []string          `json:"middlewares,omitempty"`
	Service     string            `json:"service"`
	TLS         map[string]string `json:"tls"`
}

type ingressBasicAuth struct {
	Users        []string `json:"users"`
	RemoveHeader bool     `json:"removeHeader"`
}
type ingressMiddleware struct {
	ForwardAuth   map[string]any    `json:"forwardAuth,omitempty"`
	Errors        map[string]any    `json:"errors,omitempty"`
	BasicAuth     *ingressBasicAuth `json:"basicAuth,omitempty"`
	Headers       map[string]any    `json:"headers,omitempty"`
	IPAllowList   map[string]any    `json:"ipAllowList,omitempty"`
	RedirectRegex map[string]any    `json:"redirectRegex,omitempty"`
}

type ingressService struct {
	LoadBalancer *ingressLoadBalancer `json:"loadBalancer,omitempty"`
	Weighted     *ingressWeighted     `json:"weighted,omitempty"`
}

type ingressLoadBalancer struct {
	ServersTransport string           `json:"serversTransport,omitempty"`
	Servers          []map[string]any `json:"servers"`
	Sticky           map[string]any   `json:"sticky,omitempty"`
}

type dynamicIngress struct {
	TLS  *ingressTLS             `json:"tls,omitempty"`
	HTTP ingressHTTP             `json:"http"`
	TCP  *ingressTransportConfig `json:"tcp,omitempty"`
	UDP  *ingressTransportConfig `json:"udp,omitempty"`
}

func newIngressConfig() dynamicIngress {
	var config dynamicIngress
	config.HTTP.Routers = map[string]ingressRouter{}
	config.HTTP.Services = map[string]ingressService{}
	return config
}

func (c *dynamicIngress) add(domain, upstream, auth string) {
	sum := sha256.Sum256([]byte(domain + "/" + upstream))
	name := "cove-route-" + hex.EncodeToString(sum[:16])
	c.addNamed(domain, upstream, auth, name)
}

func (c *dynamicIngress) addNamed(domain, upstream, auth, name string) {
	router := ingressRouter{Rule: routeHostRule(domain), Priority: routePriority(Route{Domain: domain}), EntryPoints: []string{"websecure"}, Service: name, TLS: map[string]string{"certResolver": "cove"}}
	if auth != "" {
		middleware := ingressMiddleware{}
		middleware.BasicAuth = &ingressBasicAuth{}
		middleware.BasicAuth.Users = []string{auth}
		middleware.BasicAuth.RemoveHeader = true
		if c.HTTP.Middlewares == nil {
			c.HTTP.Middlewares = map[string]ingressMiddleware{}
		}
		c.HTTP.Middlewares[name+"-auth"] = middleware
		router.Middlewares = []string{name + "-auth"}
	}
	c.HTTP.Routers[name] = router
	service := ingressService{LoadBalancer: &ingressLoadBalancer{}}
	service.LoadBalancer.Servers = []map[string]any{{"url": upstream}}
	c.HTTP.Services[name] = service
}

const ingressImage = "traefik:v3.7.19"

func (e *Engine) ingressArgs() ([]string, error) {
	args := []string{"--entrypoints.web.address=:80", "--entrypoints.web.http.redirections.entrypoint.to=websecure", "--entrypoints.web.http.redirections.entrypoint.scheme=https", "--entrypoints.websecure.address=:443", "--entrypoints.traefik.address=127.0.0.1:8080", "--api.insecure=true", "--api.dashboard=false", "--ping=true", "--providers.file.directory=/data/dynamic", "--providers.file.watch=true", "--certificatesresolvers.cove.acme.storage=/data/traefik-acme.json", "--certificatesresolvers.cove.acme.httpchallenge.entrypoint=web"}
	mode, err := e.ingressMode()
	if err != nil {
		return nil, err
	}
	if mode.TLSChallenge {
		args[len(args)-1] = "--certificatesresolvers.cove.acme.tlschallenge=true"
	}
	settings, err := e.ingressSettings()
	if err != nil {
		return nil, err
	}
	if settings.HTTPRedirectPriority != nil {
		args = append(args, "--entrypoints.web.http.redirections.entrypoint.priority="+strconv.Itoa(*settings.HTTPRedirectPriority))
	}
	if settings.CloudflareDNS {
		args = append(args, "--certificatesresolvers.cloudflare-dns.acme.storage=/data/cloudflare-acme.json", "--certificatesresolvers.cloudflare-dns.acme.dnschallenge.provider=cloudflare")
	}
	if e.ACMEDirectory != "" {
		u, err := url.Parse(e.ACMEDirectory)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(e.ACMEDirectory, "\r\n") {
			return nil, errors.New("ACME directory must be an HTTPS URL without credentials")
		}
		args = append(args, "--certificatesresolvers.cove.acme.caserver="+e.ACMEDirectory)
		if e.ACMERoot != "" {
			args = append(args, "--certificatesresolvers.cove.acme.cacertificates=/data/acme-root.pem")
		}
		if settings.CloudflareDNS {
			args = append(args, "--certificatesresolvers.cloudflare-dns.acme.caserver="+e.ACMEDirectory)
			if e.ACMERoot != "" {
				args = append(args, "--certificatesresolvers.cloudflare-dns.acme.cacertificates=/data/acme-root.pem")
			}
		}
	} else if e.ACMERoot != "" {
		return nil, errors.New("ACME root requires a custom ACME directory")
	}
	return args, nil
}

func (e *Engine) output(ctx context.Context, args ...string) (string, error) {
	b := &limitedBuffer{Limit: 2 * 1024 * 1024}
	err := e.Run(ctx, e.Root, args, nil, b)
	return strings.TrimSpace(b.data.String()), err
}

func (e *Engine) ensureIngress(ctx context.Context, out io.Writer) error {
	if err := e.ingressExposureReady(); err != nil {
		return err
	}
	args, err := e.ingressArgs()
	if err != nil {
		return err
	}
	for _, resource := range []string{"network", "volume"} {
		label, err := e.output(ctx, "docker", resource, "inspect", "--format", `{{index .Labels "cove.managed"}}`, "cove-ingress")
		if err != nil {
			if err = e.Run(ctx, e.Root, []string{"docker", resource, "create", "--label", "cove.managed=ingress", "cove-ingress"}, nil, out); err != nil {
				return err
			}
		} else if label != "ingress" {
			return errors.New("existing cove-ingress resource is not managed by Cove")
		}
	}
	id, err := e.output(ctx, "docker", "ps", "-aq", "--filter", "name=^/cove-ingress$")
	if err != nil {
		return err
	}
	if id == "" {
		state, err := e.loadIngressExposures()
		if err != nil {
			return err
		}
		client := e.ingressDockerClient()
		restored, restoreErr := restoreSavedIngressProxy(ctx, client, state)
		client.CloseIdleConnections()
		if restoreErr != nil {
			return restoreErr
		}
		if restored {
			return nil
		}
		mode, err := e.ingressMode()
		if err != nil {
			return err
		}
		create := []string{"docker", "create", "--name", "cove-ingress", "--label", "cove.managed=ingress", "--restart", "unless-stopped", "--network", "cove-ingress", "--security-opt", "no-new-privileges:true"}
		if !mode.Staged {
			create = append(create, "-p", "80:80", "-p", "443:443")
		}
		settings, err := e.ingressSettings()
		if err != nil {
			return err
		}
		if settings.CloudflareDNS {
			create = append(create, "-e", "CF_DNS_API_TOKEN_FILE=/data/cloudflare-dns-token")
		}
		create = append(create, "-v", "cove-ingress:/data", "--entrypoint", "sh", ingressImage, "-ec", "mkdir -p /data/dynamic; exec traefik \"$@\"", "traefik")
		return e.Run(ctx, e.Root, append(create, args...), nil, out)
	}
	label, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "cove.managed"}}`, "cove-ingress")
	if err != nil {
		return err
	}
	if label != "ingress" {
		return errors.New("existing cove-ingress container is not managed by Cove")
	}
	image, err := e.output(ctx, "docker", "inspect", "--format", `{{.Config.Image}}`, "cove-ingress")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(image, "traefik:") {
		proxy, err := e.inspectProxy(ctx)
		if err != nil || proxy.Image != image {
			return errors.New("existing ingress needs migration to Traefik before deploying routes")
		}
	}
	return nil
}

func (e *Engine) loadIngress() (map[string][]Route, error) {
	routes := map[string][]Route{}
	data, err := os.ReadFile(filepath.Join(e.Root, "ingress.json"))
	if os.IsNotExist(err) {
		return routes, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &routes); err != nil {
		return nil, err
	}
	return routes, nil
}

func (e *Engine) syncIngress(ctx context.Context, routes map[string][]Route, out io.Writer) error {
	if err := e.restrictCloudflareOrigins(ctx, routes); err != nil {
		return err
	}
	if err := e.ingressExposureReady(); err != nil {
		return err
	}
	if _, err := ingressConfig(routes); err != nil {
		return err
	}
	if err := e.ensureIngress(ctx, out); err != nil {
		return err
	}
	if err := e.validateManagedRouteIngress(ctx, routes); err != nil {
		return err
	}
	config, err := e.ingressTraffic(ctx, routes, out)
	if err != nil {
		return err
	}
	config, err = e.migrationIngressConfig(config)
	if err != nil {
		return err
	}
	panel, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "cove.panel.domain"}}`, "cove-ingress")
	if err != nil {
		return err
	}
	if panel != "" && panel != "<no value>" {
		if err = ValidateRoutes([]Route{{Domain: panel, Service: "cove", Port: 8080}}, "service"); err != nil {
			return err
		}
		for _, list := range routes {
			for _, route := range list {
				if RouteDomainsOverlap(route.Domain, panel) {
					return errors.New("the dashboard hostname is reserved")
				}
			}
		}
	}
	if e.ACMERoot != "" {
		if err = e.Run(ctx, e.Root, []string{"docker", "cp", e.ACMERoot, "cove-ingress:/data/acme-root.pem"}, nil, out); err != nil {
			return err
		}
	}
	for workload, list := range routes {
		suspension, err := e.readSuspension(workload)
		if err != nil {
			return err
		}
		if suspension != nil && suspension.Phase != "awake" {
			continue
		}
		idle, err := e.readIdle(workload)
		if err != nil {
			return err
		}
		if idle != nil {
			continue
		}
		for _, route := range routeTargets(list) {
			selected, err := e.trafficContainers(ctx, workload, route.Service, false)
			if err != nil {
				return err
			}
			ids := strings.Join(selected, " ")
			if ids == "" {
				continue
			}
			alias := routeAlias(workload, route.Service)
			for _, id := range strings.Fields(ids) {
				// Connecting a network neither restarts a running container nor starts a stopped one.
				attachment, err := e.output(ctx, "docker", "inspect", "--format", `{{with index .NetworkSettings.Networks "cove-ingress"}}connected{{range .Aliases}} {{.}}{{end}}{{end}}`, id)
				if err != nil {
					return err
				}
				fields := strings.Fields(attachment)
				if len(fields) == 0 {
					if err = e.Run(ctx, e.Root, []string{"docker", "network", "connect", "--alias", alias, "cove-ingress", id}, nil, out); err != nil {
						return err
					}
				} else if !slices.Contains(fields[1:], alias) {
					traffic, err := e.loadTraffic(workload)
					if err != nil {
						return err
					}
					if len(traffic[route.Service]) > 0 {
						continue
					}
					return fmt.Errorf("service %q joins cove-ingress without Cove's routing alias; remove that network from its Compose file", route.Service)
				}
			}
		}
	}
	confirmTransports, err := e.needsTransportConfirmation(routes)
	if err != nil {
		return err
	}
	var desired dynamicIngress
	if err = json.Unmarshal([]byte(config), &desired); err != nil {
		return err
	}
	if err = e.retireUDPIngress(ctx, desired, out); err != nil {
		return err
	}
	if err = e.publishIngress(ctx, config, out); err != nil {
		return err
	}
	var certificateErr error
	for attempt := 0; attempt < 30; attempt++ {
		current, readErr := e.output(ctx, "docker", "exec", "cove-ingress", "wget", "-qO-", "http://127.0.0.1:8080/api/http/routers")
		if readErr == nil && ingressAccepted(current, desired) {
			certificateErr = e.confirmTransportIngress(ctx, desired, confirmTransports)
			if certificateErr == nil {
				certificateErr = e.validateManagedRouteIngress(ctx, routes)
			}
			if certificateErr == nil {
				certificateErr = e.confirmRouteCertificates(ctx, routes)
			}
			if certificateErr == nil {
				if err := e.clearIngressTransition(); err != nil {
					return err
				}
				return e.pruneRouteAuxiliary(ctx, desired, out)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if certificateErr != nil {
		return certificateErr
	}
	return errors.New("ingress did not accept configuration; check ports 80/443 and ingress logs")
}

func (e *Engine) applyIngress(ctx context.Context, workload string, list []Route, out io.Writer) error {
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	if err := e.ingressExposureReady(); err != nil {
		return err
	}
	suspension, err := e.readSuspension(workload)
	if err != nil {
		return err
	}
	for _, route := range routeTargets(list) {
		if suspension != nil && suspension.Phase != "awake" {
			continue
		}
		ids, err := e.trafficContainers(ctx, workload, route.Service, false)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return fmt.Errorf("route service %q has no container; check the service name against the deployed Compose file", route.Service)
		}
	}
	previous, err := e.loadIngress()
	if err != nil {
		return err
	}
	if len(previous) == 0 && len(list) == 0 {
		return nil
	}
	if err = e.customHostConflict(previous[workload], list); err != nil {
		return err
	}
	desired := make(map[string][]Route, len(previous)+1)
	for id, routes := range previous {
		desired[id] = routes
	}
	if len(list) == 0 {
		delete(desired, workload)
	} else {
		desired[workload] = list
	}
	if _, err = ingressConfig(desired); err != nil {
		return err
	}
	if err = e.writeManagedIngress(desired); err != nil {
		// A failed write may still have replaced the file, which the next worker start would apply.
		if restoreErr := e.writeManagedIngress(previous); restoreErr != nil {
			return fmt.Errorf("%w: %w", errIngressUnrestored, errors.Join(err, restoreErr))
		}
		return err
	}
	if err = e.syncIngress(ctx, desired, out); err != nil {
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		if saveErr := e.writeManagedIngress(previous); saveErr != nil {
			return fmt.Errorf("%w: %w", errIngressUnrestored, errors.Join(err, saveErr))
		}
		if restoreErr := e.syncIngress(recovery, previous, out); restoreErr != nil {
			return fmt.Errorf("%w: %w", errIngressUnrestored, errors.Join(err, restoreErr))
		}
		return err
	}
	return nil
}

// errIngressUnrestored means a failed apply could not confirm the previous proxy configuration,
// so the proxy may still serve the routes that were being applied.
var errIngressUnrestored = errors.New("the previous proxy configuration could not be restored")

// errRoutesUnresolved marks a route operation whose outcome is not yet known: the proxy may serve
// either the previous or the requested routes. The operation stays running until recovery resolves it.
var errRoutesUnresolved = errors.New("route change is unresolved")

var errRoutesCancelled = errors.New("route change cancelled")

// updateRoutes changes only the managed proxy, so application containers keep their identity and
// run state. The previous routes are journaled before the proxy changes, and the change commits when
// the journal records Committed after the proxy and current.json hold the new routes. Failure or
// cancellation before the commit restores the previous routes; after it the change is final.
func (e *Engine) updateRoutes(ctx context.Context, r Request, dir string, out io.Writer) error {
	if err := e.validateIdleRoutes(ctx, r.Workload, r.Routes); err != nil {
		return err
	}
	path := filepath.Join(dir, "current.json")
	current, released, err := readCurrent(path)
	if err != nil {
		return err
	}
	if current.Private != nil && len(r.Routes) > 0 {
		return errors.New("deploy without private networking before adding public routes")
	}
	// Only this workload's operations change its entry, and they run one at a time.
	owned, err := e.loadIngress()
	if err != nil {
		return err
	}
	previous := owned[r.Workload]
	if !released && len(previous) == 0 {
		fmt.Fprintln(out, "No release is deployed on this node yet, so the proxy was not changed. The routes apply on the first deploy.")
		return nil
	}
	if err = e.updateRouteJournal(r.ID, func(change *routeChange) { *change = routeChange{Previous: previous} }); err != nil {
		return err
	}
	if err = e.applyIngress(ctx, r.Workload, r.Routes, out); err != nil {
		if errors.Is(err, errIngressUnrestored) {
			return fmt.Errorf("%w: %w", errRoutesUnresolved, err)
		}
		// The proxy kept or was restored to the previous routes, and current.json is unchanged.
		return err
	}
	if released {
		current.Routes = r.Routes
		if err = writeJSON(path, current); err != nil {
			return e.restorePrevious(ctx, r.Workload, previous, err, out)
		}
	}
	err = e.updateRouteJournal(r.ID, func(change *routeChange) { change.Committed = true })
	if errors.Is(err, errRoutesCancelled) {
		return e.restorePrevious(ctx, r.Workload, previous, err, out)
	}
	if err != nil {
		// The commit may have reached the disk, so only recovery reading the journal can decide.
		return fmt.Errorf("%w: %w", errRoutesUnresolved, err)
	}
	fmt.Fprintf(out, "Applied %d route(s) to the managed proxy. Application containers were not restarted or started.\n", len(r.Routes))
	return nil
}

// updateRouteJournal changes the journaled route change unless cancellation was requested first.
// Holding the engine lock orders it against Cancel, so a commit and a cancellation cannot both win.
func (e *Engine) updateRouteJournal(id string, update func(*routeChange)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err := e.read(id)
	if err != nil {
		return err
	}
	if j.CancelRequested {
		return errRoutesCancelled
	}
	if j.RouteChange == nil {
		j.RouteChange = &routeChange{}
	}
	update(j.RouteChange)
	return writeJSON(e.path(id), j)
}

// restorePrevious returns cause after restoring the routes from before a change, or an unresolved
// error when the proxy cannot be confirmed to serve the previous routes again.
func (e *Engine) restorePrevious(ctx context.Context, workload string, previous []Route, cause error, out io.Writer) error {
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	if err := e.restoreRoutes(recovery, workload, previous, out); err != nil {
		return fmt.Errorf("%w: %w", errRoutesUnresolved, errors.Join(cause, err))
	}
	return cause
}

// restoreRoutes durably returns a workload's routes in ingress.json and current.json to previous
// and then syncs the proxy to them. Repeating it is harmless.
func (e *Engine) restoreRoutes(ctx context.Context, workload string, previous []Route, out io.Writer) error {
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	if err := e.ingressExposureReady(); err != nil {
		return err
	}
	owned, err := e.loadIngress()
	if err != nil {
		return err
	}
	if len(previous) == 0 {
		delete(owned, workload)
	} else {
		owned[workload] = previous
	}
	if err = e.writeManagedIngress(owned); err != nil {
		return err
	}
	path := filepath.Join(e.Root, "workloads", workload, "current.json")
	current, released, err := readCurrent(path)
	if err != nil {
		return err
	}
	if released && !RoutesEqual(current.Routes, previous) {
		current.Routes = previous
		if err = writeJSON(path, current); err != nil {
			return err
		}
	}
	if err = e.syncIngress(ctx, owned, out); err != nil {
		return err
	}
	fmt.Fprintln(out, "Restored the previous routes on the managed proxy.")
	return nil
}

// recoverRoutes settles a route operation left running without an executor, retrying later when the
// proxy cannot be confirmed. state is reported when the previous routes are restored.
func (e *Engine) recoverRoutes(ctx context.Context, id, state string) {
	err := e.settleRoutes(ctx, id, state)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		delete(e.recovering, id)
		return
	}
	fmt.Fprintln(os.Stderr, "Route recovery for operation", id, "failed; retrying:", err)
	recovery := e.recovering[id]
	recovery.running = false
	recovery.next = time.Now().Add(routeRecoveryRetry)
	e.recovering[id] = recovery
}

var routeRecoveryRetry = 10 * time.Second

func (e *Engine) settleRoutes(ctx context.Context, id, state string) error {
	e.mu.Lock()
	j, err := e.read(id)
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if j.SuspensionChange != nil || j.IdleChange != nil {
		return e.settleSuspension(ctx, id)
	}
	if j.JobRun != nil {
		return e.settleJob(ctx, id, state)
	}
	if j.RolloutChange != nil {
		return e.settleRollout(ctx, id, state)
	}
	if j.Operation.State != "running" || j.RouteChange == nil {
		return nil
	}
	if !j.RouteChange.Committed {
		file, err := os.OpenFile(filepath.Join(e.Root, "operations", id+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		log := &boundedLog{file: file, remaining: 4 * 1024 * 1024}
		if info, statErr := file.Stat(); statErr == nil {
			log.remaining -= info.Size()
		}
		fmt.Fprintln(log, "The route change did not commit. Restoring the previous routes.")
		err = e.restoreRoutes(ctx, j.Request.Workload, j.RouteChange.Previous, log)
		if err != nil {
			fmt.Fprintln(log, "Could not restore the previous routes yet; the worker retries automatically:", err)
		}
		if flushErr := log.flush(true); err == nil {
			err = flushErr
		}
		if err != nil {
			return err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// Read again for a cancellation that arrived during the restore.
	if j, err = e.read(id); err != nil || j.Operation.State != "running" {
		return err
	}
	now := time.Now().UTC()
	j.Operation.FinishedAt = &now
	if j.RouteChange.Committed {
		j.Operation.State, j.Operation.Error = "succeeded", ""
	} else {
		j.Operation.State = state
		if j.CancelRequested {
			j.Operation.State = "cancelled"
		}
		j.Operation.Error = routeOutcome(j.Operation.State)
	}
	return writeJSON(e.path(id), j)
}

const routeRestoring = "The route change did not complete. Restoring the previous routes; the worker retries until the proxy confirms them."

func routeOutcome(state string) string {
	switch state {
	case "cancelled":
		return "Route change cancelled; the previous routes are still applied."
	case "interrupted":
		return "Route change interrupted; the previous routes are still applied."
	}
	return "Route change failed; the previous routes are still applied. Inspect the operation log."
}

// busy reports whether the workload has an executing operation or an unsettled route change.
func (e *Engine) busy(workload string) bool {
	if e.active[workload] {
		return true
	}
	for _, recovery := range e.recovering {
		if recovery.workload == workload {
			return true
		}
	}
	return false
}

func readCurrent(path string) (deployedState, bool, error) {
	var current deployedState
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return current, false, nil
	}
	if err != nil {
		return current, false, err
	}
	return current, true, json.Unmarshal(data, &current)
}

func ingressAccepted(data string, desired dynamicIngress) bool {
	var routers []struct {
		Name        string
		Status      string
		Rule        string
		Service     string
		Middlewares []string
		EntryPoints []string
		Priority    int
	}
	if json.Unmarshal([]byte(data), &routers) != nil {
		return false
	}
	found := 0
	for _, router := range routers {
		if !strings.HasPrefix(router.Name, "cove-route-") {
			continue
		}
		name := strings.TrimSuffix(router.Name, "@file")
		want, ok := desired.HTTP.Routers[name]
		if !ok || router.Status != "enabled" || router.Rule != want.Rule || strings.TrimSuffix(router.Service, "@file") != want.Service || len(router.Middlewares) != len(want.Middlewares) {
			return false
		}
		if !slices.Equal(router.EntryPoints, want.EntryPoints) {
			return false
		}
		if want.Priority != 0 && router.Priority != want.Priority {
			return false
		}
		// A protected route counts as applied only once the proxy enforces its authentication.
		for i, middleware := range want.Middlewares {
			if strings.TrimSuffix(router.Middlewares[i], "@file") != middleware {
				return false
			}
		}
		found++
	}
	return found == len(desired.HTTP.Routers)
}

func (e *Engine) recoverIngressDecisions() error {
	e.mu.Lock()
	ids := make([]string, 0, len(e.recovering))
	for id := range e.recovering {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	routes, err := e.loadIngress()
	if err != nil {
		return err
	}
	changed := false
	for _, id := range ids {
		journal, err := e.read(id)
		if err != nil {
			return err
		}
		if journal.RouteChange == nil {
			continue
		}
		desired := journal.Request.Routes
		if !journal.RouteChange.Committed {
			desired = journal.RouteChange.Previous
		}
		if RoutesEqual(routes[journal.Request.Workload], desired) {
			continue
		}
		if len(desired) == 0 {
			delete(routes, journal.Request.Workload)
		} else {
			routes[journal.Request.Workload] = desired
		}
		changed = true
	}
	if !changed {
		return nil
	}
	// Decide all interrupted route changes before any startup sync can expose an uncommitted protection removal.
	return e.writeManagedIngress(routes)
}
