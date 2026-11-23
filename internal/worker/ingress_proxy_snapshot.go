package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

type ingressProxySnapshot struct {
	ID         string                          `json:"id"`
	Image      string                          `json:"image"`
	Running    bool                            `json:"running"`
	Config     json.RawMessage                 `json:"config"`
	HostConfig json.RawMessage                 `json:"host_config"`
	Networks   map[string]ingressProxyEndpoint `json:"networks"`
	Launch     ingressProxyLaunch              `json:"launch"`
}

type ingressProxyLaunch struct {
	Kind       string `json:"kind"`
	StaticFile string `json:"static_file,omitempty"`
}

type ingressProxyEndpoint struct {
	NetworkID         string            `json:"NetworkID"`
	Aliases           []string          `json:"Aliases"`
	Links             []string          `json:"Links"`
	IPAddress         string            `json:"IPAddress"`
	GlobalIPv6Address string            `json:"GlobalIPv6Address"`
	MacAddress        string            `json:"MacAddress"`
	DriverOpts        map[string]string `json:"DriverOpts"`
	GwPriority        int               `json:"GwPriority"`
	IPAMConfig        *struct {
		IPv4Address  string   `json:"IPv4Address,omitempty"`
		IPv6Address  string   `json:"IPv6Address,omitempty"`
		LinkLocalIPs []string `json:"LinkLocalIPs,omitempty"`
	} `json:"IPAMConfig"`
}

func inspectIngressProxy(data []byte) (ingressProxySnapshot, error) {
	var result ingressProxySnapshot
	if len(data) > 2<<20 {
		return result, errors.New("proxy launch definition exceeds its private snapshot limit")
	}
	var inspected []struct {
		ID         string          `json:"Id"`
		Name       string          `json:"Name"`
		Image      string          `json:"Image"`
		Config     json.RawMessage `json:"Config"`
		HostConfig json.RawMessage `json:"HostConfig"`
		State      struct {
			Running *bool `json:"Running"`
		} `json:"State"`
		NetworkSettings struct {
			Networks map[string]ingressProxyEndpoint `json:"Networks"`
		} `json:"NetworkSettings"`
		Mounts []struct {
			Type        string `json:"Type"`
			Name        string `json:"Name"`
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
		} `json:"Mounts"`
	}
	if json.Unmarshal(data, &inspected) != nil || len(inspected) != 1 {
		return result, errors.New("managed proxy inspection is unreadable or ambiguous")
	}
	item := inspected[0]
	if item.Name != "/cove-ingress" || len(item.ID) != 64 || !containerIdentity.MatchString(item.ID) || !imageDigest.MatchString(item.Image) || item.State.Running == nil {
		return result, errors.New("managed proxy identity or running state could not be confirmed")
	}
	var config struct {
		Image      string            `json:"Image"`
		Labels     map[string]string `json:"Labels"`
		Entrypoint []string          `json:"Entrypoint"`
		Cmd        []string          `json:"Cmd"`
		Env        []string          `json:"Env"`
	}
	var host struct {
		NetworkMode     string                                         `json:"NetworkMode"`
		Privileged      bool                                           `json:"Privileged"`
		AutoRemove      bool                                           `json:"AutoRemove"`
		PublishAllPorts bool                                           `json:"PublishAllPorts"`
		VolumesFrom     []string                                       `json:"VolumesFrom"`
		Binds           []string                                       `json:"Binds"`
		Mounts          []struct{ Type, Source, Target string }        `json:"Mounts"`
		PortBindings    map[string][]struct{ HostIP, HostPort string } `json:"PortBindings"`
	}
	if json.Unmarshal(item.Config, &config) != nil || json.Unmarshal(item.HostConfig, &host) != nil || config.Labels["cove.managed"] != "ingress" || !supportedIngressProxyImage(config.Image, item.Image, config.Labels) {
		return result, errors.New("existing proxy launch is not a supported Cove-managed Traefik container")
	}
	if host.Privileged || host.NetworkMode == "host" || host.NetworkMode == "none" || strings.HasPrefix(host.NetworkMode, "container:") {
		return result, errors.New("managed listener replacement requires an unprivileged bridged proxy")
	}
	if host.AutoRemove || host.PublishAllPorts || len(host.VolumesFrom) > 0 {
		return result, errors.New("proxy replacement requires retained containers, explicit publications and independent mounts")
	}
	for _, bindings := range host.PortBindings {
		for _, binding := range bindings {
			port, err := strconv.Atoi(binding.HostPort)
			if err != nil || port < 1 || port > 65535 {
				return result, errors.New("proxy replacement requires fixed host publication ports")
			}
		}
	}
	for _, mount := range item.Mounts {
		if mount.Type == "volume" {
			named := false
			for _, binding := range host.Binds {
				parts := strings.Split(binding, ":")
				if len(parts) >= 2 && parts[0] == mount.Name && parts[1] == mount.Destination && mount.Name != "" {
					named = true
				}
			}
			for _, configured := range host.Mounts {
				if configured.Type == "volume" && configured.Source == mount.Name && configured.Target == mount.Destination && mount.Name != "" {
					named = true
				}
			}
			if !named {
				return result, errors.New("proxy volume mounts need explicit named sources before listener replacement")
			}
		}
		if mount.Type == "tmpfs" && mount.Source == "" {
			mount.Source = mount.Destination
		}
		for _, path := range []string{mount.Source, mount.Destination} {
			path = strings.TrimRight(path, "/")
			if path == "" || path == "/run" || path == "/var/run" || strings.HasSuffix(path, ".sock") {
				return result, errors.New("proxy mounts must not expose a Docker socket or its runtime directory")
			}
		}
	}
	for _, value := range config.Env {
		key, setting, _ := strings.Cut(value, "=")
		key = strings.ToUpper(key)
		if strings.HasPrefix(key, "TRAEFIK_PROVIDERS_DOCKER") && !(key == "TRAEFIK_PROVIDERS_DOCKER" && strings.EqualFold(setting, "false")) {
			return result, errors.New("proxy replacement does not support the Docker configuration provider")
		}
	}
	launch, err := ingressProxyLaunchShape(config.Entrypoint, config.Cmd)
	if err != nil {
		return result, err
	}
	if len(item.NetworkSettings.Networks) == 0 || len(item.NetworkSettings.Networks) > 256 {
		return result, errors.New("proxy network attachments are missing or exceed the snapshot limit")
	}
	for name, endpoint := range item.NetworkSettings.Networks {
		if name == "" || len(name) > 256 || strings.ContainsAny(name, " \t\r\n\x00/") || len(endpoint.NetworkID) != 64 || !containerIdentity.MatchString(endpoint.NetworkID) || len(endpoint.Aliases) > 256 {
			return result, errors.New("proxy network identity could not be confirmed")
		}
		for _, address := range []struct {
			value string
			v4    bool
		}{{endpoint.IPAddress, true}, {endpoint.GlobalIPv6Address, false}} {
			if address.value == "" {
				continue
			}
			ip, err := netip.ParseAddr(address.value)
			if err != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() || ip.Is4() != address.v4 {
				return result, errors.New("proxy has an invalid effective network address")
			}
		}
		if endpoint.MacAddress != "" {
			mac, err := net.ParseMAC(endpoint.MacAddress)
			if err != nil || len(mac) != 6 {
				return result, errors.New("proxy has an invalid effective network MAC address")
			}
		}
	}
	return ingressProxySnapshot{ID: item.ID, Image: item.Image, Running: *item.State.Running, Config: item.Config, HostConfig: item.HostConfig, Networks: item.NetworkSettings.Networks, Launch: launch}, nil
}

