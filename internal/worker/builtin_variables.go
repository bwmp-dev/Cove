package worker

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

const (
	CoveFQDN        = "COVE_FQDN"
	CoveURL         = "COVE_URL"
	CoveCommitSHA   = "COVE_COMMIT_SHA"
	CoveBranch      = "COVE_BRANCH"
	CoveEnvironment = "COVE_ENVIRONMENT"
	CoveProject     = "COVE_PROJECT"
	CoveRelease     = "COVE_RELEASE"
)

type BuiltinVariable struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func BuiltinVariables() []BuiltinVariable {
	return []BuiltinVariable{
		{CoveFQDN, "The first public route's hostname, or empty when no route exists."},
		{CoveURL, "The first public route's HTTPS URL, or empty when no route exists."},
		{CoveCommitSHA, "The checked-out immutable Git commit, or empty for a Compose-only resource."},
		{CoveBranch, "The selected Git branch or tag, or empty for a Compose-only resource."},
		{CoveEnvironment, "The environment name, or empty for project resources."},
		{CoveProject, "The project name."},
		{CoveRelease, "The deployment operation ID."},
	}
}

func validateBuiltins(values map[string]string) error {
	allowed := map[string]bool{}
	for _, definition := range BuiltinVariables() {
		allowed[definition.Name] = true
	}
	for name, value := range values {
		if !allowed[name] || len(value) > 65536 || strings.ContainsRune(value, 0) {
			return errors.New("invalid builtin variable")
		}
	}
	return nil
}

func mergeBuiltinDefaults(environment, defaults map[string]string) (map[string]string, map[string]string) {
	merged := make(map[string]string, len(environment)+len(defaults))
	public := map[string]string{}
	for name, value := range defaults {
		merged[name] = value
		if _, overridden := environment[name]; !overridden {
			public[name] = value
		}
	}
	for name, value := range environment {
		merged[name] = value
	}
	return merged, public
}

func (e *Engine) prepareBuiltinEnvironment(ctx context.Context, r Request, dir string, args, env []string) (string, error) {
	if len(r.BuiltinDefaults) == 0 {
		return "", nil
	}
	resolved := &limitedBuffer{Limit: 2 * 1024 * 1024}
	if err := e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, resolved); err != nil {
		return "", errors.New("could not resolve Compose for builtin variables")
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(resolved.data.Bytes(), &config); err != nil {
		return "", err
	}
	var services map[string]map[string]json.RawMessage
	if err := json.Unmarshal(config["services"], &services); err != nil {
		return "", err
	}
	for _, service := range services {
		environment := map[string]*string{}
		if raw := service["environment"]; len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &environment); err != nil {
				return "", err
			}
		}
		for name := range r.BuiltinDefaults {
			if _, explicit := environment[name]; !explicit {
				value := strings.ReplaceAll(r.Environment[name], "$", "$$")
				environment[name] = &value
			}
		}
		raw, err := json.Marshal(environment)
		if err != nil {
			return "", err
		}
		service["environment"] = raw
		if raw := service["build"]; len(raw) > 0 && string(raw) != "null" {
			var build map[string]json.RawMessage
			if err := json.Unmarshal(raw, &build); err != nil {
				return "", err
			}
			arguments := map[string]*string{}
			if raw := build["args"]; len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &arguments); err != nil {
					return "", err
				}
			}
			for name := range r.BuiltinDefaults {
				if _, explicit := arguments[name]; !explicit {
					value := strings.ReplaceAll(r.Environment[name], "$", "$$")
					arguments[name] = &value
				}
			}
			raw, err := json.Marshal(arguments)
			if err != nil {
				return "", err
			}
			build["args"] = raw
			raw, err = json.Marshal(build)
			if err != nil {
				return "", err
			}
			service["build"] = raw
		}
	}
	raw, err := json.Marshal(services)
	if err != nil {
		return "", err
	}
	config["services"] = raw

	path := filepath.Join(dir, "builtin-"+r.ID+".json")
	return path, writeJSON(path, config)
}
