package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type ingressProxyChange struct {
	Digest       string                  `json:"digest,omitempty"`
	ID           string                  `json:"id"`
	Phase        string                  `json:"phase"`
	Committed    bool                    `json:"committed,omitempty"`
	Candidate    string                  `json:"candidate,omitempty"`
	Previous     *ingressProxySnapshot   `json:"previous,omitempty"`
	Installed    *ingressProxySnapshot   `json:"installed,omitempty"`
	Desired      IngressExposureSettings `json:"desired"`
	DesiredPorts map[string]int          `json:"desired_ports,omitempty"`
	Validator    string                  `json:"validator,omitempty"`
	Staging      string                  `json:"staging,omitempty"`
	StartedAt    time.Time               `json:"started_at"`
	FinishedAt   *time.Time              `json:"finished_at,omitempty"`
}

type ingressExposureState struct {
	Settings IngressExposureSettings `json:"settings"`
	Ports    map[string]int          `json:"ports,omitempty"`
	Change   *ingressProxyChange     `json:"change,omitempty"`
}

func (e *Engine) loadIngressExposures() (ingressExposureState, error) {
	var state ingressExposureState
	data, err := os.ReadFile(filepath.Join(e.Root, "ingress-exposures.json"))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil || len(data) > 4<<20 {
		return state, errors.New("managed listener recovery state could not be read")
	}
	if json.Unmarshal(data, &state) != nil {
		return state, errors.New("managed listener recovery state is unreadable")
	}
	if change := state.Change; change != nil {
		if change.Staging != "" && change.Staging != ".ingress-listener-check-"+change.ID {
			return state, errors.New("private proxy validation staging identity is invalid")
		}
		if change.Validator != "" && !validIngressValidatorName(change.Validator) {
			return state, errors.New("proxy validation recovery name is invalid")
		}
		if !identifier.MatchString(change.ID) || change.Phase == "" || change.StartedAt.IsZero() || change.FinishedAt == nil && change.Previous == nil {
			return state, errors.New("managed listener recovery checkpoint is incomplete")
		}
		switch change.Phase {
		case "validating", "applying", "restoring", "cleanup":
			if change.FinishedAt != nil {
				return state, errors.New("active proxy recovery checkpoint has a terminal timestamp")
			}
		case "completed", "failed", "interrupted":
			if change.FinishedAt == nil {
				return state, errors.New("terminal proxy recovery checkpoint has no completion timestamp")
			}
		default:
			return state, errors.New("proxy recovery checkpoint has an unknown phase")
		}
		for _, previous := range []*ingressProxySnapshot{change.Previous, change.Installed} {
			if previous == nil {
				continue
			}
			if len(previous.ID) != 64 || !containerIdentity.MatchString(previous.ID) || !imageDigest.MatchString(previous.Image) || len(previous.Networks) == 0 || len(previous.Networks) > 256 {
				return state, errors.New("proxy recovery snapshot identity is invalid")
			}
			if _, err := ingressProxyCreateBody(*previous, change.ID); err != nil {
				return state, errors.New("private proxy recovery configuration is unreadable")
			}
			for _, endpoint := range previous.Networks {
				if len(endpoint.NetworkID) != 64 || !containerIdentity.MatchString(endpoint.NetworkID) {
					return state, errors.New("proxy recovery network identity is invalid")
				}
			}
		}
	}
	return state, nil
}

func (e *Engine) saveIngressExposures(state ingressExposureState) error {
	data, err := json.Marshal(state)
	if err != nil || len(data) > 4<<20 {
		return errors.New("private listener recovery state exceeds its size budget")
	}
	return writeJSON(filepath.Join(e.Root, "ingress-exposures.json"), state)
}

func captureIngressProxy(ctx context.Context, client *http.Client) (ingressProxySnapshot, error) {
	var raw map[string]json.RawMessage
	var empty ingressProxySnapshot
	if err := ingressDockerJSON(ctx, client, "GET", "/containers/cove-ingress/json", nil, &raw); err != nil {
		return empty, err
	}
	var settings struct {
		Networks map[string]ingressProxyEndpoint
	}
	if json.Unmarshal(raw["NetworkSettings"], &settings) != nil {
		return empty, errors.New("proxy network configuration is unreadable")
	}
	for name, endpoint := range settings.Networks {
		var network struct{ ID, Name, Driver string }
		if err := ingressDockerJSON(ctx, client, "GET", "/networks/"+url.PathEscape(name), nil, &network); err != nil {
			return empty, err
		}
		if network.Name != name || name == "bridge" || network.Driver != "bridge" || len(network.ID) != 64 || !containerIdentity.MatchString(network.ID) || endpoint.NetworkID != "" && endpoint.NetworkID != network.ID {
			return empty, errors.New("proxy network identity or bridge driver could not be confirmed")
		}
		endpoint.NetworkID = network.ID
		if endpoint.IPAMConfig != nil {
			if endpoint.IPAddress == "" {
				endpoint.IPAddress = endpoint.IPAMConfig.IPv4Address
			}
			if endpoint.GlobalIPv6Address == "" {
				endpoint.GlobalIPv6Address = endpoint.IPAMConfig.IPv6Address
			}
		}
		settings.Networks[name] = endpoint
	}
	raw["NetworkSettings"], _ = json.Marshal(settings)
	data, err := json.Marshal([]any{raw})
	if err != nil {
		return empty, err
	}
	return inspectIngressProxy(data)
}

