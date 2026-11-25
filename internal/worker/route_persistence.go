package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const protectedRouteSource = "cove-protected-route-v2"

func (p *RoutePolicy) UnmarshalJSON(data []byte) error {
	type policy RoutePolicy
	var wire struct {
		policy
		Protocol int     `json:"cove_route_protocol,omitempty"`
		Source   *string `json:"cove_certificate_source,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("invalid route policy JSON")
	}
	if wire.CertificateSource == "cove-protected-tunnel-v1" {
		if wire.Protocol != 3 || wire.Source == nil {
			return errors.New("invalid protected tunnel route policy")
		}
		wire.CertificateSource = *wire.Source
	} else if wire.CertificateSource == protectedRouteSource {
		if wire.Protocol != 2 || wire.Source == nil {
			return errors.New("invalid protected route policy")
		}
		wire.CertificateSource = *wire.Source
		if !HasRouteExtensions([]Route{{Policy: (*RoutePolicy)(&wire.policy)}}) {
			return errors.New("invalid protected route policy")
		}
	} else if wire.Protocol != 0 || wire.Source != nil {
		return errors.New("unexpected protected route metadata")
	}
	*p = RoutePolicy(wire.policy)
	return nil
}

func marshalWorkerState(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	switch value.(type) {
	case journal, *journal, deployedState, *deployedState, backupManifest, *backupManifest, map[string][]Route:
	default:
		return data, nil
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	changed := protectPersistedRoutes(document)
	if protectPersistedBuild(document) {
		changed = true
	}
	if !changed {
		return data, nil
	}
	return json.Marshal(document)
}

func protectPersistedRoutes(value any) bool {
	changed := false
	switch node := value.(type) {
	case []any:
		for _, child := range node {
			changed = protectPersistedRoutes(child) || changed
		}
	case map[string]any:
		if _, domain := node["domain"].(string); domain {
			_, service := node["service"].(string)
			_, port := node["port"].(json.Number)
			policy, ok := node["policy"].(map[string]any)
			if service && port && ok && (policy["timeouts"] != nil || policy["visibility"] != nil || policy["listener"] != nil || policy["certificate_source"] == "default" || policy["certificate_id"] != nil || policy["maintenance"] != nil || policy["forward_auth"] != nil || policy["error_pages"] != nil) {
				source, _ := policy["certificate_source"].(string)
				// Older binaries reject this certificate source instead of silently dropping an auth gate.
				policy["certificate_source"] = protectedRouteSource
				policy["cove_certificate_source"] = source
				policy["cove_route_protocol"] = 2
				return true
			}
		}
		for _, child := range node {
			changed = protectPersistedRoutes(child) || changed
		}
	}
	return changed
}
