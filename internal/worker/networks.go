package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

type NetworkAttachment struct {
	ID      string `json:"network_id"`
	Service string `json:"service"`
	Alias   string `json:"alias"`
}

var networkID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func ValidateNetworks(networks []NetworkAttachment) error {
	if len(networks) > 32 {
		return errors.New("at most 32 shared network connections are allowed")
	}
	seen := map[string]bool{}
	for _, n := range networks {
		key := n.ID + "/" + n.Service
		if !networkID.MatchString(n.ID) || !serviceIdentifier.MatchString(n.Service) || !domainLabel.MatchString(n.Alias) || len(n.Alias) > 63 || seen[key] {
			return errors.New("invalid or duplicate shared network connection")
		}
		seen[key] = true
	}
	return nil
}

func addSharedNetworks(config map[string]any, attachments []NetworkAttachment) error {
	services, ok := config["services"].(map[string]any)
	if !ok {
		return errors.New("Compose has no services")
	}
	networks, ok := config["networks"].(map[string]any)
	if !ok {
		networks = map[string]any{}
		config["networks"] = networks
	}
	added := map[string]bool{}
	for _, n := range attachments {
		service, ok := services[n.Service].(map[string]any)
		if !ok {
			return errors.New("shared network service does not exist in Compose")
		}
		if _, ok = service["network_mode"]; ok {
			return errors.New("shared networks cannot be used with network_mode")
		}
		name := "cove-shared-" + n.ID
		if _, exists := networks[name]; exists && !added[name] {
			return errors.New("Compose uses a reserved shared network name")
		}
		networks[name] = map[string]any{"external": true, "name": name}
		added[name] = true
	}
	for _, n := range attachments {
		service := services[n.Service].(map[string]any)
		existing, ok := service["networks"].(map[string]any)
		if !ok {
			return errors.New("could not resolve service networks")
		}
		existing["cove-shared-"+n.ID] = map[string]any{"aliases": []string{n.Alias}}
	}
	return nil
}

func (e *Engine) prepareNetworks(ctx context.Context, r Request, dir string, args, env []string, out io.Writer) (string, error) {
	resolved := &limitedBuffer{Limit: 2 * 1024 * 1024}
	if err := e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, resolved); err != nil {
		return "", errors.New("could not resolve Compose for shared networks")
	}
	var config map[string]any
	if err := unmarshalCompose(resolved.data.Bytes(), &config); err != nil {
		return "", errors.New("invalid resolved Compose configuration")
	}
	if err := addSharedNetworks(config, r.Networks); err != nil {
		return "", err
	}
	e.networkMu.Lock()
	defer e.networkMu.Unlock()
	for _, n := range r.Networks {
		name := "cove-shared-" + n.ID
		inspect := func() error {
			result := &limitedBuffer{Limit: 65536}
			if err := e.Run(ctx, dir, []string{"docker", "network", "inspect", name}, nil, result); err != nil {
				return err
			}
			var entries []struct {
				Driver     string
				Internal   bool
				Labels     map[string]string
				Containers map[string]json.RawMessage
			}
			if json.Unmarshal(result.data.Bytes(), &entries) != nil || len(entries) != 1 || entries[0].Driver != "bridge" || !entries[0].Internal || entries[0].Labels["cove.shared-network"] != n.ID {
				return errors.New("existing network is not the expected Cove private network")
			}
			for container := range entries[0].Containers {
				info := &limitedBuffer{Limit: 65536}
				if err := e.Run(ctx, dir, []string{"docker", "inspect", "--format", "{{index .Config.Labels \"com.docker.compose.project\"}}\n{{json .NetworkSettings.Networks}}", container}, nil, info); err != nil {
					return err
				}
				owner, raw, ok := strings.Cut(strings.TrimSpace(info.data.String()), "\n")
				if !ok {
					return errors.New("could not inspect connected network aliases")
				}
				if owner == e.composeProject(r.Workload) {
					continue
				}
				var connected map[string]struct{ Aliases []string }
				if json.Unmarshal([]byte(raw), &connected) != nil {
					return errors.New("invalid connected network aliases")
				}
				for _, alias := range connected[name].Aliases {
					if alias == n.Alias {
						return errors.New("DNS alias is still attached to another resource; redeploy that resource to disconnect it first")
					}
				}
			}
			return nil
		}
		existing, err := e.output(ctx, "docker", "network", "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}")
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(existing) == "" {
			if err = e.Run(ctx, dir, []string{"docker", "network", "create", "--driver", "bridge", "--internal", "--label", "cove.shared-network=" + n.ID, name}, nil, out); err != nil {
				return "", err
			}
		}
		if err = inspect(); err != nil {
			return "", err
		}
	}
	path := filepath.Join(dir, r.ID+".yaml")
	if err := writeJSON(path, literalCompose(config)); err != nil {
		return "", err
	}
	return path, nil
}
