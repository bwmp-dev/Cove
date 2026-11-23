package catalog

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

func Export(t Template, compose string) (Template, error) {
	var doc map[string]any
	if yaml.Unmarshal([]byte(compose), &doc) != nil {
		return t, errors.New("source Compose could not be parsed")
	}
	services, ok := doc["services"].(map[string]any)
	if !ok {
		return t, errors.New("source Compose requires services")
	}
	for key := range doc {
		if key != "services" && key != "volumes" && key != "configs" && key != "networks" && key != "version" && !strings.HasPrefix(key, "x-") {
			return t, fmt.Errorf("remove or convert source %s before exporting", key)
		}
	}
	t.SchemaVersion = 1
	t.Compose = ""
	t.Variables = []Variable{}
	t.Images = map[string]string{}
	t.Capabilities = []string{}
	t.Storage = []string{}
	fileVariables := map[string]bool{}
	for _, file := range t.FileSecrets {
		if !fileVariables[file.Variable] {
			t.Variables = append(t.Variables, Variable{Key: file.Variable, Label: "File secret " + file.Name, Type: "secret", Required: true})
			fileVariables[file.Variable] = true
		}
	}
	output := map[string]any{"services": services}
	sequence := 0
	variable := func(label string) string {
		sequence++
		key := fmt.Sprintf("SETTING_%d", sequence)
		for fileVariables[key] {
			sequence++
			key = fmt.Sprintf("SETTING_%d", sequence)
		}
		t.Variables = append(t.Variables, Variable{Key: key, Label: label, Type: "secret", Required: true})
		return "${" + key + ":?Enter a fresh template value}"
	}
	var replace func(any, string) any
	replace = func(value any, label string) any {
		switch v := value.(type) {
		case map[string]any:
			keys := []string{}
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				v[k] = replace(v[k], label+" "+k)
			}
			return v
		case []any:
			for i := range v {
				v[i] = replace(v[i], fmt.Sprintf("%s %d", label, i+1))
			}
			return v
		case nil:
			return variable(label)
		default:
			return variable(label)
		}
	}
	names := []string{}
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		service, ok := services[name].(map[string]any)
		if !ok {
			return t, errors.New("source services must be mappings")
		}
		image, ok := service["image"].(string)
		if !ok || !imageVersion.MatchString(image) {
			return t, errors.New("pin every source image to an explicit version before exporting")
		}
		t.Images[name] = image
		fields := []string{}
		for field := range service {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		for _, field := range fields {
			switch field {
			case "image", "restart", "depends_on", "volumes", "expose", "init", "read_only", "stop_grace_period", "cap_drop":
			case "ports", "labels":
				delete(service, field)
			case "environment":
				env := map[string]any{}
				switch v := service[field].(type) {
				case map[string]any:
					env = v
				case []any:
					for _, item := range v {
						key, _, _ := strings.Cut(fmt.Sprint(item), "=")
						env[key] = nil
					}
				default:
					return t, errors.New("source environment must be a mapping or list")
				}
				keys := []string{}
				for key := range env {
					if !variableName.MatchString(key) {
						return t, errors.New("export requires uppercase environment variable names")
					}
					keys = append(keys, key)
				}
				sort.Strings(keys)
				for _, key := range keys {
					env[key] = variable(name + " " + key)
				}
				service[field] = env
			case "command", "entrypoint", "hostname", "user", "working_dir":
				service[field] = replace(service[field], name+" "+field)
			case "healthcheck":
				check, ok := service[field].(map[string]any)
				if !ok {
					return t, errors.New("source healthcheck must be a mapping")
				}
				if test, ok := check["test"].([]any); ok {
					for i := range test {
						if i == 0 && (test[i] == "CMD" || test[i] == "CMD-SHELL" || test[i] == "NONE") {
							continue
						}
						test[i] = replace(test[i], name+" health command")
					}
				} else if check["test"] != nil {
					check["test"] = replace(check["test"], name+" health command")
				}
			default:
				return t, fmt.Errorf("source service field %s requires a reviewed custom repository template", field)
			}
		}
		if mounts, ok := service["volumes"].([]any); ok {
			for _, raw := range mounts {
				source := ""
				switch m := raw.(type) {
				case string:
					source, _, _ = strings.Cut(m, ":")
				case map[string]any:
					if m["type"] != "volume" {
						return t, errors.New("replace source host mounts with named volumes before exporting")
					}
					source, _ = m["source"].(string)
				}
				if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`).MatchString(source) {
					return t, errors.New("replace source host mounts with named volumes before exporting")
				}
			}
		}
	}
	if volumes, ok := doc["volumes"].(map[string]any); ok {
		output["volumes"] = volumes
		for name := range volumes {
			t.Storage = append(t.Storage, name)
		}
		sort.Strings(t.Storage)
	}
	if networks, ok := doc["networks"].(map[string]any); ok {
		output["networks"] = networks
	}
	if configs, ok := doc["configs"].(map[string]any); ok {
		names := []string{}
		for name := range configs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			raw := configs[name]
			config, ok := raw.(map[string]any)
			if !ok || config["content"] == nil || len(config) != 1 {
				return t, errors.New("exported Compose configs must use inline content")
			}
			config["content"] = variable("Configuration file " + name)
		}
		output["configs"] = configs
	}
	data, err := yaml.Marshal(output)
	if err != nil {
		return t, err
	}
	t.Compose = string(data)
	t.Notes = "Exported configuration contains no previous variable or command values. Enter fresh values when using it. Host ports and integration labels were removed; review routing and application initialization before deploying."
	t.Backup = "Review the application's consistency and database backup requirements before scheduling backups."
	if _, err = Validate(t); err != nil {
		return t, err
	}
	return t, nil
}
