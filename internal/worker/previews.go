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

	"go.yaml.in/yaml/v3"
)

// Parse and constrain repository YAML before Compose can load includes or host files.
func previewCompose(path, root, workload string, environment map[string]string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("preview Compose exceeds 1 MiB")
	}
	var config map[string]any
	if err = yaml.Unmarshal(data, &config); err != nil {
		return nil, errors.New("invalid preview Compose YAML")
	}
	for key := range config {
		if key != "services" && key != "volumes" && key != "networks" && key != "name" && key != "version" && !strings.HasPrefix(key, "x-") {
			return nil, fmt.Errorf("previews do not support top-level %s", key)
		}
	}
	services, ok := config["services"].(map[string]any)
	if !ok || len(services) == 0 || len(services) > 4 {
		return nil, errors.New("previews require one to four services")
	}
	volumes := map[string]any{}
	if raw, exists := config["volumes"]; exists && raw != nil {
		declared, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("invalid preview volumes")
		}
		for name, options := range declared {
			if !serviceIdentifier.MatchString(name) {
				return nil, errors.New("invalid preview volume name")
			}
			if options != nil {
				m, ok := options.(map[string]any)
				if !ok || len(m) > 0 {
					return nil, errors.New("preview volumes cannot have external names, drivers, or options")
				}
			}
			volumes[name] = map[string]any{"name": "cove-preview-" + workload + "-" + name, "labels": map[string]string{"dev.cove.preview": workload}}
		}
	}
	if raw := config["networks"]; raw != nil {
		networks, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("invalid preview networks")
		}
		for _, options := range networks {
			if options != nil {
				m, ok := options.(map[string]any)
				if !ok || len(m) > 0 {
					return nil, errors.New("preview networks cannot use external names or options")
				}
			}
		}
	}
	allowed := map[string]bool{}
	for _, key := range strings.Fields("image build command entrypoint environment healthcheck depends_on expose working_dir user restart stop_grace_period stop_signal init read_only tmpfs volumes networks") {
		allowed[key] = true
	}
	for name, raw := range services {
		if !serviceIdentifier.MatchString(name) {
			return nil, errors.New("invalid preview service")
		}
		service, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("invalid preview service")
		}
		for key := range service {
			if !allowed[key] {
				return nil, fmt.Errorf("preview service %s does not support %s", name, key)
			}
		}
		expanded, err := expandPreviewValue(service, environment)
		if err != nil {
			return nil, err
		}
		service = expanded.(map[string]any)
		services[name] = service
		if err = previewExplicitValues(service["environment"]); err != nil {
			return nil, err
		}
		if raw := service["volumes"]; raw != nil {
			mounts, ok := raw.([]any)
			if !ok {
				return nil, errors.New("invalid preview mounts")
			}
			for i, mount := range mounts {
				var source, target string
				readOnly := false
				switch v := mount.(type) {
				case string:
					parts := strings.Split(v, ":")
					if len(parts) < 2 || len(parts) > 3 {
						return nil, errors.New("previews require declared named volumes")
					}
					source, target = parts[0], parts[1]
					if len(parts) == 3 {
						if parts[2] != "ro" && parts[2] != "rw" {
							return nil, errors.New("unsupported preview mount mode")
						}
						readOnly = parts[2] == "ro"
					}
				case map[string]any:
					for key := range v {
						if key != "type" && key != "source" && key != "target" && key != "read_only" {
							return nil, errors.New("unsupported preview volume option")
						}
					}
					if v["type"] != "volume" {
						return nil, errors.New("preview bind mounts are not allowed")
					}
					source, _ = v["source"].(string)
					target, _ = v["target"].(string)
					readOnly, _ = v["read_only"].(bool)
				default:
					return nil, errors.New("invalid preview mount")
				}
				if volumes[source] == nil || !strings.HasPrefix(target, "/") {
					return nil, errors.New("previews require declared named volumes and absolute container targets")
				}
				mounts[i] = map[string]any{"type": "volume", "source": source, "target": target, "read_only": readOnly}
			}
		}
		if raw := service["build"]; raw != nil {
			build := map[string]any{}
			switch v := raw.(type) {
			case string:
				build["context"] = v
			case map[string]any:
				build = v
			default:
				return nil, errors.New("invalid preview build")
			}
			for key := range build {
				if key != "context" && key != "dockerfile" && key != "target" && key != "args" {
					return nil, fmt.Errorf("preview builds do not support %s", key)
				}
			}
			if err = previewExplicitValues(build["args"]); err != nil {
				return nil, err
			}
			contextPath, _ := build["context"].(string)
			if contextPath == "" {
				contextPath = "."
			}
			contextPath, err = previewPath(filepath.Dir(path), contextPath, root)
			if err != nil {
				return nil, err
			}
			build["context"] = contextPath
			dockerfile, _ := build["dockerfile"].(string)
			if dockerfile == "" {
				dockerfile = "Dockerfile"
			}
			if _, err = previewPath(contextPath, dockerfile, root); err != nil {
				return nil, err
			}
			build["dockerfile"] = dockerfile
			service["build"] = build
			service["image"] = "cove-preview-" + workload + "-" + name
		}
		service["networks"] = []string{"default"}
		service["cpus"] = 1
		service["mem_limit"] = "512m"
		service["pids_limit"] = 256
		service["security_opt"] = []string{"no-new-privileges:true"}
		service["labels"] = map[string]string{"dev.cove.preview": workload}
	}
	return map[string]any{"services": services, "volumes": volumes, "networks": map[string]any{"default": map[string]any{"internal": true}}}, nil
}

