package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

const managedListenerPrefix = "cove-listener-"

func ingressListenerName(id string) string { return managedListenerPrefix + id }

func validateCoveFileProvider(snapshot ingressProxySnapshot, source []byte) error {
	if snapshot.Launch.Kind == "cli" {
		var config struct {
			Cmd []string
			Env []string
		}
		if json.Unmarshal(snapshot.Config, &config) != nil {
			return errors.New("proxy command is unreadable")
		}
		for _, value := range config.Env {
			if strings.HasPrefix(strings.ToUpper(value), "TRAEFIK_PROVIDERS_") {
				return errors.New("provider environment overrides need an explicit managed file-provider configuration before listener changes")
			}
		}
		directory := false
		for _, arg := range config.Cmd {
			lower := strings.ToLower(arg)
			if !strings.HasPrefix(lower, "--providers.") {
				continue
			}
			if !strings.HasPrefix(lower, "--providers.file.") {
				return errors.New("listener replacement supports the managed file provider only")
			}
			key, value, _ := strings.Cut(arg, "=")
			switch strings.ToLower(key) {
			case "--providers.file.directory":
				directory = path.Clean(value) == "/data/dynamic"
			case "--providers.file.filename":
				return errors.New("listener replacement requires the managed dynamic directory")
			case "--providers.file.watch":
				if strings.EqualFold(value, "false") {
					return errors.New("enable managed dynamic file watching before changing listeners")
				}
			}
		}
		if !directory {
			return errors.New("listener replacement requires the managed dynamic directory")
		}
		return nil
	}
	var config map[string]any
	if yaml.Unmarshal(source, &config) != nil {
		return errors.New("proxy static file is unreadable")
	}
	providers, ok := asMap(lookup(config, "providers"))
	if !ok {
		return errors.New("listener replacement requires the managed file provider")
	}
	for name, value := range providers {
		if !strings.EqualFold(name, "file") && !strings.EqualFold(name, "providersThrottleDuration") && value != nil && value != false {
			return errors.New("listener replacement supports the managed file provider only")
		}
	}
	file, ok := asMap(lookup(providers, "file"))
	if !ok {
		return errors.New("listener replacement requires the managed file provider")
	}
	directory, _ := lookup(file, "directory").(string)
	if path.Clean(directory) != "/data/dynamic" || lookup(file, "filename") != nil {
		return errors.New("listener replacement requires the managed dynamic directory")
	}
	if watch, ok := lookup(file, "watch").(bool); ok && !watch {
		return errors.New("enable managed dynamic file watching before changing listeners")
	}
	return nil
}

func ingressACMEStorage(snapshot ingressProxySnapshot, source []byte) (map[string]string, error) {
	result := map[string]string{}
	if snapshot.Launch.Kind == "cli" {
		var config struct{ Cmd []string }
		if json.Unmarshal(snapshot.Config, &config) != nil {
			return nil, errors.New("proxy command is unreadable")
		}
		for _, arg := range config.Cmd {
			for _, match := range resolverFlag.FindAllStringSubmatch(arg, -1) {
				if _, ok := result[match[1]]; !ok {
					result[match[1]] = ""
				}
			}
			key, value, ok := strings.Cut(arg, "=")
			if ok && strings.HasPrefix(strings.ToLower(key), "--certificatesresolvers.") && strings.HasSuffix(strings.ToLower(key), ".acme.storage") {
				matches := resolverFlag.FindStringSubmatch(arg)
				if len(matches) == 2 {
					result[matches[1]] = value
				}
			}
		}
	} else {
		var config map[string]any
		if yaml.Unmarshal(source, &config) != nil {
			return nil, errors.New("proxy static file is unreadable")
		}
		resolvers, _ := asMap(lookup(config, "certificatesResolvers"))
		for name, raw := range resolvers {
			definition, _ := asMap(raw)
			acme, ok := asMap(lookup(definition, "acme"))
			if !ok {
				continue
			}
			storage, _ := lookup(acme, "storage").(string)
			result[name] = storage
		}
	}
	if len(result) > 32 {
		return nil, errors.New("at most 32 certificate resolvers can be validated during listener changes")
	}
	for name, storage := range result {
		clean := path.Clean(storage)
		if !resolverName.MatchString(name) || !strings.HasPrefix(clean, "/data/") || len(clean) > 4096 || strings.ContainsAny(clean, "\x00\r\n") {
			return nil, errors.New("certificate resolver storage must be explicit and persisted under the proxy data volume before listener replacement")
		}
		result[name] = clean
	}
	return result, nil
}

func ingressProxyHTTP3(snapshot ingressProxySnapshot, source []byte) bool {
	if snapshot.Launch.Kind == "cli" {
		var config struct{ Cmd []string }
		if json.Unmarshal(snapshot.Config, &config) != nil {
			return false
		}
		enabled := false
		for _, arg := range config.Cmd {
			value := strings.ToLower(arg)
			switch value {
			case "--entrypoints.websecure.http3", "--entrypoints.websecure.http3=true":
				enabled = true
			case "--entrypoints.websecure.http3=false":
				enabled = false
			default:
				if strings.HasPrefix(value, "--entrypoints.websecure.http3.") {
					enabled = true
				}
			}
		}
		return enabled
	}
	var config map[string]any
	if yaml.Unmarshal(source, &config) != nil {
		return false
	}
	points, _ := asMap(lookup(config, "entryPoints"))
	secure, _ := asMap(lookup(points, "websecure"))
	value := lookup(secure, "http3")
	if enabled, ok := value.(bool); ok {
		return enabled
	}
	return value != nil
}

