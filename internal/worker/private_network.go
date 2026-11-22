package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type PrivateNetwork struct {
	Service  string `json:"service"`
	Hostname string `json:"hostname"`
	AuthKey  string `json:"auth_key,omitempty"`
	Port     int    `json:"port"`
}

func ValidatePrivateNetwork(p *PrivateNetwork) error {
	if p == nil {
		return nil
	}
	if !serviceIdentifier.MatchString(p.Service) || !domainLabel.MatchString(p.Hostname) || len(p.Hostname) > 63 || p.Port < 1 || p.Port > 65535 {
		return errors.New("valid private service, hostname and port required")
	}
	if !strings.HasPrefix(p.AuthKey, "tskey-auth-") || len(p.AuthKey) > 4096 || strings.ContainsAny(p.AuthKey, "\r\n\x00") {
		return errors.New("a Tailscale auth key is required")
	}
	return nil
}

func addPrivateNetwork(config map[string]any, p *PrivateNetwork, servePath string) error {
	services, ok := config["services"].(map[string]any)
	if !ok {
		return errors.New("Compose has no services")
	}
	app, ok := services[p.Service].(map[string]any)
	if !ok {
		return errors.New("private service does not exist")
	}
	const sidecar = "cove-tailscale"
	if _, exists := services[sidecar]; exists {
		return errors.New("Compose uses the reserved cove-tailscale service name")
	}
	for _, field := range []string{"hostname", "domainname", "dns", "dns_search", "dns_opt", "extra_hosts", "mac_address"} {
		if _, exists := app[field]; exists {
			return fmt.Errorf("remove %s from the private service before sharing the Tailscale network namespace", field)
		}
	}
	for _, value := range services {
		service, ok := value.(map[string]any)
		if !ok {
			return errors.New("invalid Compose service")
		}
		if ports, ok := service["ports"].([]any); ok && len(ports) > 0 {
			return errors.New("private resources cannot publish host ports")
		}
		if _, exists := service["network_mode"]; exists {
			return errors.New("private resources cannot specify network_mode")
		}
		if labels, ok := service["labels"].(map[string]any); ok {
			for key := range labels {
				if strings.HasPrefix(key, "traefik.") || strings.HasPrefix(key, "caddy") {
					return errors.New("remove public proxy labels before enabling private access")
				}
			}
		}
	}
	networks, ok := app["networks"].(map[string]any)
	if !ok || len(networks) == 0 {
		return errors.New("could not resolve private service networks")
	}
	for key, value := range networks {
		network, ok := value.(map[string]any)
		if !ok {
			network = map[string]any{}
			networks[key] = network
		}
		aliases, _ := network["aliases"].([]any)
		network["aliases"] = append(aliases, p.Service)
	}
	volumes, ok := config["volumes"].(map[string]any)
	if !ok {
		volumes = map[string]any{}
		config["volumes"] = volumes
	}
	if _, exists := volumes["cove-tailscale-state"]; exists {
		return errors.New("Compose uses the reserved Tailscale volume name")
	}
	volumes["cove-tailscale-state"] = map[string]any{}
	services[sidecar] = map[string]any{
		"healthcheck": map[string]any{"test": []string{"CMD-SHELL", "tailscale status --json | grep -q '\"BackendState\": \"Running\"'"}, "interval": "5s", "timeout": "3s", "retries": 20},
		"image":       "ghcr.io/tailscale/tailscale:v1.104.7", "hostname": p.Hostname, "restart": "unless-stopped",
		"cap_add": []string{"NET_ADMIN"}, "devices": []map[string]string{{"source": "/dev/net/tun", "target": "/dev/net/tun", "permissions": "rwm"}},
		"environment": map[string]string{"TS_AUTHKEY": p.AuthKey, "TS_AUTH_ONCE": "true", "TS_HOSTNAME": p.Hostname, "TS_STATE_DIR": "/var/lib/tailscale", "TS_USERSPACE": "false", "TS_ACCEPT_DNS": "false", "TS_SERVE_CONFIG": "/config/serve.json"},
		"networks":    networks,
		"volumes":     []map[string]any{{"type": "volume", "source": "cove-tailscale-state", "target": "/var/lib/tailscale"}, {"type": "bind", "source": servePath, "target": "/config/serve.json", "read_only": true}},
	}
	delete(app, "networks")
	app["network_mode"] = "service:" + sidecar
	depends, ok := app["depends_on"].(map[string]any)
	if !ok {
		depends = map[string]any{}
	}
	depends[sidecar] = map[string]any{"condition": "service_started"}
	app["depends_on"] = depends
	return nil
}

func (e *Engine) preparePrivateNetwork(ctx context.Context, r Request, dir string, args, env []string, out io.Writer) (string, error) {
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		return "", errors.New("Tailscale private networking requires /dev/net/tun on the worker")
	}
	resolved := &limitedBuffer{Limit: 2 << 20}
	if err := e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, resolved); err != nil {
		return "", errors.New("could not resolve private networking configuration")
	}
	var config map[string]any
	if err := json.Unmarshal(resolved.data.Bytes(), &config); err != nil {
		return "", err
	}
	servePath := filepath.Join(dir, r.ID+"-tailscale.json")
	serve := map[string]any{"TCP": map[string]any{fmt.Sprint(r.Private.Port): map[string]string{"TCPForward": fmt.Sprintf("127.0.0.1:%d", r.Private.Port)}}, "AllowFunnel": map[string]bool{}}
	if err := writeJSON(servePath, serve); err != nil {
		return "", err
	}
	if err := addPrivateNetwork(config, r.Private, servePath); err != nil {
		return "", err
	}
	path := filepath.Join(dir, r.ID+"-private.yaml")
	return path, writeJSON(path, config)
}
