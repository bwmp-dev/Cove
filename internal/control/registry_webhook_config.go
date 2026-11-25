package control

import (
	"cove/internal/worker"
	"errors"
	"regexp"
)

var registryWebhookTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

type registryWebhookFilter struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
}

type registryWebhookConfiguration struct {
	Provider       string                  `json:"provider"`
	Enabled        bool                    `json:"enabled"`
	RegistryID     string                  `json:"registry_id"`
	RegistryServer string                  `json:"registry_server"`
	Filters        []registryWebhookFilter `json:"filters"`
}

func validateRegistryWebhook(config registryWebhookConfiguration) error {
	if config.Provider != "distribution" && config.Provider != "docker_hub" && config.Provider != "ghcr" {
		return errors.New("choose CNCF Distribution, Docker Hub or GHCR notifications")
	}
	if config.RegistryID == "" || len(config.Filters) == 0 || len(config.Filters) > 32 {
		return errors.New("select a connected registry and one through 32 repository filters")
	}
	seen := map[registryWebhookFilter]bool{}
	for _, filter := range config.Filters {
		if err := validateRegistryWebhookFilter(filter); err != nil {
			return err
		}
		if seen[filter] {
			return errors.New("each repository and tag filter must be unique")
		}
		seen[filter] = true
	}
	return nil
}

func validateRegistryWebhookFilter(filter registryWebhookFilter) error {
	if err := worker.ValidateRegistryRepository(filter.Repository); err != nil {
		return err
	}
	if filter.Tag != "*" && !registryWebhookTag.MatchString(filter.Tag) {
		return errors.New("use a registry tag of at most 128 characters, or * to watch every tag")
	}
	return nil
}

func registryPushMatches(config registryWebhookConfiguration, event registryPushEvent) bool {
	if !config.Enabled {
		return false
	}
	for _, filter := range config.Filters {
		if filter.Repository == event.Repository && (filter.Tag == "*" || filter.Tag == event.Tag) {
			return true
		}
	}
	return false
}