func validateListenerPorts(settings IngressExposureSettings, ports map[string]int) error {
	if err := validateIngressExposures(settings); err != nil {
		return err
	}
	if len(ports) != len(settings.Listeners) {
		return errors.New("managed listener allocation is incomplete")
	}
	used := map[int]bool{}
	for _, listener := range settings.Listeners {
		port := ports[listener.ID]
		if port < 1024 || port > 65535 || port == 8080 || used[port] {
			return errors.New("managed listener container ports must be unique unreserved ports")
		}
		used[port] = true
	}
	return nil
}

func ingressExposureCLI(snapshot ingressProxySnapshot, settings IngressExposureSettings, ports, previous map[string]int) ([]string, error) {
	if snapshot.Launch.Kind != "cli" {
		return nil, errors.New("this proxy uses a static configuration file")
	}
	if err := validateListenerPorts(settings, ports); err != nil {
		return nil, err
	}
	var config struct{ Cmd []string }
	if json.Unmarshal(snapshot.Config, &config) != nil {
		return nil, errors.New("private proxy command is unreadable")
	}
	result := []string{}
	for _, arg := range config.Cmd {
		lower := strings.ToLower(arg)
		if strings.HasPrefix(lower, "--entrypoints.websecure.http3") {
			continue
		}
		if strings.HasPrefix(lower, "--entrypoints."+managedListenerPrefix) {
			name, _, _ := strings.Cut(strings.TrimPrefix(lower, "--entrypoints."), ".")
			if _, owned := previous[strings.TrimPrefix(name, managedListenerPrefix)]; !owned {
				return nil, errors.New("an unmanaged entrypoint uses Cove's reserved listener namespace")
			}
			continue
		}
		result = append(result, arg)
	}
	if settings.HTTP3 {
		result = append(result, "--entrypoints.websecure.http3=true", "--entrypoints.websecure.http3.advertisedport="+strconv.Itoa(settings.Public.HTTPS[0].Port))
	}
	listeners := append([]IngressListener{}, settings.Listeners...)
	slices.SortFunc(listeners, func(a, b IngressListener) int { return strings.Compare(a.ID, b.ID) })
	for _, listener := range listeners {
		protocol := "tcp"
		if listener.Protocol == "udp" {
			protocol = "udp"
		}
		result = append(result, fmt.Sprintf("--entrypoints.%s.address=:%d/%s", ingressListenerName(listener.ID), ports[listener.ID], protocol))
	}
	return result, nil
}

func ingressExposureStaticFile(source []byte, settings IngressExposureSettings, ports, previous map[string]int) ([]byte, error) {
	if err := validateListenerPorts(settings, ports); err != nil {
		return nil, err
	}
	if len(source) > 1<<20 {
		return nil, errors.New("proxy static configuration exceeds its private size limit")
	}
	var config map[string]any
	if yaml.Unmarshal(source, &config) != nil || len(config) == 0 {
		return nil, errors.New("proxy static configuration is unreadable")
	}
	key := "entryPoints"
	for name := range config {
		if strings.EqualFold(name, key) {
			key = name
			break
		}
	}
	entrypoints, ok := asMap(config[key])
	if !ok {
		return nil, errors.New("proxy static configuration has no explicit entrypoints")
	}
	for name := range entrypoints {
		if strings.HasPrefix(strings.ToLower(name), managedListenerPrefix) {
			if _, owned := previous[strings.TrimPrefix(strings.ToLower(name), managedListenerPrefix)]; !owned {
				return nil, errors.New("an unmanaged entrypoint uses Cove's reserved listener namespace")
			}
			delete(entrypoints, name)
		}
	}
	secureKey := ""
	for name := range entrypoints {
		if strings.EqualFold(name, "websecure") {
			secureKey = name
			break
		}
	}
	secure, ok := asMap(entrypoints[secureKey])
	if !ok {
		return nil, errors.New("proxy static configuration has no explicit HTTPS entrypoint")
	}
	for name := range secure {
		if strings.EqualFold(name, "http3") {
			delete(secure, name)
		}
	}
	if settings.HTTP3 {
		secure["http3"] = map[string]any{"advertisedPort": settings.Public.HTTPS[0].Port}
	}
	entrypoints[secureKey] = secure
	for _, listener := range settings.Listeners {
		protocol := "tcp"
		if listener.Protocol == "udp" {
			protocol = "udp"
		}
		entrypoints[ingressListenerName(listener.ID)] = map[string]any{"address": fmt.Sprintf(":%d/%s", ports[listener.ID], protocol)}
	}
	config[key] = entrypoints
	if providers, ok := asMap(lookup(config, "providers")); ok {
		for name, value := range providers {
			if strings.EqualFold(name, "docker") && value != nil && value != false {
				return nil, errors.New("managed listener changes do not support the Docker provider")
			}
		}
	}
	result, err := yaml.Marshal(config)
	if err != nil {
		return nil, errors.New("updated proxy static configuration could not be encoded")
	}
	return result, nil
}