func previewPath(base, path, root string) (string, error) {
	if filepath.IsAbs(path) || strings.Contains(path, ":") {
		return "", errors.New("preview build paths must be relative to the repository")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(base, path))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("preview build path escapes repository")
	}
	return resolved, nil
}

func previewExplicitValues(value any) error {
	switch v := value.(type) {
	case nil:
		return nil
	case map[string]any:
		for _, item := range v {
			if item == nil {
				return errors.New("preview environment and build arguments require explicit values")
			}
		}
	case []any:
		for _, item := range v {
			text, ok := item.(string)
			if !ok || !strings.Contains(text, "=") {
				return errors.New("preview environment and build arguments cannot inherit worker variables")
			}
		}
	default:
		return errors.New("invalid preview environment or build arguments")
	}
	return nil
}

func expandPreviewValue(value any, environment map[string]string) (any, error) {
	switch v := value.(type) {
	case string:
		var out strings.Builder
		for i := 0; i < len(v); {
			if v[i] != '$' {
				out.WriteByte(v[i])
				i++
				continue
			}
			if i+1 < len(v) && v[i+1] == '$' {
				out.WriteByte('$')
				i += 2
				continue
			}
			if i+1 >= len(v) || v[i+1] != '{' {
				return nil, errors.New("preview interpolation requires ${NAME}; escape shell variables with $$")
			}
			end := strings.IndexByte(v[i+2:], '}')
			if end < 0 {
				return nil, errors.New("invalid preview variable")
			}
			end += i + 2
			key := v[i+2 : end]
			replacement, ok := environment[key]
			if !environmentIdentifier.MatchString(key) || !ok {
				return nil, errors.New("preview variable is missing or uses unsupported interpolation")
			}
			out.WriteString(replacement)
			i = end + 1
		}
		return out.String(), nil
	case map[string]any:
		for key, item := range v {
			expanded, err := expandPreviewValue(item, environment)
			if err != nil {
				return nil, err
			}
			v[key] = expanded
		}
		return v, nil
	case []any:
		for i, item := range v {
			expanded, err := expandPreviewValue(item, environment)
			if err != nil {
				return nil, err
			}
			v[i] = expanded
		}
		return v, nil
	default:
		return v, nil
	}
}

func (e *Engine) preparePreview(r Request, dir, path string) (string, error) {
	root := filepath.Join(dir, "releases", r.ID)
	if r.Source == nil {
		root = dir
	}
	config, err := previewCompose(path, root, r.Workload, r.Environment)
	if err != nil {
		return "", err
	}
	// A repository symlink could otherwise enter a BuildKit context outside this checkout.
	if err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("preview repositories cannot contain symbolic links")
		}
		return nil
	}); err != nil {
		return "", err
	}
	target := filepath.Join(dir, r.ID+".preview.compose.json")
	if err = writeJSON(target, literalCompose(config)); err != nil {
		return "", err
	}
	if err = writeJSON(filepath.Join(dir, "preview.json"), r.Workload); err != nil {
		return "", err
	}
	return target, nil
}

func (e *Engine) removePreview(ctx context.Context, r Request, out io.Writer) error {
	dir := filepath.Join(e.Root, "workloads", r.Workload)
	raw, err := os.ReadFile(filepath.Join(dir, "preview.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var owner string
	if json.Unmarshal(raw, &owner) != nil || owner != r.Workload {
		return errors.New("invalid preview ownership")
	}
	containers, err := e.Containers(ctx, r.Workload)
	if err != nil {
		return err
	}
	if err = e.applyIngress(ctx, r.Workload, nil, out); err != nil {
		return err
	}
	for _, container := range containers {
		label, err := e.output(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "dev.cove.preview"}}`, container.ID)
		if err != nil || label != r.Workload {
			return errors.New("refusing to remove a non-preview container")
		}
		if err = e.Run(ctx, dir, []string{"docker", "rm", "--force", "--volumes", container.ID}, nil, out); err != nil {
			return err
		}
	}
	for _, kind := range []string{"network", "volume"} {
		label := "dev.cove.preview=" + r.Workload
		if kind == "network" {
			label = "com.docker.compose.project=" + e.composeProject(r.Workload)
		}
		ids, err := e.output(ctx, "docker", kind, "ls", "-q", "--filter", "label="+label)
		if err != nil {
			return err
		}
		for _, id := range strings.Fields(ids) {
			if err = e.Run(ctx, dir, []string{"docker", kind, "rm", id}, nil, out); err != nil {
				return err
			}
		}
	}
	// Keep the ownership marker until the controller acknowledges cleanup; retries are idempotent.
	for _, reference := range []string{"cove-preview-" + r.Workload + "-*", "cove-release/" + r.Workload + ":*"} {
		tags, err := e.output(ctx, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference="+reference)
		if err != nil {
			return err
		}
		for _, tag := range strings.Fields(tags) {
			if err = e.Run(ctx, dir, []string{"docker", "image", "rm", tag}, nil, out); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"releases", "release-states"} {
		if err = os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