func ingressProxyLaunchShape(entrypoint, command []string) (ingressProxyLaunch, error) {
	invalid := errors.New("proxy startup command is unfamiliar; existing container was left unchanged")
	flags := func(args []string) bool {
		if len(args) == 0 || len(args) > 256 {
			return false
		}
		for _, arg := range args {
			lower := strings.ToLower(arg)
			if len(arg) > 4096 || !strings.HasPrefix(arg, "--") || strings.ContainsAny(arg, "\x00\r\n") || strings.HasPrefix(lower, "--configfile") || strings.HasPrefix(lower, "--providers.docker") && lower != "--providers.docker=false" {
				return false
			}
		}
		return true
	}
	if slices.Equal(entrypoint, []string{"traefik"}) || slices.Equal(entrypoint, []string{"/entrypoint.sh"}) {
		if flags(command) {
			return ingressProxyLaunch{Kind: "cli"}, nil
		}
		return ingressProxyLaunch{}, invalid
	}
	if len(entrypoint) == 0 && len(command) > 1 && command[0] == "traefik" && flags(command[1:]) {
		return ingressProxyLaunch{Kind: "cli"}, nil
	}
	var script string
	var args []string
	if slices.Equal(entrypoint, []string{"sh", "-ec"}) && len(command) >= 1 {
		script, args = command[0], command[1:]
	} else if slices.Equal(entrypoint, []string{"sh"}) && len(command) >= 2 && command[0] == "-ec" {
		script, args = command[1], command[2:]
	} else {
		return ingressProxyLaunch{}, invalid
	}
	if strings.TrimSpace(script) == `mkdir -p /data/dynamic; exec traefik "$@"` && len(args) > 1 && args[0] == "traefik" && flags(args[1:]) {
		return ingressProxyLaunch{Kind: "cli"}, nil
	}
	lines := strings.FieldsFunc(strings.ReplaceAll(script, "\r\n", "\n"), func(r rune) bool { return r == '\n' || r == ';' })
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	if len(args) != 0 || len(lines) != 3 || lines[0] != "mkdir -p /data/dynamic" || lines[1] != "cp /etc/traefik/panel.yaml /data/dynamic/panel.yaml" {
		return ingressProxyLaunch{}, invalid
	}
	prefix := "exec traefik --configFile="
	if !strings.HasPrefix(lines[2], prefix) || !staticFilePath.MatchString(strings.TrimPrefix(lines[2], prefix)) {
		return ingressProxyLaunch{}, invalid
	}
	return ingressProxyLaunch{Kind: "file", StaticFile: strings.TrimPrefix(lines[2], prefix)}, nil
}

