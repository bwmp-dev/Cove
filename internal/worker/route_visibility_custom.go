package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

func validatePrivateCustomConfig(content string, privateRoutes bool) error {
	if !privateRoutes {
		return nil
	}
	var config map[string]any
	if yaml.Unmarshal([]byte(content), &config) != nil {
		return errors.New("private route reservations require readable custom configuration")
	}
	for _, section := range []string{"http", "tcp"} {
		group, _ := asMap(lookup(config, section))
		routers, _ := asMap(lookup(group, "routers"))
		for _, definition := range routers {
			body, ok := asMap(definition)
			if !ok {
				return errors.New("custom router configuration is unreadable")
			}
			if section == "http" {
				router := routerDefinition(section, "", body)
				if privateListenerDefault(router.EntryPoints) {
					return errors.New("private routes require custom HTTP routers to select explicit entrypoints outside managed private listeners")
				}
			}
			if err := validatePrivateSharedRouter(section, body); err != nil {
				return err
			}
		}
	}
	return nil
}

func validatePrivateSharedRouter(section string, body map[string]any) error {
	router := routerDefinition(section, "", body)
	if !sharedEntryPoints(router.EntryPoints) {
		return nil
	}
	if section == "tcp" {
		return errors.New("private hostnames cannot be protected while a custom TCP router shares public HTTP or HTTPS; select a dedicated TCP listener")
	}
	priority, err := privateCustomPriority(lookup(body, "priority"))
	if err != nil {
		return err
	}
	if priority >= int64(privateRouteDenialPriority) {
		return errors.New("custom HTTP priority conflicts with the reserved public denial for private hostnames")
	}
	return nil
}

func validatePrivateLiveData(data []byte, guards map[string]ingressRouter, owned map[string]ingressRouter) error {
	var raw struct {
		Routers    map[string]json.RawMessage `json:"routers"`
		TCPRouters map[string]json.RawMessage `json:"tcpRouters"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return errors.New("private route reservations require readable proxy runtime data")
	}
	for section, routers := range map[string]map[string]json.RawMessage{"http": raw.Routers, "tcp": raw.TCPRouters} {
		for name, definition := range routers {
			base, file := strings.CutSuffix(name, "@file")
			if !file {
				continue
			}
			decoder := json.NewDecoder(bytes.NewReader(definition))
			decoder.UseNumber()
			var body map[string]any
			if decoder.Decode(&body) != nil {
				return errors.New("private route reservation runtime object is unreadable")
			}
			if guard, owned := guards[base]; owned && section == "http" {
				priority, err := privateCustomPriority(lookup(body, "priority"))
				router := routerDefinition(section, base, body)
				service, _ := lookup(body, "service").(string)
				middlewares, _ := lookup(body, "middlewares").([]any)
				tls, hasTLS := lookup(body, "tls").(map[string]any)
				tlsMatches := lookup(body, "tls") == nil
				if guard.TLS != nil {
					tlsMatches = hasTLS && tls["certResolver"] == nil && tls["domains"] == nil && (tls["options"] == nil || tls["options"] == "default")
				}
				status, _ := lookup(body, "status").(string)
				if !tlsMatches || status != "" && status != "enabled" {
					return errors.New("an owned private-host public denial lost its TLS or active state")
				}

				if err != nil || priority != int64(guard.Priority) || router.Rule != guard.Rule || !slices.Equal(router.EntryPoints, guard.EntryPoints) || service != guard.Service || len(middlewares) != 0 {
					return errors.New("an owned private-host public denial was changed outside its route journal")
				}
				continue
			}
			if section == "http" {
				router := routerDefinition(section, base, body)
				if _, managed := owned[base]; !managed && privateListenerDefault(router.EntryPoints) {
					return errors.New("a custom HTTP router could bypass private route controls through an implicit or private listener")
				}
			}
			if err := validatePrivateSharedRouter(section, body); err != nil {
				return err
			}
		}
	}
	return nil
}

func privateCustomPriority(value any) (int64, error) {
	switch priority := value.(type) {
	case nil:
		return 0, nil
	case int:
		return int64(priority), nil
	case int64:
		return priority, nil
	case json.Number:
		if parsed, err := priority.Int64(); err == nil {
			return parsed, nil
		}
	case uint64:
		if priority <= math.MaxInt64 {
			return int64(priority), nil
		}
	case float64:
		if !math.IsNaN(priority) && !math.IsInf(priority, 0) && math.Trunc(priority) == priority && math.Abs(priority) < 1<<53 {
			return int64(priority), nil
		}
	}
	return 0, errors.New("custom HTTP priority must be a bounded integer while private hostnames are reserved")
}

func privateListenerDefault(entries []string) bool {
	if len(entries) == 0 {
		return true
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry, managedListenerPrefix) {
			return true
		}
	}
	return false
}
