package worker

import (
	"encoding/json"
	"errors"
	"strings"

	"go.yaml.in/yaml/v3"
)

func transportRouteReferences(routes map[string][]Route) map[string]string {
	refs := map[string]string{}
	for _, list := range routes {
		for _, route := range list {
			if route.IsTransportRoute() {
				refs[ingressListenerName(route.listener())] = route.Protocol
			}
		}
	}
	return refs
}

func transportRouterConflict(section string, entries []string, refs map[string]string) bool {
	for listener, protocol := range refs {
		for _, entry := range entries {
			if entry == listener {
				return true
			}
		}
		if len(entries) == 0 && (section == "udp") == (protocol == "udp") {
			return true
		}
	}
	return false
}

func validateTransportCustomConfig(content string, refs map[string]string) error {
	if len(refs) == 0 {
		return nil
	}
	var config map[string]any
	if yaml.Unmarshal([]byte(content), &config) != nil {
		return errors.New("transport reservations require readable custom configuration")
	}
	for _, section := range []string{"http", "tcp", "udp"} {
		group, _ := asMap(lookup(config, section))
		routers, _ := asMap(lookup(group, "routers"))
		for name, value := range routers {
			body, ok := asMap(value)
			if !ok {
				return errors.New("custom router configuration is unreadable")
			}
			router := routerDefinition(section, name, body)
			if transportRouterConflict(section, router.EntryPoints, refs) {
				return errors.New("a custom router shares a dedicated workload transport listener; choose explicit other entrypoints")
			}
		}
	}
	return nil
}

func validateTransportLiveData(data []byte, refs map[string]string, owned dynamicIngress) error {
	if len(refs) == 0 {
		return nil
	}
	var raw map[string]map[string]map[string]any
	if json.Unmarshal(data, &raw) != nil {
		return errors.New("transport reservations require readable proxy runtime data")
	}
	for section, group := range map[string]string{"http": "routers", "tcp": "tcpRouters", "udp": "udpRouters"} {
		for name, body := range raw[group] {
			base, file := strings.CutSuffix(name, "@file")
			if !file {
				continue
			}
			managed := false
			switch section {
			case "http":
				_, managed = owned.HTTP.Routers[base]
			case "tcp":
				if owned.TCP != nil {
					_, managed = owned.TCP.Routers[base]
				}
			case "udp":
				if owned.UDP != nil {
					_, managed = owned.UDP.Routers[base]
				}
			}
			if managed {
				continue
			}
			router := routerDefinition(section, base, body)
			if transportRouterConflict(section, router.EntryPoints, refs) {
				return errors.New("a custom runtime router conflicts with a dedicated workload transport listener")
			}
		}
	}
	return nil
}
