package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type trafficTarget struct {
	Service string `json:"service"`
	Weight  int    `json:"weight"`
}

type trafficState map[string][]trafficTarget

type ingressWeighted struct {
	Services []ingressWeightedService `json:"services"`
	Sticky   map[string]any           `json:"sticky,omitempty"`
}

type ingressWeightedService struct {
	Name   string `json:"name"`
	Weight int    `json:"weight"`
}

func (e *Engine) loadTraffic(workload string) (trafficState, error) {
	if !workloadIdentifier.MatchString(workload) {
		return nil, errors.New("invalid traffic workload")
	}
	state := trafficState{}
	data, err := os.ReadFile(filepath.Join(e.Root, "workloads", workload, "traffic.json"))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	for service, targets := range state {
		if !serviceIdentifier.MatchString(service) || len(targets) == 0 || len(targets) > 2 {
			return nil, errors.New("invalid traffic service")
		}
		total := 0
		for _, target := range targets {
			if !serviceIdentifier.MatchString(target.Service) || target.Weight < 0 || target.Weight > 100 {
				return nil, errors.New("invalid traffic target")
			}
			total += target.Weight
		}
		if total != 100 {
			return nil, errors.New("traffic weights must total 100")
		}
	}
	return state, nil
}

func (e *Engine) serviceNames(workload, service string) ([]string, error) {
	traffic, err := e.loadTraffic(workload)
	if err != nil {
		return nil, err
	}
	if targets := traffic[service]; len(targets) > 0 {
		names := []string{}
		for _, target := range targets {
			if target.Weight > 0 {
				names = append(names, target.Service)
			}
		}
		return names, nil
	}
	return []string{service}, nil
}

func (e *Engine) trafficContainers(ctx context.Context, workload, service string, runningOnly bool) ([]string, error) {
	names, err := e.serviceNames(workload, service)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	flag := "-aq"
	if runningOnly {
		flag = "-q"
	}
	for _, name := range names {
		output, err := e.output(ctx, "docker", "ps", flag, "--filter", "label=com.docker.compose.project=cove-"+workload, "--filter", "label=com.docker.compose.service="+name)
		if err != nil {
			return nil, err
		}
		ids = append(ids, strings.Fields(output)...)
	}
	return ids, nil
}

func trafficServiceName(traffic trafficState, actual string) string {
	for original, targets := range traffic {
		for _, target := range targets {
			if target.Service == actual {
				return original
			}
		}
	}
	return actual
}

func restoreServiceNames(config map[string]any, traffic trafficState) error {
	if len(traffic) == 0 {
		return nil
	}
	services, ok := config["services"].(map[string]any)
	if !ok {
		return errors.New("snapshot services are unavailable")
	}
	next := map[string]any{}
	for actual, raw := range services {
		name := trafficServiceName(traffic, actual)
		if next[name] != nil {
			return errors.New("snapshot has conflicting original service names")
		}
		service, ok := raw.(map[string]any)
		if !ok {
			return errors.New("invalid snapshot service")
		}
		if dependencies, ok := service["depends_on"].(map[string]any); ok {
			mapped := map[string]any{}
			for key, value := range dependencies {
				mapped[trafficServiceName(traffic, key)] = value
			}
			service["depends_on"] = mapped
		}
		next[name] = service
	}
	config["services"] = next
	return nil
}

