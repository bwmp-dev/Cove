package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"
)

type projectConfiguration struct {
	APIVersion   string                     `json:"api_version"`
	Project      string                     `json:"project"`
	Variables    map[string]*string         `json:"variables,omitempty"`
	VariableKeys []string                   `json:"variable_keys,omitempty"`
	Policy       *deploymentPolicy          `json:"policy,omitempty"`
	Environments []configurationEnvironment `json:"environments"`
	Resources    []configurationResource    `json:"resources"`
}

type configurationEnvironment struct {
	Name         string             `json:"name"`
	Variables    map[string]*string `json:"variables,omitempty"`
	VariableKeys []string           `json:"variable_keys,omitempty"`
	Policy       *deploymentPolicy  `json:"policy,omitempty"`
}

type configurationResource struct {
	ID            string                 `json:"-"`
	Key           string                 `json:"key"`
	Environment   string                 `json:"environment,omitempty"`
	Configuration workloadInput          `json:"configuration"`
	VariableKeys  []string               `json:"variable_keys,omitempty"`
	Bindings      []configurationBinding `json:"bindings"`
	Revision      string                 `json:"revision,omitempty"`
}

type configurationBinding struct {
	SourceID string `json:"source_id,omitempty"`
	Name     string `json:"name"`
	Resource string `json:"resource"`
	Key      string `json:"key"`
}

var configurationKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func parseProjectConfiguration(content string) (projectConfiguration, error) {
	var manifest projectConfiguration
	if len(content) == 0 || len(content) > 2<<20 || strings.ContainsRune(content, 0) {
		return manifest, errors.New("cove.yaml must contain at most 2 MiB of text")
	}
	var value any
	decoder := yaml.NewDecoder(strings.NewReader(content))
	if err := decoder.Decode(&value); err != nil {
		return manifest, errors.New("invalid cove.yaml")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return manifest, errors.New("cove.yaml must contain one document")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return manifest, errors.New("cove.yaml requires string mapping keys")
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err = strict.Decode(&manifest); err != nil {
		return manifest, errors.New("cove.yaml contains an unknown field or an invalid value type")
	}
	return manifest, validateProjectConfiguration(manifest)
}

func validateProjectConfiguration(manifest projectConfiguration) error {
	validName := func(name string) bool { return strings.TrimSpace(name) != "" && len(name) <= 120 }
	if manifest.APIVersion != "cove/v1" || !validName(manifest.Project) || len(manifest.Environments) > 32 || len(manifest.Resources) > 128 {
		return errors.New("use api_version cove/v1, a project name and at most 32 environments and 128 resources")
	}
	validateVariables := func(values map[string]*string) error {
		if len(values) > 256 {
			return errors.New("at most 256 shared variables per scope")
		}
		plain := map[string]string{}
		for name, value := range values {
			plain[name] = ""
			if value != nil {
				plain[name] = *value
			}
		}
		return validateWorkload(workloadInput{Name: "variables", Compose: "services: {}", Environment: plain})
	}
	validatePolicy := func(policy *deploymentPolicy) error {
		if policy == nil {
			return nil
		}
		return validateSchedule(policy)
	}
	if err := validateVariables(manifest.Variables); err != nil {
		return err
	}
	if err := validatePolicy(manifest.Policy); err != nil {
		return err
	}
	environments := map[string]bool{"": true}
	for _, environment := range manifest.Environments {
		if !validName(environment.Name) || environments[environment.Name] {
			return errors.New("environment names must be unique and contain 1-120 characters")
		}
		environments[environment.Name] = true
		if err := validateVariables(environment.Variables); err != nil {
			return err
		}
		if err := validatePolicy(environment.Policy); err != nil {
			return err
		}
	}
	resources := map[string]bool{}
	for _, resource := range manifest.Resources {
		if !configurationKey.MatchString(resource.Key) || resources[resource.Key] || !environments[resource.Environment] {
			return errors.New("resource keys must be unique; each resource must select a declared environment or project resources")
		}
		resources[resource.Key] = true
		config := resource.Configuration
		if config.EnvironmentID != "" || config.ProjectID != "" || config.EnvironmentPatch != nil || config.NodeID == "" || config.Private != nil || config.Backup != nil {
			return errors.New("resource configuration requires node_id; declare placement outside configuration and manage infrastructure credentials through their dedicated settings")
		}
		if err := validateWorkload(config); err != nil {
			return err
		}
	}
	for _, resource := range manifest.Resources {
		seen := map[string]bool{}
		for _, binding := range resource.Bindings {
			if !variableName.MatchString(binding.Name) || len(binding.Name) > 128 || !variableName.MatchString(binding.Key) || len(binding.Key) > 128 || ((binding.SourceID == "") == (binding.Resource == "")) || binding.SourceID == "" && (!resources[binding.Resource] || binding.Resource == resource.Key) || seen[binding.Name] {
				return errors.New("variable bindings require unique variable names and a different declared resource")
			}
			seen[binding.Name] = true
		}
	}
	return nil
}

func (s *Server) configurationVersion(manifest projectConfiguration) string {
	ids := map[string]string{}
	for _, resource := range manifest.Resources {
		ids[resource.Key] = resource.ID
	}
	raw, _ := json.Marshal(struct {
		Manifest    projectConfiguration
		ResourceIDs map[string]string
	}{manifest, ids})
	digest := hmac.New(sha256.New, s.Key)
	digest.Write(raw)
	return hex.EncodeToString(digest.Sum(nil))
}

func (s *Server) readProjectConfiguration(ctx context.Context, tx pgx.Tx, team, project string) (projectConfiguration, error) {
	manifest := projectConfiguration{APIVersion: "cove/v1", Environments: []configurationEnvironment{}, Resources: []configurationResource{}}
	if err := tx.QueryRow(ctx, `SELECT name FROM projects WHERE id=$1 AND team_id=$2`, project, team).Scan(&manifest.Project); err != nil {
		return manifest, err
	}
	rows, err := tx.Query(ctx, `SELECT e.id,e.name FROM environments e WHERE e.project_id=$1 AND e.team_id=$2 AND NOT EXISTS(SELECT 1 FROM environment_previews p WHERE p.environment_id=e.id) ORDER BY e.name`, project, team)
	if err != nil {
		return manifest, err
	}
	environments := map[string]string{}
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			break
		}
		environments[id] = name
		manifest.Environments = append(manifest.Environments, configurationEnvironment{Name: name})
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return manifest, err
	}
	rows, err = tx.Query(ctx, `SELECT w.id,COALESCE(k.key,w.id),COALESCE(w.environment_id,''),w.configuration,COALESCE((SELECT o.result->>'revision' FROM operations o WHERE o.workload_id=w.id AND o.action IN ('deploy','rollback','adopt') AND o.state='succeeded' ORDER BY o.updated_at DESC LIMIT 1),'') FROM workloads w LEFT JOIN configuration_resource_keys k ON k.workload_id=w.id WHERE w.project_id=$1 AND w.team_id=$2 AND NOT EXISTS(SELECT 1 FROM previews p WHERE p.workload_id=w.id) AND NOT EXISTS(SELECT 1 FROM environment_preview_resources p WHERE p.resource_id=w.id) ORDER BY COALESCE(k.key,w.id)`, project, team)
	if err != nil {
		return manifest, err
	}
	keys := map[string]string{}
	for rows.Next() {
		var resource configurationResource
		var id, environment string
		var encrypted []byte
		if err = rows.Scan(&id, &resource.Key, &environment, &encrypted, &resource.Revision); err != nil {
			break
		}
		if err = s.unsealJSON(encrypted, &resource.Configuration); err != nil {
			break
		}
		resource.ID = id
		resource.Environment = environments[environment]
		resource.Configuration.ProjectID, resource.Configuration.EnvironmentID = "", ""
		keys[id] = resource.Key
		manifest.Resources = append(manifest.Resources, resource)
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return manifest, err
	}
	rows, err = tx.Query(ctx, `SELECT b.workload_id,b.name,b.source_id,b.source_key FROM resource_variable_bindings b JOIN workloads w ON w.id=b.workload_id WHERE w.project_id=$1 AND w.team_id=$2 ORDER BY b.workload_id,b.name`, project, team)
	if err != nil {
		return manifest, err
	}
	bindings := map[string][]configurationBinding{}
	for rows.Next() {
		var id, source string
		var binding configurationBinding
		if err = rows.Scan(&id, &binding.Name, &source, &binding.Key); err != nil {
			break
		}
		if keys[source] == "" {
			binding.SourceID = source
		} else {
			binding.Resource = keys[source]
		}
		bindings[id] = append(bindings[id], binding)
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return manifest, err
	}
	for id, key := range keys {
		for i := range manifest.Resources {
			if manifest.Resources[i].Key == key {
				manifest.Resources[i].Bindings = bindings[id]
			}
		}
	}
	rows, err = tx.Query(ctx, `SELECT COALESCE(environment_id,''),name,value FROM shared_variables WHERE project_id=$1 AND team_id=$2 ORDER BY environment_id NULLS FIRST,name`, project, team)
	if err != nil {
		return manifest, err
	}
	for rows.Next() {
		var environment, name string
		var encrypted []byte
		if err = rows.Scan(&environment, &name, &encrypted); err != nil {
			break
		}
		var value []byte
		value, err = unseal(s.Key, encrypted)
		if err != nil {
			break
		}
		text := string(value)
		if environment == "" {
			if manifest.Variables == nil {
				manifest.Variables = map[string]*string{}
			}
			manifest.Variables[name] = &text
		} else {
			for i := range manifest.Environments {
				item := &manifest.Environments[i]
				if item.Name == environments[environment] {
					if item.Variables == nil {
						item.Variables = map[string]*string{}
					}
					item.Variables[name] = &text
				}
			}
		}
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return manifest, err
	}
	rows, err = tx.Query(ctx, `SELECT COALESCE(environment_id,''),approval,freeze_from,freeze_until,window_start,window_end,timezone,recurring_windows,freezes,outside_window FROM deployment_policies WHERE team_id=$1 AND (project_id=$2 OR environment_id IN (SELECT id FROM environments WHERE project_id=$2))`, team, project)
	if err != nil {
		return manifest, err
	}
	for rows.Next() {
		var environment string
		policy := &deploymentPolicy{}
		if err = rows.Scan(&environment, &policy.Approval, &policy.FreezeFrom, &policy.FreezeUntil, &policy.WindowStart, &policy.WindowEnd, &policy.Timezone, &policy.Windows, &policy.Freezes, &policy.OutsideWindow); err != nil {
			break
		}
		if environment == "" {
			manifest.Policy = policy
		} else {
			for i := range manifest.Environments {
				if manifest.Environments[i].Name == environments[environment] {
					manifest.Environments[i].Policy = policy
				}
			}
		}
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	return manifest, err
}

