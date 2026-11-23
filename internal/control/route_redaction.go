package control

import (
	"encoding/json"

	"cove/internal/worker"
)

func routesWithoutSecrets(routes []worker.Route) []worker.Route {
	if routes == nil {
		return nil
	}
	result := append([]worker.Route{}, routes...)
	for i := range result {
		result[i].Auth = ""
		if result[i].Policy == nil {
			continue
		}
		policy := *result[i].Policy
		if policy.ForwardAuth != nil {
			auth := *policy.ForwardAuth
			auth.URL, auth.Path = "", ""
			policy.ForwardAuth = &auth
		}
		result[i].Policy = &policy
	}
	return result
}

func safeRouteJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var routes []worker.Route
	if json.Unmarshal(raw, &routes) != nil {
		return `"[redacted]"`
	}
	data, err := json.Marshal(routesWithoutSecrets(routes))
	if err != nil {
		return `"[redacted]"`
	}
	return string(data)
}

func redactConfigurationChanges(changes []configurationChange) {
	for i := range changes {
		for j := range changes[i].Diffs {
			diff := &changes[i].Diffs[j]
			if diff.Field == "routes" {
				diff.Before, diff.After = safeRouteJSON(json.RawMessage(diff.Before)), safeRouteJSON(json.RawMessage(diff.After))
			}
		}
	}
}

func portableRouteInput(routes []worker.Route, old []worker.Route) []routeInput {
	inputs := make([]routeInput, len(routes))
	for i, route := range routes {
		inputs[i].Route = route
		if route.Auth != "" {
			value := route.Auth
			inputs[i].Auth = &value
		}
		if route.Policy == nil || route.Policy.ForwardAuth == nil {
			continue
		}
		for _, previous := range old {
			if route.Domain != previous.Domain || previous.Policy == nil || previous.Policy.ForwardAuth == nil || route.Policy.PathPrefix != previous.Policy.PathPrefix {
				continue
			}
			policy, auth := *route.Policy, *route.Policy.ForwardAuth
			if auth.URL == "" && auth.Service == "" {
				auth.URL = previous.Policy.ForwardAuth.URL
				auth.Service, auth.Port = previous.Policy.ForwardAuth.Service, previous.Policy.ForwardAuth.Port
			}
			if auth.Path == "" {
				auth.Path = previous.Policy.ForwardAuth.Path
			}
			policy.ForwardAuth = &auth
			inputs[i].Policy = &policy
			break
		}
	}
	return inputs
}