func (e *Engine) ingressTraffic(ctx context.Context, routes map[string][]Route, out io.Writer) (string, error) {
	config := newIngressConfig()
	workloads := make([]string, 0, len(routes))
	for workload := range routes {
		workloads = append(workloads, workload)
	}
	sort.Strings(workloads)
	for _, workload := range workloads {
		suspension, err := e.readSuspension(workload)
		if err != nil {
			return "", err
		}
		idle, err := e.refreshIdleGateway(ctx, workload, routes[workload])
		if err != nil {
			return "", err
		}
		traffic, err := e.loadTraffic(workload)
		if err != nil {
			return "", err
		}
		for _, route := range routes[workload] {
			before := map[string]bool{}
			for name := range config.HTTP.Routers {
				before[name] = true
			}
			config.addRoute(workload, route)
			if route.Policy != nil && route.Policy.Maintenance != nil && route.Policy.Maintenance.Enabled {
				continue
			}
			if len(traffic) == 0 && suspension == nil && idle == nil {
				continue
			}
			name := ""
			for key := range config.HTTP.Routers {
				if !before[key] {
					name = key
					break
				}
			}
			if name == "" {
				return "", errors.New("traffic router was not generated")
			}
			router, base := config.HTTP.Routers[name], config.HTTP.Services[name]
			if suspension != nil && suspension.Phase != "awake" && (idle == nil || suspension.Options.Reason != "idle" || suspension.ManualStop) {
				sum := sha256.Sum256([]byte(suspension.Operation + suspension.Phase))
				key := name + "-paused-" + hex.EncodeToString(sum[:6])
				delete(config.HTTP.Services, name)
				config.HTTP.Services[key] = ingressService{LoadBalancer: &ingressLoadBalancer{Servers: []map[string]any{}}}
				router.Service = key
				config.HTTP.Routers[name] = router
				continue
			}
			if len(traffic) == 0 && idle == nil {
				continue
			}
			targets := []Route{{Service: route.Service, Port: route.Port}}
			if route.Policy != nil {
				for _, upstream := range route.Policy.Upstreams {
					targets = append(targets, Route{Service: upstream.Service, Port: upstream.Port})
				}
			}
			groups := []ingressWeightedService{}
			for _, target := range targets {
				selected := traffic[target.Service]
				if len(selected) == 0 {
					selected = []trafficTarget{{Service: target.Service, Weight: 100}}
				}
				for _, generation := range selected {
					if generation.Weight == 0 {
						continue
					}
					servers := []map[string]any{}
					if idle != nil {
						servers, err = idle.servers(generation.Service, target.Port)
						if err != nil {
							return "", err
						}
					} else {
						ids, err := e.output(ctx, "docker", "ps", "-q", "--filter", "label=com.docker.compose.project=cove-"+workload, "--filter", "label=com.docker.compose.service="+generation.Service)
						if err != nil {
							return "", err
						}
						for _, id := range strings.Fields(ids) {
							if !containerIdentity.MatchString(id) {
								return "", errors.New("invalid traffic container")
							}
							address, err := e.attachTrafficContainer(ctx, workload, id, out, "")
							if err != nil {
								return "", err
							}
							servers = append(servers, map[string]any{"url": "http://" + net.JoinHostPort(address, strconv.Itoa(target.Port))})
						}
					}
					data, _ := json.Marshal(servers)
					sum := sha256.Sum256(append(data, []byte(generation.Service+strconv.Itoa(generation.Weight))...))
					key := name + "-" + hex.EncodeToString(sum[:8])
					config.HTTP.Services[key] = ingressService{LoadBalancer: &ingressLoadBalancer{Servers: servers, Sticky: base.LoadBalancer.Sticky}}
					groups = append(groups, ingressWeightedService{Name: key, Weight: generation.Weight})
				}
			}
			data, _ := json.Marshal(groups)
			sum := sha256.Sum256(data)
			next := name + "-" + hex.EncodeToString(sum[:8])
			weighted := &ingressWeighted{Services: groups}
			if route.Policy != nil && route.Policy.Sticky {
				weighted.Sticky = map[string]any{"cookie": map[string]any{"name": "cove_generation_" + hex.EncodeToString(sum[:8]), "secure": true, "httpOnly": true, "sameSite": "lax"}}
			}
			config.HTTP.Services[next] = ingressService{Weighted: weighted}
			router.Service = next
			delete(config.HTTP.Services, name)
			delete(config.HTTP.Routers, name)
			config.HTTP.Routers[next] = router
		}
	}
	if err := e.applyRouteAuxiliary(ctx, routes, &config, out); err != nil {
		return "", err
	}
	if len(config.HTTP.Routers) == 0 {
		return "{}", nil
	}
	data, err := json.Marshal(config)
	return string(data), err
}

func (e *Engine) refreshTrafficIngress(ctx context.Context, workload string, out io.Writer) error {
	traffic, err := e.loadTraffic(workload)
	if err != nil || len(traffic) == 0 {
		return err
	}
	state, released, err := readCurrent(filepath.Join(e.Root, "workloads", workload, "current.json"))
	if err != nil {
		return err
	}
	if !released {
		return errors.New("traffic release is unavailable")
	}
	return e.applyIngress(ctx, workload, state.Routes, out)
}

func (e *Engine) attachTrafficContainer(ctx context.Context, workload, id string, out io.Writer, alias string) (string, error) {
	project, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, id)
	if err != nil {
		return "", err
	}
	if project != "cove-"+workload {
		return "", errors.New("traffic container belongs to another workload")
	}
	var inspected struct {
		Project string `json:"project"`
		Address string `json:"address"`
	}
	inspect := func() error {
		return e.imageJSON(ctx, nil, &inspected, "docker", "inspect", "--format", `{"project":{{json (index .Config.Labels "com.docker.compose.project")}},"address":{{json (index .NetworkSettings.Networks "cove-ingress").IPAddress}}}`, id)
	}
	// A missing attachment is not readable with the IP template until the network has been connected.
	attachment, err := e.output(ctx, "docker", "inspect", "--format", `{{with index .NetworkSettings.Networks "cove-ingress"}}{{.IPAddress}}{{end}}`, id)
	if err != nil {
		return "", err
	}
	if attachment == "" {
		args := []string{"docker", "network", "connect"}
		if alias != "" {
			args = append(args, "--alias", alias)
		}
		if err = e.Run(ctx, e.Root, append(args, "cove-ingress", id), nil, out); err != nil {
			return "", err
		}
	}
	if err = inspect(); err != nil {
		return "", err
	}
	if inspected.Project != "cove-"+workload || net.ParseIP(inspected.Address) == nil {
		return "", errors.New("traffic endpoint ownership or address unavailable")
	}
	return inspected.Address, nil
}
