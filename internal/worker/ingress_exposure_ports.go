package worker

import (
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

func ingressStaticEntrypoints(snapshot ingressProxySnapshot, source []byte) ([]IngressEntrypoint, error) {
	entries := []IngressEntrypoint{}
	if snapshot.Launch.Kind == "cli" {
		var config struct{ Cmd []string }
		if json.Unmarshal(snapshot.Config, &config) != nil {
			return nil, errors.New("private proxy command is unreadable")
		}
		for _, arg := range config.Cmd {
			if !strings.HasPrefix(strings.ToLower(arg), "--entrypoints.") {
				continue
			}
			name, value, ok := strings.Cut(arg[len("--entrypoints."):], ".")
			if ok && strings.HasPrefix(strings.ToLower(value), "address=") {
				entries = append(entries, IngressEntrypoint{Name: name, Address: value[len("address="):]})
			}
		}
	} else {
		var config map[string]any
		if len(source) > 1<<20 || yaml.Unmarshal(source, &config) != nil {
			return nil, errors.New("private proxy static configuration is unreadable")
		}
		points, ok := asMap(lookup(config, "entryPoints"))
		if !ok {
			return nil, errors.New("proxy entrypoints are not explicit")
		}
		for name, raw := range points {
			point, ok := asMap(raw)
			if !ok {
				return nil, errors.New("proxy entrypoint definition is unreadable")
			}
			address, ok := lookup(point, "address").(string)
			if !ok {
				return nil, errors.New("proxy entrypoint address is not explicit")
			}
			entries = append(entries, IngressEntrypoint{Name: name, Address: address})
		}
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.Name == "" || seen[entry.Name] {
			return nil, errors.New("proxy entrypoint definitions are ambiguous")
		}
		if _, _, err := ingressEntrypointPort(entry.Address); err != nil {
			return nil, err
		}
		seen[entry.Name] = true
	}
	slices.SortFunc(entries, func(a, b IngressEntrypoint) int { return strings.Compare(a.Name, b.Name) })
	return entries, nil
}

func ingressEntrypointPort(address string) (int, string, error) {
	protocol := "tcp"
	if before, suffix, ok := strings.Cut(address, "/"); ok {
		if suffix != "tcp" && suffix != "udp" {
			return 0, "", errors.New("proxy entrypoint uses an unsupported transport")
		}
		address, protocol = before, suffix
	}
	_, raw, err := net.SplitHostPort(address)
	if err != nil {
		return 0, "", errors.New("proxy entrypoint uses an unfamiliar bind address")
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, "", errors.New("proxy entrypoint port is invalid")
	}
	return port, protocol, nil
}

func allocateIngressListenerPorts(settings IngressExposureSettings, previous map[string]int, entries []IngressEntrypoint) (map[string]int, error) {
	if err := validateIngressExposures(settings); err != nil {
		return nil, err
	}
	used := map[int]int{}
	owned := map[string]bool{}
	for _, entry := range entries {
		port, protocol, err := ingressEntrypointPort(entry.Address)
		if err != nil {
			return nil, err
		}
		if settings.HTTP3 && protocol == "udp" && port == 443 {
			return nil, errors.New("an existing UDP entrypoint reserves the HTTPS HTTP/3 port")
		}
		used[port]++
		if strings.HasPrefix(strings.ToLower(entry.Name), managedListenerPrefix) {
			id := strings.TrimPrefix(entry.Name, managedListenerPrefix)
			if previous[id] != port {
				return nil, errors.New("reserved listener entrypoint does not match Cove's saved allocation")
			}
			owned[id] = true
		}
	}
	for id, port := range previous {
		if !owned[id] {
			return nil, errors.New("a saved managed listener is missing from the proxy's static configuration")
		}
		used[port]--
	}
	assigned := map[string]int{}
	listeners := append([]IngressListener{}, settings.Listeners...)
	slices.SortFunc(listeners, func(a, b IngressListener) int { return strings.Compare(a.ID, b.ID) })
	for _, listener := range listeners {
		port := previous[listener.ID]
		if port == 0 {
			continue
		}
		if used[port] > 0 {
			return nil, errors.New("a managed listener overlaps an unmanaged entrypoint")
		}
		assigned[listener.ID] = port
		used[port]++
	}
	for _, listener := range listeners {
		if assigned[listener.ID] != 0 {
			continue
		}
		port := 0
		if port == 0 {
			for candidate := 20000; candidate <= 60000; candidate++ {
				if used[candidate] == 0 {
					port = candidate
					break
				}
			}
		}
		if port == 0 {
			return nil, errors.New("no container port is available for the managed listener")
		}
		assigned[listener.ID] = port
		used[port]++
	}
	if err := validateListenerPorts(settings, assigned); err != nil {
		return nil, err
	}
	return assigned, nil
}
