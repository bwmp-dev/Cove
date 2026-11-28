package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type ImportedCompose struct {
	Compose     string            `json:"compose"`
	Environment map[string]string `json:"environment"`
}

var importedVariable = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)

func validateImportedCompose(content string) error {
	if len(content) == 0 || len(content) > 1<<20 {
		return errors.New("import requires at most 1 MiB of Compose text")
	}
	var document map[string]any
	if yaml.Unmarshal([]byte(content), &document) != nil {
		return errors.New("invalid imported Compose document")
	}
	if document["include"] != nil {
		return errors.New("flatten Compose include files before importing")
	}
	services, ok := document["services"].(map[string]any)
	if !ok || len(services) == 0 || len(services) > 64 {
		return errors.New("import requires 1-64 Compose services")
	}
	for name, raw := range services {
		service, ok := raw.(map[string]any)
		if !ok || !serviceIdentifier.MatchString(name) {
			return errors.New("invalid imported service")
		}
		for _, field := range []string{"build", "extends", "env_file", "label_file", "develop", "provider"} {
			if service[field] != nil {
				return fmt.Errorf("flatten or remove %s before importing; supporting files are not included", field)
			}
		}
		if image, ok := service["image"].(string); !ok || image == "" {
			return errors.New("every imported service requires an image; build the image before importing")
		}
	}
	for _, kind := range []string{"configs", "secrets"} {
		if objects, ok := document[kind].(map[string]any); ok {
			for _, raw := range objects {
				if object, ok := raw.(map[string]any); ok && (object["file"] != nil || object["environment"] != nil) {
					return errors.New("replace file-backed and host-environment configs and secrets before importing")
				}
			}
		}
	}
	if volumes, ok := document["volumes"].(map[string]any); ok {
		for _, raw := range volumes {
			object, _ := raw.(map[string]any)
			if object["name"] == nil {
				return errors.New("declare each existing volume's physical name explicitly before importing; automatic project-prefixed volume names cannot be inferred from an API export")
			}
		}
	}
	return nil
}

func (e *Engine) PrepareImportedCompose(ctx context.Context, r Request) (result *ImportedCompose, resultErr error) {
	if err := validateImportedCompose(r.Compose); err != nil {
		return nil, err
	}
	if err := ValidateRequest(Request{ID: "import", Workload: "import", Action: "deploy", Compose: r.Compose, Environment: r.Environment}); err != nil {
		return nil, err
	}
	var document map[string]any
	if yaml.Unmarshal([]byte(r.Compose), &document) != nil {
		return nil, errors.New("invalid imported Compose")
	}
	encoded, _ := json.Marshal(document)
	// Exported files must not inherit credentials from the worker process environment.
	for _, match := range importedVariable.FindAllStringSubmatch(strings.ReplaceAll(string(encoded), "$$", ""), -1) {
		if _, ok := r.Environment[match[1]]; !ok {
			return nil, errors.New("supply every Compose interpolation variable explicitly in the exported environment, including variables with defaults")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp(e.Root, ".import-*")
	if err != nil {
		return nil, errors.New("could not prepare private import directory")
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			result = nil
			resultErr = errors.Join(resultErr, errors.New("could not clean up private import files"))
		}
	}()
	path := filepath.Join(dir, "compose.yaml")
	content, err := NormalizeComposeText(r.Compose)
	if err != nil || os.WriteFile(path, []byte(content), 0600) != nil {
		return nil, errors.New("could not prepare imported Compose")
	}
	env := []string{}
	for name, value := range r.Environment {
		env = append(env, name+"="+value)
	}
	out := &limitedBuffer{Limit: 1 << 20}
	if err = e.Run(ctx, dir, []string{"docker", "compose", "--project-name", "cove-import", "--env-file", os.DevNull, "--file", path, "--profile", "*", "config", "--format", "json"}, env, out); err != nil {
		return nil, errors.New("imported Compose could not be resolved; check required variables, missing files and Compose compatibility")
	}
	var resolved map[string]any
	if unmarshalCompose(out.data.Bytes(), &resolved) != nil {
		return nil, errors.New("invalid resolved import")
	}
	delete(resolved, "name")
	if networks, ok := resolved["networks"].(map[string]any); ok {
		for _, raw := range networks {
			if network, ok := raw.(map[string]any); ok {
				if name, ok := network["name"].(string); ok && strings.HasPrefix(name, "cove-import_") {
					delete(network, "name")
				}
			}
		}
	}
	services, _ := resolved["services"].(map[string]any)
	for _, raw := range services {
		if service, ok := raw.(map[string]any); ok {
			delete(service, "profiles")
			if volumes, ok := service["volumes"].([]any); ok {
				for _, raw := range volumes {
					if volume, ok := raw.(map[string]any); ok && volume["type"] == "bind" {
						if source, ok := volume["source"].(string); ok && (source == dir || strings.HasPrefix(source, dir+string(filepath.Separator))) {
							return nil, errors.New("replace relative bind mounts with verified absolute source paths before importing")
						}
					}
				}
			}
		}
	}
	return protectImportedCompose(resolved)
}

func protectImportedCompose(resolved map[string]any) (*ImportedCompose, error) {
	result := &ImportedCompose{Environment: map[string]string{}}
	var protect func(any) (any, error)
	protect = func(value any) (any, error) {
		switch v := value.(type) {
		case string:
			if strings.Contains(v, "{{") {
				return nil, errors.New("imported literal template expressions cannot be represented as Cove variable references; flatten them before importing")
			}
			if len(result.Environment) >= 256 || !validImportedValue(v) {
				return nil, errors.New("import exceeds 256 private fields or a field exceeds 64 KiB")
			}
			key := fmt.Sprintf("COVE_IMPORT_%03d", len(result.Environment)+1)
			result.Environment[key] = v
			return "${" + key + "}", nil
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				next, err := protect(v[key])
				if err != nil {
					return nil, err
				}
				v[key] = next
			}
		case []any:
			for i, entry := range v {
				next, err := protect(entry)
				if err != nil {
					return nil, err
				}
				v[i] = next
			}
		}
		return value, nil
	}
	protected, err := protect(resolved)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(protected)
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("protected import exceeds Compose size limit")
	}
	result.Compose = string(raw)
	return result, nil
}

func validImportedValue(value string) bool {
	return len(value) <= 65536 && !strings.ContainsRune(value, 0)
}
