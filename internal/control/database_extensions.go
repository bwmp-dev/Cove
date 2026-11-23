package control

import (
	"cove/internal/worker"
	"errors"
	"go.yaml.in/yaml/v3"
	"slices"
)

func configureDatabaseExtensions(config *workloadInput) error {
	if config.Database == nil || config.Database.Engine != "postgresql" {
		return nil
	}
	var compose map[string]any
	if err := yaml.Unmarshal([]byte(config.Compose), &compose); err != nil {
		return err
	}
	services, ok := compose["services"].(map[string]any)
	if !ok {
		return errors.New("database services unavailable")
	}
	service, ok := services[config.Database.Service].(map[string]any)
	if !ok {
		return errors.New("database service unavailable")
	}
	command := []any{"postgres"}
	if existing, ok := service["command"].([]any); ok {
		command = existing
	}
	filtered := []any{}
	for i := 0; i < len(command); i++ {
		if command[i] == "-c" && i+1 < len(command) && command[i+1] == "shared_preload_libraries=pg_stat_statements" {
			i++
			continue
		}
		filtered = append(filtered, command[i])
	}
	if slices.Contains(config.Database.Extensions, "pg_stat_statements") {
		filtered = append(filtered, "-c", "shared_preload_libraries=pg_stat_statements")
	}
	service["command"] = filtered
	encoded, err := yaml.Marshal(compose)
	if err != nil {
		return err
	}
	config.Compose = string(encoded)
	return nil
}

func changeDatabaseExtension(config *workloadInput, name string, enabled bool) error {
	if config.Database == nil || config.Database.Engine != "postgresql" || !worker.DatabaseExtension(name) {
		return errors.New("select a supported PostgreSQL extension")
	}
	if enabled && (name == "vector" && config.Database.Variant != "vector" || name == "postgis" && config.Database.Variant != "postgis") {
		return errors.New("this extension requires an isolated copy with its pinned image variant")
	}
	config.Database.Extensions = slices.DeleteFunc(config.Database.Extensions, func(value string) bool { return value == name })
	if name == "postgis" && !enabled {
		config.Database.Extensions = slices.DeleteFunc(config.Database.Extensions, func(value string) bool {
			return slices.Contains([]string{"postgis_topology", "postgis_tiger_geocoder", "fuzzystrmatch"}, value)
		})
	}
	if enabled {
		config.Database.Extensions = append(config.Database.Extensions, name)
	}
	return configureDatabaseExtensions(config)
}