func supportedIngressProxyImage(reference, image string, labels map[string]string) bool {
	return strings.HasPrefix(reference, "traefik:") || imageDigest.MatchString(reference) && reference == image && identifier.MatchString(labels["cove.ingress-revision"]) && strings.HasPrefix(labels["cove.ingress-source-image"], "traefik:")
}

func ingressProxyCreateBody(snapshot ingressProxySnapshot, revision string) (map[string]json.RawMessage, error) {
	if !identifier.MatchString(revision) {
		return nil, errors.New("invalid managed proxy replacement revision")
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(snapshot.Config, &body) != nil || body == nil {
		return nil, errors.New("private proxy launch configuration is unreadable")
	}
	var labels map[string]string
	if json.Unmarshal(body["Labels"], &labels) != nil || labels["cove.managed"] != "ingress" {
		return nil, errors.New("private proxy launch ownership is unreadable")
	}
	var reference string
	if json.Unmarshal(body["Image"], &reference) != nil {
		return nil, errors.New("private proxy image reference is unreadable")
	}
	if strings.HasPrefix(reference, "traefik:") {
		labels["cove.ingress-source-image"] = reference
	}
	labels["cove.ingress-revision"] = revision
	body["Labels"], _ = json.Marshal(labels)
	body["Image"], _ = json.Marshal(snapshot.Image)
	body["HostConfig"] = append(json.RawMessage{}, snapshot.HostConfig...)
	endpoints := make(map[string]map[string]any, len(snapshot.Networks))
	for name, endpoint := range snapshot.Networks {
		endpoints[name] = ingressProxyEndpointConfig(endpoint)
	}
	body["NetworkingConfig"], _ = json.Marshal(map[string]any{"EndpointsConfig": endpoints})
	return body, nil
}

func ingressProxyEndpointConfig(endpoint ingressProxyEndpoint) map[string]any {
	ipam := map[string]any{}
	if endpoint.IPAMConfig != nil && len(endpoint.IPAMConfig.LinkLocalIPs) > 0 {
		ipam["LinkLocalIPs"] = endpoint.IPAMConfig.LinkLocalIPs
	}
	if endpoint.IPAddress != "" {
		ipam["IPv4Address"] = endpoint.IPAddress
	}
	if endpoint.GlobalIPv6Address != "" {
		ipam["IPv6Address"] = endpoint.GlobalIPv6Address
	}
	return map[string]any{
		"NetworkID": endpoint.NetworkID, "Aliases": endpoint.Aliases, "Links": endpoint.Links,
		"MacAddress": endpoint.MacAddress, "DriverOpts": endpoint.DriverOpts,
		"GwPriority": endpoint.GwPriority, "IPAMConfig": ipam,
	}
}

type ingressDockerConnection struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *ingressDockerConnection) Close() error {
	c.cancel()
	return c.Conn.Close()
}

func (e *Engine) ingressDockerClient() *http.Client {
	transport := &http.Transport{DisableCompression: true, MaxIdleConns: 1, MaxIdleConnsPerHost: 1, MaxResponseHeaderBytes: 65536, ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
		if address != "docker:80" {
			return nil, errors.New("unexpected Docker control destination")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// The CLI retains the configured Docker context and its SSH/TLS verification.
		commandContext, cancel := context.WithCancel(context.Background())
		command := exec.CommandContext(commandContext, "docker", "system", "dial-stdio")
		command.Dir = e.Root
		command.Stderr = io.Discard
		input, err := command.StdinPipe()
		if err != nil {
			cancel()
			return nil, errors.New("could not open the Docker CLI control transport")
		}
		output, err := command.StdoutPipe()
		if err != nil {
			input.Close()
			cancel()
			return nil, errors.New("could not open the Docker CLI control transport")
		}
		if err := command.Start(); err != nil {
			input.Close()
			output.Close()
			cancel()
			return nil, errors.New("could not start the Docker CLI control transport")
		}
		client, relay := net.Pipe()
		go func() {
			io.Copy(input, relay)
			input.Close()
		}()
		go func() {
			io.Copy(relay, output)
			relay.Close()
			input.Close()
			cancel()
			command.Wait()
		}()
		return &ingressDockerConnection{Conn: client, cancel: cancel}, nil
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var errIngressDockerMissing = errors.New("Docker Engine object was not found")

func ingressDockerJSON(ctx context.Context, client *http.Client, method, path string, body, result any) error {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\r\n\x00#") {
		return errors.New("invalid Docker control path")
	}
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return errors.New("could not encode the private Docker launch request")
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, bytes.NewReader(payload))
	if err != nil {
		return errors.New("invalid Docker control request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Docker CLI control transport did not complete the request")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return errors.New("Docker control response is incomplete or exceeds its private size limit")
	}
	if response.StatusCode == 404 {
		return errIngressDockerMissing
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Docker Engine request failed (HTTP %d)", response.StatusCode)
	}
	if result != nil && json.Unmarshal(data, result) != nil {
		return errors.New("Docker control response is unreadable")
	}
	return nil
}
