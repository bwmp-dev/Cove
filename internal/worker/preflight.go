package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type PreflightCheck struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Message string `json:"message"`
}
type PreflightReport struct {
	Checks   []PreflightCheck `json:"checks"`
	Revision string           `json:"revision,omitempty"`
}

func (e *Engine) Preflight(ctx context.Context, r Request) (report PreflightReport) {
	report = PreflightReport{Checks: []PreflightCheck{}}
	add := func(name, state, message string) {
		report.Checks = append(report.Checks, PreflightCheck{name, state, message})
	}
	if !e.preflightMu.TryLock() {
		add("Preflight", "error", "Another inspection is running on this worker. Try again when it completes.")
		return report
	}
	defer e.preflightMu.Unlock()
	if r.Action != "deploy" || ValidateRequest(r) != nil {
		add("Configuration", "error", "Invalid deployment configuration.")
		return report
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp(e.Root, "preflight-")
	if err != nil {
		add("Storage", "error", "Cannot create a private inspection directory.")
		return report
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			add("Cleanup", "warning", "Temporary inspection files could not be removed; inspect worker storage.")
		}
	}()
	if metrics := inspectMetrics(ctx, e.Root); metrics != nil {
		if metrics.DiskAvailable < 100*1024*1024 {
			add("Disk", "error", "Less than 100 MiB is available on the worker data filesystem.")
		} else if metrics.DiskAvailable < 1024*1024*1024 {
			add("Disk", "warning", "Less than 1 GiB is available; builds and backups may require more.")
		} else {
			add("Disk", "pass", "Worker data filesystem has at least 1 GiB available; image and build sizes are not predicted.")
		}
	} else {
		add("Disk", "warning", "Disk usage could not be measured.")
	}
	if err = e.Run(ctx, dir, []string{"docker", "info", "--format", "{{.ServerVersion}}"}, nil, io.Discard); err != nil {
		add("Docker", "error", "Docker is unavailable.")
		return report
	}
	add("Docker", "pass", "Docker is reachable.")
	composePath := filepath.Join(dir, "compose.yaml")
	if r.Source != nil {
		composePath, report.Revision, err = e.checkout(ctx, r, dir, io.Discard)
		if err != nil {
			add("Git source", "error", "Cannot fetch the selected revision or locate its Compose file.")
			return report
		}
		add("Git source", "pass", "Repository access and Compose path verified.")
	} else if err = os.WriteFile(composePath, []byte(r.Compose), 0600); err != nil {
		add("Compose", "error", "Cannot stage Compose configuration.")
		return report
	}
	if r.Preview {
		composePath, err = e.preparePreview(r, dir, composePath)
		if err != nil {
			add("Preview isolation", "error", err.Error())
			return report
		}
	}
	env := []string{}
	if r.OutboundProxy != nil {
		env = append(env, r.OutboundProxy.Env()...)
		if r.OutboundProxy.HTTP != "" || r.OutboundProxy.HTTPS != "" {
			add("Docker daemon proxy", "warning", "Registry pulls run in dockerd. Configure its http-proxy, https-proxy and no-proxy host settings explicitly; request proxy variables do not configure dockerd. Use scripts/configure-docker-proxy.py --apply only during an operator-approved maintenance window.")
		}
	}
	for key, value := range r.Environment {
		env = append(env, key+"="+value)
	}
	if r.Registry != nil {
		config, e := registryConfig(dir, r.Registry)
		if e != nil {
			add("Registry", "error", "Cannot prepare registry credentials.")
			return report
		}
		env = append(env, "DOCKER_CONFIG="+config)
	}
	args := []string{"docker", "compose", "--project-name", "cove-" + r.Workload, "--file", composePath, "config", "--format", "json"}
	resolved := &limitedBuffer{Limit: 2 * 1024 * 1024}
	if err = e.Run(ctx, dir, args, env, resolved); err != nil {
		add("Compose", "error", "Compose validation failed. Check required variables, service definitions, and file paths.")
		return report
	}
	var config map[string]any
	if json.Unmarshal(resolved.data.Bytes(), &config) != nil {
		add("Compose", "error", "Compose reported warnings or could not produce a complete configuration. Check missing variables.")
		return report
	}
	if err = applyLimits(config, r.Limits); err != nil {
		add("Resource limits", "error", "Limits reference invalid or missing services.")
		return report
	}
	if err = validateFileSecretTargets(config, r); err != nil {
		add("File secrets", "error", err.Error())
		return report
	}
	if len(r.FileSecrets) > 0 {
		add("File secrets", "pass", "Source variables and selected runtime/build services exist; no secret files were mounted.")
	}
	if r.GuardedDeploy != nil && *r.GuardedDeploy {
		if _, err = guardedService(config); err != nil {
			add("Deployment safety", "error", err.Error())
			return report
		}
		add("Deployment safety", "warning", "Compose meets the stateless guard requirements. Image-declared volumes and candidate health are checked during deployment.")
	}
	services, _ := config["services"].(map[string]any)
	if len(services) == 0 {
		add("Compose", "error", "No services are configured.")
		return report
	}
	add("Compose", "pass", "Compose configuration and configured service limits are valid.")
	names := []string{}
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	ports := map[string]bool{}
	for _, name := range names {
		if ctx.Err() != nil {
			add("Timeout", "error", "Inspection timed out before all services could be checked.")
			break
		}
		service, _ := services[name].(map[string]any)
		if service["build"] != nil {
			add("Image: "+name, "warning", "This image must be built. Build success is not established by preflight.")
		} else if image, ok := service["image"].(string); ok {
			if e.Run(ctx, dir, []string{"docker", "manifest", "inspect", "--", image}, env, io.Discard) != nil {
				add("Image: "+name, "error", "Image manifest is unavailable. Check the tag, registry credentials, and registry connectivity.")
			} else {
				add("Image: "+name, "pass", "Image manifest is accessible.")
			}
		}
		if bindings, ok := service["ports"].([]any); ok {
			for _, binding := range bindings {
				port, _ := binding.(map[string]any)
				published, _ := port["published"].(string)
				if published != "" && published != "0" {
					protocol, _ := port["protocol"].(string)
					host, _ := port["host_ip"].(string)
					if host == "" || host == "0.0.0.0" || host == "::" {
						add("Exposure: "+name, "warning", "This service publishes a host port on all interfaces. Bind to loopback or a private address unless public access is intended.")
					}
					key := host + ":" + protocol + ":" + published
					if ports[key] {
						add("Ports: "+name, "error", "Multiple services publish the same host port; review their bindings.")
					}
					ports[key] = true
				}
			}
		}
		if mounts, ok := service["volumes"].([]any); ok {
			for _, mount := range mounts {
				volume, _ := mount.(map[string]any)
				if volume["type"] == "bind" {
					source, _ := volume["source"].(string)
					if _, err = os.Stat(source); err != nil {
						add("Mounts: "+name, "warning", "A bind source is missing or inaccessible on this worker. Verify it before deploying.")
					}
				}
			}
		}
	}
	if len(ports) > 0 {
		add("Host ports", "warning", "Host ports are published. Existing Docker and non-Docker listeners can still conflict at deployment time.")
	}
	add("Runtime", "warning", "Preflight does not start containers, test application health, or guarantee that data migrations will succeed.")
	return report
}
