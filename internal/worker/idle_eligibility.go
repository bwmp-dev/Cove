package worker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

func idleComposeEligibility(config map[string]any, routes []Route, completed map[string]bool) error {
	if err := idleAuthEligibility(routes); err != nil {
		return err
	}
	observed := map[string]bool{}
	for _, target := range routeTargets(routes) {
		observed[target.Service] = true
	}
	services, ok := config["services"].(map[string]any)
	if !ok || len(services) == 0 || len(services) > 32 {
		return errors.New("idle sleep requires one to 32 Compose services")
	}
	for name, raw := range services {
		service, ok := raw.(map[string]any)
		if !ok {
			return errors.New("invalid idle Compose service")
		}
		for _, key := range []string{"ports", "network_mode", "external_links", "links", "privileged", "devices", "pid", "ipc"} {
			if value := service[key]; value != nil && value != false {
				return fmt.Errorf("service %s uses %s, whose activity cannot be observed by managed HTTP routes", name, key)
			}
		}
		if !observed[name] && !completed[name] {
			return fmt.Errorf("service %s has no managed HTTP route; background services cannot sleep automatically", name)
		}
	}
	networks, _ := config["networks"].(map[string]any)
	for _, raw := range networks {
		network, _ := raw.(map[string]any)
		if network["external"] == true {
			return errors.New("idle sleep cannot observe external network traffic")
		}
	}
	return nil
}

func (e *Engine) idleEligibility(ctx context.Context, workload string) error {
	state, released, err := readCurrent(filepath.Join(e.Root, "workloads", workload, "current.json"))
	if err != nil {
		return err
	}
	if !released || len(state.Routes) == 0 {
		return errors.New("deploy a service with managed HTTP routes before enabling idle sleep")
	}
	if state.Private != nil || len(state.Networks) > 0 {
		return errors.New("private and shared-network traffic cannot be observed for idle sleep")
	}
	if _, pending, err := e.pendingRollout(workload); err != nil {
		return err
	} else if pending != nil && pending.Phase == "waiting" {
		return errors.New("promote or abort the staged rollout before enabling idle sleep")
	}
	links, err := e.loadLinkConfig()
	if err != nil {
		return err
	}
	for _, source := range links.Sources {
		if source.Workload == workload || source.TargetWorkload == workload {
			return errors.New("remove private connections before enabling idle sleep")
		}
	}
	for _, target := range links.Targets {
		if target.Workload == workload {
			return errors.New("remove private connections before enabling idle sleep")
		}
	}
	resolved := &limitedBuffer{Limit: 2 << 20}
	args := []string{"docker", "compose", "--project-name", "cove-" + workload, "--file", state.ComposePath, "config", "--format", "json"}
	env := []string{}
	for key, value := range state.Environment {
		env = append(env, key+"="+value)
	}
	if err = e.Run(ctx, filepath.Dir(state.ComposePath), args, env, resolved); err != nil {
		return errors.New("could not resolve the deployed Compose configuration")
	}
	var config map[string]any
	if err = decodeResolvedCompose(resolved.data.Bytes(), &config); err != nil {
		return err
	}
	traffic, err := e.loadTraffic(workload)
	if err != nil {
		return err
	}
	routes := []Route{}
	for _, target := range routeTargets(state.Routes) {
		names := traffic[target.Service]
		if len(names) == 0 {
			names = []trafficTarget{{Service: target.Service, Weight: 100}}
		}
		for _, name := range names {
			routes = append(routes, Route{Service: name.Service, Port: target.Port})
		}
	}
	containers, err := e.HealthContainers(ctx, workload)
	if err != nil {
		return err
	}
	completed := map[string]bool{}
	for _, container := range containers {
		if container.ExpectedCompleted && container.State == "exited" && container.ExitCode != nil && *container.ExitCode == 0 {
			completed[container.Service] = true
		}
	}
	actualCompleted := map[string]bool{}
	services, _ := config["services"].(map[string]any)
	for name := range services {
		actualCompleted[name] = completed[trafficServiceName(traffic, name)]
	}
	if err = idleComposeEligibility(config, routes, actualCompleted); err != nil {
		return err
	}
	if len(containers) == 0 || len(containers) > 128 {
		return errors.New("idle sleep requires deployed containers, with at most 128 replicas")
	}
	for _, container := range containers {
		if container.ExpectedCompleted && container.State == "exited" && container.ExitCode != nil && *container.ExitCode == 0 {
			continue
		}
		if container.State != "running" || container.Health != "healthy" {
			return errors.New("every running service must have a passing Docker healthcheck before idle sleep")
		}
	}
	return nil
}

func (e *Engine) validateIdleCandidate(ctx context.Context, r Request, dir string, args, env []string) error {
	policy, err := e.readIdle(r.Workload)
	if err != nil || policy == nil {
		return err
	}
	if r.Private != nil || len(r.Networks) > 0 {
		return errors.New("disable idle sleep before adding private access or shared networks")
	}
	resolved := &limitedBuffer{Limit: 2 << 20}
	if err = e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, resolved); err != nil {
		return errors.New("could not resolve candidate idle eligibility")
	}
	var config map[string]any
	if err = decodeResolvedCompose(resolved.data.Bytes(), &config); err != nil {
		return err
	}
	completed := map[string]bool{}
	services, _ := config["services"].(map[string]any)
	for _, raw := range services {
		service, _ := raw.(map[string]any)
		dependencies, _ := service["depends_on"].(map[string]any)
		for name, raw := range dependencies {
			dependency, _ := raw.(map[string]any)
			if dependency["condition"] == "service_completed_successfully" {
				completed[name] = true
			}
		}
	}
	return idleComposeEligibility(config, r.Routes, completed)
}
