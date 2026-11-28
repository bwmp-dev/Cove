package worker

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const routeAuxPlaceholder = "http://cove-route-helper.invalid:8080"

func routeAuxKey(route Route) string {
	data, _ := json.Marshal(route)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:16])
}

func activeRouteAux(route Route) bool {
	p := route.Policy
	return p != nil && (p.Maintenance != nil && p.Maintenance.Enabled || len(p.ErrorPages) > 0 || p.ForwardAuth != nil)
}

func (e *Engine) routeAuxTargets(ctx context.Context, workload string, auth *RouteForwardAuth, out io.Writer) ([]routeAuxTarget, error) {
	targets := []routeAuxTarget{}
	traffic, err := e.loadTraffic(workload)
	if err != nil {
		return nil, err
	}
	selected := traffic[auth.Service]
	if len(selected) == 0 {
		selected = []trafficTarget{{Service: auth.Service, Weight: 100}}
	}
	for _, generation := range selected {
		if generation.Weight == 0 {
			continue
		}
		text, err := e.output(ctx, "docker", "ps", "-q", "--filter", "label=com.docker.compose.project="+e.composeProject(workload), "--filter", "label=com.docker.compose.service="+generation.Service)
		if err != nil {
			return nil, err
		}
		ids := strings.Fields(text)
		for _, id := range ids {
			if !containerIdentity.MatchString(id) {
				return nil, errors.New("invalid authentication container identity")
			}
			alias := ""
			if len(traffic[auth.Service]) == 0 {
				alias = routeAlias(workload, auth.Service)
			}
			address, err := e.attachTrafficContainer(ctx, workload, id, out, alias)
			if err != nil {
				return nil, err
			}
			path := auth.Path
			if path == "" {
				path = "/"
			}
			targets = append(targets, routeAuxTarget{URL: "http://" + net.JoinHostPort(address, strconv.Itoa(auth.Port)) + path, Weight: max(1, generation.Weight*100/len(ids))})
		}
	}
	slices.SortFunc(targets, func(a, b routeAuxTarget) int { return strings.Compare(a.URL, b.URL) })
	return targets, nil
}

func routeAuxRoots() (string, error) {
	paths := []string{}
	if custom := os.Getenv("SSL_CERT_FILE"); custom != "" {
		paths = append(paths, custom)
	}
	paths = append(paths, "/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem")
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if x509.NewCertPool().AppendCertsFromPEM(data) {
			return string(data), nil
		}
	}
	return "", errors.New("a system CA bundle is required for public HTTPS forward-auth")
}

func (e *Engine) applyRouteAuxiliary(ctx context.Context, routes map[string][]Route, config *dynamicIngress, out io.Writer) error {
	aux := routeAuxConfig{Entries: map[string]routeAuxEntry{}}
	proxy, err := readOutboundConfig(e.Root)
	if err != nil {
		return err
	}
	aux.Proxy = proxy
	public := false
	for workload, list := range routes {
		for _, route := range list {
			if !activeRouteAux(route) {
				continue
			}
			entry := routeAuxEntry{Maintenance: route.Policy.Maintenance, ErrorPages: route.Policy.ErrorPages, Auth: route.Policy.ForwardAuth}
			if entry.Auth != nil {
				if entry.Auth.URL != "" {
					public = true
				} else {
					var err error
					entry.Targets, err = e.routeAuxTargets(ctx, workload, entry.Auth, out)
					if err != nil {
						return err
					}
				}
			}
			aux.Entries[routeAuxKey(route)] = entry
		}
	}
	if len(aux.Entries) == 0 {
		return nil
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "start", "cove-ingress"}, nil, out); err != nil {
		return err
	}
	peer, err := e.output(ctx, "docker", "inspect", "--format", `{{with index .NetworkSettings.Networks "cove-ingress"}}{{.IPAddress}}{{end}}`, "cove-ingress")
	if err != nil || net.ParseIP(peer) == nil {
		return errors.New("could not identify the managed proxy peer")
	}
	aux.Peer = peer
	if public {
		aux.CAPEM, err = routeAuxRoots()
		if err != nil {
			return err
		}
	}
	if _, err = newRouteAuxHandler(aux, nil); err != nil {
		return err
	}
	data, err := json.Marshal(aux)
	if err != nil {
		return err
	}
	if len(data) > 4*1024*1024 {
		return errors.New("route helper configuration exceeds the node's 4 MiB page budget")
	}
	image, err := e.linkGatewayImage(ctx)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(image + "\x00" + string(data)))
	digest := hex.EncodeToString(sum[:])
	name, err := e.ensureRouteAuxiliary(ctx, digest, image, data, out)
	if err != nil {
		return err
	}
	bindRouteAuxiliary(config, "http://"+name+":8080", digest, aux.Entries)
	return nil
}