func publicProjectConfiguration(manifest projectConfiguration, includeSecrets bool) projectConfiguration {
	keys := func(values map[string]*string) []string {
		names := make([]string, 0, len(values))
		for name := range values {
			names = append(names, name)
		}
		sort.Strings(names)
		return names
	}
	manifest.VariableKeys = keys(manifest.Variables)
	if !includeSecrets {
		manifest.Variables = nil
	}
	for i := range manifest.Environments {
		item := &manifest.Environments[i]
		item.VariableKeys = keys(item.Variables)
		if !includeSecrets {
			item.Variables = nil
		}
	}
	for i := range manifest.Resources {
		resource := &manifest.Resources[i]
		resource.VariableKeys = []string{}
		for name := range resource.Configuration.Environment {
			resource.VariableKeys = append(resource.VariableKeys, name)
		}
		sort.Strings(resource.VariableKeys)
		config := &resource.Configuration
		if !includeSecrets {
			config.Environment = nil
			if config.ComposeBundle != nil && (len(config.ComposeBundle.Files) > 0 || len(config.ComposeBundle.Assets) > 0) {
				config.ComposeBundle = nil
			}
		}
		if config.Source != nil {
			source := *config.Source
			source.Token, source.SSHPrivateKey = "", ""
			config.Source = &source
		}
		// Infrastructure credentials are never part of a portable manifest.
		config.Private, config.Backup = nil, nil
		for j := range config.Routes {
			config.Routes[j].Auth = ""
		}
	}
	return manifest
}