type ingressProxyContainer struct {
	ID, Name, Image string
	State           struct{ Running bool }
	Config          struct{ Labels map[string]string }
	NetworkSettings struct {
		Networks map[string]ingressProxyEndpoint
	}
}

func ingressProxyInspect(ctx context.Context, client *http.Client, id string) (*ingressProxyContainer, error) {
	isID := len(id) == 64 && containerIdentity.MatchString(id)
	if id != "cove-ingress" && !isID && !validIngressValidatorName(id) {
		return nil, errors.New("invalid proxy recovery identity")
	}
	var container ingressProxyContainer
	err := ingressDockerJSON(ctx, client, "GET", "/containers/"+id+"/json", nil, &container)
	if errors.Is(err, errIngressDockerMissing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(container.ID) != 64 || !containerIdentity.MatchString(container.ID) || isID && container.ID != id || !isID && container.Name != "/"+id {
		return nil, errors.New("proxy recovery identity was not confirmed")
	}
	return &container, nil
}

func ingressProxyOriginal(ctx context.Context, client *http.Client, change *ingressProxyChange) (*ingressProxyContainer, error) {
	if change.Previous == nil || !identifier.MatchString(change.ID) {
		return nil, errors.New("proxy recovery checkpoint has no original container")
	}
	original, err := ingressProxyInspect(ctx, client, change.Previous.ID)
	if err != nil {
		return nil, err
	}
	if original == nil {
		return nil, nil
	}
	if original.Config.Labels["cove.managed"] != "ingress" || original.Image != change.Previous.Image || original.Name != "/cove-ingress" && original.Name != "/cove-ingress-backup-"+change.ID {
		return nil, errors.New("original proxy ownership or identity changed; recovery is held")
	}
	return original, nil
}

func ingressProxyCandidate(container *ingressProxyContainer, change *ingressProxyChange) bool {
	return container != nil && change.Previous != nil && container.ID != change.Previous.ID && container.Config.Labels["cove.managed"] == "ingress" && container.Config.Labels["cove.ingress-revision"] == change.ID && container.Image == change.Previous.Image && (change.Candidate == "" || change.Candidate == container.ID)
}

func restoreIngressProxy(ctx context.Context, client *http.Client, change *ingressProxyChange) error {
	if change.Committed {
		return errors.New("a committed proxy change must finish instead of restoring the old proxy")
	}
	original, err := ingressProxyOriginal(ctx, client, change)
	if err != nil {
		return err
	}
	if original == nil {
		return errors.New("original proxy is missing; recovery remains held")
	}
	current, err := ingressProxyInspect(ctx, client, "cove-ingress")
	if err != nil {
		return err
	}
	if current != nil && current.ID != original.ID && !ingressProxyCandidate(current, change) {
		return errors.New("another container owns the proxy name; recovery left it unchanged")
	}
	for name, endpoint := range original.NetworkSettings.Networks {
		previous, ok := change.Previous.Networks[name]
		if !ok || endpoint.NetworkID != "" && endpoint.NetworkID != previous.NetworkID || endpoint.IPAddress != "" && endpoint.IPAddress != previous.IPAddress || endpoint.GlobalIPv6Address != "" && endpoint.GlobalIPv6Address != previous.GlobalIPv6Address || endpoint.MacAddress != "" && endpoint.MacAddress != previous.MacAddress {
			return errors.New("original proxy network attachments changed; recovery is held")
		}
		if original.State.Running && (endpoint.NetworkID != previous.NetworkID || endpoint.IPAddress != previous.IPAddress || endpoint.GlobalIPv6Address != previous.GlobalIPv6Address || endpoint.MacAddress != previous.MacAddress) {
			return errors.New("running original proxy peers changed; recovery is held")
		}
		if endpoint.IPAMConfig != nil && (endpoint.IPAMConfig.IPv4Address != "" && endpoint.IPAMConfig.IPv4Address != previous.IPAddress || endpoint.IPAMConfig.IPv6Address != "" && endpoint.IPAMConfig.IPv6Address != previous.GlobalIPv6Address) {
			return errors.New("original proxy requested peer addresses changed; recovery is held")
		}
	}
	var host map[string]json.RawMessage
	if json.Unmarshal(change.Previous.HostConfig, &host) != nil || host["RestartPolicy"] == nil {
		return errors.New("original proxy restart policy is unreadable; recovery remains held")
	}
	if current != nil && current.ID != original.ID {
		if err := ingressDockerJSON(ctx, client, "DELETE", "/containers/"+current.ID+"?force=1", nil, nil); err != nil {
			return err
		}
	}
	running := original.State.Running
	if running && (original.Name != "/cove-ingress" || len(original.NetworkSettings.Networks) != len(change.Previous.Networks) || !change.Previous.Running) {
		if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+original.ID+"/stop?t=10", nil, nil); err != nil {
			return err
		}
		running = false
	}
	if original.Name != "/cove-ingress" {
		if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+original.ID+"/rename?name=cove-ingress", nil, nil); err != nil {
			return err
		}
	}
	if !running {
		original, err = ingressProxyInspect(ctx, client, original.ID)
		if err != nil {
			return err
		}
		if original == nil {
			return errors.New("original proxy disappeared during restoration")
		}
	}
	for name, endpoint := range change.Previous.Networks {
		if existing, connected := original.NetworkSettings.Networks[name]; connected {
			if running || endpoint.IPAddress == "" && endpoint.GlobalIPv6Address == "" {
				continue
			}
			if existing.IPAMConfig != nil && existing.IPAMConfig.IPv4Address == endpoint.IPAddress && existing.IPAMConfig.IPv6Address == endpoint.GlobalIPv6Address {
				continue
			}
			if err := ingressDockerJSON(ctx, client, "POST", "/networks/"+endpoint.NetworkID+"/disconnect", map[string]any{"Container": original.ID, "Force": true}, nil); err != nil {
				return err
			}
		}
		connection := map[string]any{"Container": original.ID, "EndpointConfig": ingressProxyEndpointConfig(endpoint)}
		if err := ingressDockerJSON(ctx, client, "POST", "/networks/"+endpoint.NetworkID+"/connect", connection, nil); err != nil {
			return err
		}
	}
	if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+original.ID+"/update", map[string]json.RawMessage{"RestartPolicy": host["RestartPolicy"]}, nil); err != nil {
		return err
	}
	if change.Previous.Running && !running {
		if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+original.ID+"/start", nil, nil); err != nil {
			return err
		}
	}
	restored, err := ingressProxyInspect(ctx, client, original.ID)
	if err != nil {
		return err
	}
	if restored == nil || restored.Name != "/cove-ingress" || restored.State.Running != change.Previous.Running || len(restored.NetworkSettings.Networks) != len(change.Previous.Networks) {
		return errors.New("original proxy restoration is not confirmed")
	}
	for name, expected := range change.Previous.Networks {
		got := restored.NetworkSettings.Networks[name]
		if !restored.State.Running {
			if got.NetworkID != "" && got.NetworkID != expected.NetworkID {
				return errors.New("stopped proxy network identity changed")
			}
			if expected.IPAddress != "" || expected.GlobalIPv6Address != "" {
				if got.IPAMConfig == nil || got.IPAMConfig.IPv4Address != expected.IPAddress || got.IPAMConfig.IPv6Address != expected.GlobalIPv6Address {
					return errors.New("stopped proxy peer configuration was not restored")
				}
			}
			continue
		}
		if got.NetworkID != expected.NetworkID || got.IPAddress != expected.IPAddress || got.GlobalIPv6Address != expected.GlobalIPv6Address || got.MacAddress != expected.MacAddress {
			return errors.New("original proxy peer restoration is not confirmed")
		}
	}
	return nil
}