func bindRouteAuxiliary(config *dynamicIngress, endpoint, digest string, entries map[string]routeAuxEntry) {
	for key, service := range config.HTTP.Services {
		if service.LoadBalancer == nil {
			continue
		}
		for _, server := range service.LoadBalancer.Servers {
			if text, ok := server["url"].(string); ok && strings.HasPrefix(text, routeAuxPlaceholder+"/") {
				server["url"] = endpoint + strings.TrimPrefix(text, routeAuxPlaceholder)
			}
		}
		config.HTTP.Services[key] = service
	}
	for key, middleware := range config.HTTP.Middlewares {
		if text, ok := middleware.ForwardAuth["address"].(string); ok && strings.HasPrefix(text, routeAuxPlaceholder+"/") {
			middleware.ForwardAuth["address"] = endpoint + strings.TrimPrefix(text, routeAuxPlaceholder)
		}
		config.HTTP.Middlewares[key] = middleware
	}
	// A changed helper endpoint must participate in live router confirmation before its predecessor is retired.
	keys := make([]string, 0, len(config.HTTP.Routers))
	for key := range config.HTTP.Routers {
		keys = append(keys, key)
	}
	for _, key := range keys {
		router := config.HTTP.Routers[key]
		for routeKey := range entries {
			if strings.HasPrefix(key, "cove-route-"+routeKey) {
				delete(config.HTTP.Routers, key)
				config.HTTP.Routers[key+"-aux-"+digest[:12]] = router
				break
			}
		}
	}
}

func (e *Engine) ensureRouteAuxiliary(ctx context.Context, digest, image string, data []byte, out io.Writer) (string, error) {
	owner, err := e.buildOwner()
	if err != nil {
		return "", err
	}
	name := "cove-route-aux-" + owner[:8] + "-" + digest[:16]
	dir := filepath.Join(e.Root, "route-aux", digest)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err = writeJSON(filepath.Join(dir, "config.json"), json.RawMessage(data)); err != nil {
		return "", err
	}
	id, err := e.output(ctx, "docker", "ps", "-aq", "--no-trunc", "--filter", "name=^/"+name+"$")
	if err != nil {
		return "", err
	}
	if id != "" {
		labels, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "cove.managed"}} {{index .Config.Labels "cove.route-aux-owner"}} {{index .Config.Labels "cove.route-aux-digest"}}`, id)
		if err != nil {
			return "", err
		}
		if labels != "route-aux "+owner+" "+digest {
			return "", errors.New("existing route helper has unexpected ownership")
		}
	} else {
		args := []string{"docker", "create", "--name", name, "--label", "cove.managed=route-aux", "--label", "cove.route-aux-owner=" + owner, "--label", "cove.route-aux-digest=" + digest, "--restart", "unless-stopped", "--network", "cove-ingress", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--pids-limit", "64", "--memory", "128m", "--log-driver", "json-file", "--log-opt", "max-size=1m", "--log-opt", "max-file=2", "--mount", "type=bind,source=" + dir + ",target=/config,readonly", image, "route-helper"}
		if err = e.Run(ctx, e.Root, args, nil, out); err != nil {
			return "", err
		}
	}
	if err = e.Run(ctx, e.Root, []string{"docker", "start", name}, nil, out); err != nil {
		return "", err
	}
	for attempt := 0; attempt < 20; attempt++ {
		if err = e.Run(ctx, e.Root, []string{"docker", "exec", name, "/cove-worker", "healthcheck", "http://127.0.0.1:8080/health"}, nil, io.Discard); err == nil {
			return name, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return "", errors.New("route helper did not become ready")
}

func (e *Engine) pruneRouteAuxiliary(ctx context.Context, config dynamicIngress, out io.Writer) error {
	root := filepath.Join(e.Root, "route-aux")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	owner, err := e.buildOwner()
	if err != nil {
		return err
	}
	retained := map[string]bool{}
	retain := func(text string) {
		endpoint, err := url.Parse(text)
		if err == nil {
			retained[endpoint.Hostname()] = true
		}
	}
	for _, service := range config.HTTP.Services {
		if service.LoadBalancer != nil {
			for _, server := range service.LoadBalancer.Servers {
				if text, ok := server["url"].(string); ok {
					retain(text)
				}
			}
		}
	}
	for _, middleware := range config.HTTP.Middlewares {
		if text, ok := middleware.ForwardAuth["address"].(string); ok {
			retain(text)
		}
	}
	ids, err := e.output(ctx, "docker", "ps", "-aq", "--no-trunc", "--filter", "label=cove.managed=route-aux", "--filter", "label=cove.route-aux-owner="+owner)
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, id := range strings.Fields(ids) {
		if !containerIdentity.MatchString(id) {
			return errors.New("invalid route helper identity")
		}
		text, err := e.output(ctx, "docker", "inspect", "--format", `{{.Name}} {{index .Config.Labels "cove.managed"}} {{index .Config.Labels "cove.route-aux-owner"}} {{index .Config.Labels "cove.route-aux-digest"}}`, id)
		if err != nil {
			return err
		}
		fields := strings.Fields(text)
		if len(fields) != 4 || fields[1] != "route-aux" || fields[2] != owner || len(fields[3]) != 64 {
			return errors.New("route helper ownership changed during cleanup")
		}
		if _, err = hex.DecodeString(fields[3]); err != nil {
			return err
		}
		if retained[strings.TrimPrefix(fields[0], "/")] {
			kept[fields[3]] = true
			continue
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "stop", "--time", "20", id}, nil, out); err != nil {
			return err
		}
		if err = e.Run(ctx, e.Root, []string{"docker", "rm", id}, nil, out); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || len(name) != 64 || kept[name] {
			continue
		}
		if _, err = hex.DecodeString(name); err != nil {
			continue
		}
		if err = os.RemoveAll(filepath.Join(root, name)); err != nil {
			return err
		}
	}
	return nil
}
