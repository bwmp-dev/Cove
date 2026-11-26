package worker

import (
	"context"
	"errors"
)

func (e *Engine) removalBindSources(ctx context.Context, config map[string]any, containers []Container, bind func(string) error, volumes map[string]bool) error {
	services, ok := config["services"].(map[string]any)
	if !ok || len(services) == 0 {
		return errors.New("saved services are unavailable")
	}
	definitions, _ := config["volumes"].(map[string]any)
	for _, raw := range services {
		service, ok := raw.(map[string]any)
		if !ok {
			return errors.New("saved service is invalid")
		}
		if service["volumes"] == nil {
			continue
		}
		mounts, ok := service["volumes"].([]any)
		if !ok {
			return errors.New("saved mounts are unavailable")
		}
		for _, raw := range mounts {
			mount, ok := raw.(map[string]any)
			if !ok {
				return errors.New("saved mount has not been resolved")
			}
			source, _ := mount["source"].(string)
			switch mount["type"] {
			case "bind":
				if err := bind(source); err != nil {
					return err
				}
			case "volume":
				definition, _ := definitions[source].(map[string]any)
				name, _ := definition["name"].(string)
				if name == "" {
					return errors.New("saved volume name is unresolved")
				}
				volumes[name] = true
			case "tmpfs":
			default:
				return errors.New("saved mount type is unavailable")
			}
		}
	}
	for _, field := range []string{"configs", "secrets"} {
		definitions, _ := config[field].(map[string]any)
		for _, raw := range definitions {
			definition, ok := raw.(map[string]any)
			if !ok {
				return errors.New("saved file mount is unavailable")
			}
			if source, ok := definition["file"].(string); ok {
				if err := bind(source); err != nil {
					return err
				}
			}
		}
	}
	for _, container := range containers {
		var inspected struct {
			ID     string
			Mounts []struct{ Type, Source, Name string }
		}
		if err := e.imageJSON(ctx, nil, &inspected, "docker", "inspect", "--format", "{{json .}}", container.ID); err != nil || inspected.ID == "" {
			return errors.New("container mount inspection is unavailable")
		}
		for _, mount := range inspected.Mounts {
			switch mount.Type {
			case "bind":
				if err := bind(mount.Source); err != nil {
					return err
				}
			case "volume":
				volumes[mount.Name] = true
			}
		}
	}
	for name := range volumes {
		if !fileVolumeName.MatchString(name) {
			return errors.New("volume identity is unavailable")
		}
		var inspected struct {
			Driver  string
			Options map[string]string
		}
		if err := e.imageJSON(ctx, nil, &inspected, "docker", "volume", "inspect", "--format", "{{json .}}", name); err != nil || inspected.Driver != "local" {
			return errors.New("volume backing storage is unavailable")
		}
		if device := inspected.Options["device"]; device != "" {
			if err := bind(device); err != nil {
				return err
			}
		}
	}
	return nil
}