func finishIngressProxy(ctx context.Context, client *http.Client, change *ingressProxyChange) error {
	if !change.Committed {
		return errors.New("proxy cleanup requires a committed checkpoint")
	}
	current, err := ingressProxyInspect(ctx, client, "cove-ingress")
	if err != nil {
		return err
	}
	if !ingressProxyCandidate(current, change) {
		return errors.New("committed proxy replacement is not confirmed; cleanup remains held")
	}
	original, err := ingressProxyOriginal(ctx, client, change)
	if err != nil {
		return err
	}
	if original != nil {
		if original.Name != "/cove-ingress-backup-"+change.ID {
			return errors.New("retained proxy identity changed; cleanup remains held")
		}
		if original.State.Running {
			if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+original.ID+"/stop?t=10", nil, nil); err != nil {
				return err
			}
		}
	}
	if current.State.Running != change.Previous.Running {
		method := "start"
		if !change.Previous.Running {
			method = "stop?t=10"
		}
		if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+current.ID+"/"+method, nil, nil); err != nil {
			return err
		}
	}
	confirmed, err := ingressProxyInspect(ctx, client, current.ID)
	if err != nil {
		return err
	}
	if !ingressProxyCandidate(confirmed, change) || confirmed.Name != "/cove-ingress" || confirmed.State.Running != change.Previous.Running {
		return errors.New("committed proxy runtime intent was not confirmed")
	}
	if original != nil {
		if err := ingressDockerJSON(ctx, client, "DELETE", "/containers/"+original.ID+"?force=1", nil, nil); err != nil {
			return err
		}
	}
	return nil
}
