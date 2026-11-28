package worker

import (
	"context"
	"crypto/x509"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

type FileSecret struct {
	Name          string   `json:"name"`
	Variable      string   `json:"variable"`
	Services      []string `json:"services"`
	BuildServices []string `json:"build_services"`
}

var databaseCAName = regexp.MustCompile(`^cove_ca_[a-f0-9]{32}$`)

func validateDatabaseCAs(certificates map[string]string) error {
	if len(certificates) > 64 {
		return errors.New("too many database CA certificates")
	}
	total := 0
	for name, certificate := range certificates {
		total += len(certificate)
		roots := x509.NewCertPool()
		if !databaseCAName.MatchString(name) || len(certificate) > 65536 || !roots.AppendCertsFromPEM([]byte(certificate)) {
			return errors.New("invalid database CA certificate")
		}
	}
	if total > 256*1024 {
		return errors.New("database CA bundles exceed the request size limit")
	}
	return nil
}

func ValidateFileSecrets(secrets []FileSecret) error {
	if len(secrets) > 64 {
		return errors.New("at most 64 file secrets are allowed")
	}
	names := map[string]bool{}
	for _, secret := range secrets {
		if !environmentIdentifier.MatchString(secret.Name) || len(secret.Name) > 128 || !environmentIdentifier.MatchString(secret.Variable) || len(secret.Variable) > 128 || names[secret.Name] {
			return errors.New("file secrets require unique names and valid variable names")
		}
		names[secret.Name] = true
		if len(secret.Services)+len(secret.BuildServices) == 0 || len(secret.Services) > 64 || len(secret.BuildServices) > 64 {
			return errors.New("select runtime or build services for each secret")
		}
		for _, list := range [][]string{secret.Services, secret.BuildServices} {
			seen := map[string]bool{}
			for _, service := range list {
				if !serviceIdentifier.MatchString(service) || seen[service] {
					return errors.New("invalid or duplicate secret service")
				}
				seen[service] = true
			}
		}
	}
	return nil
}

func (e *Engine) prepareFileSecrets(ctx context.Context, r Request, dir string, args, env []string) (string, error) {
	output := &limitedBuffer{Limit: 2 << 20}
	if err := e.Run(ctx, dir, append(append([]string{}, args...), "config", "--format", "json"), env, output); err != nil {
		return "", err
	}
	var config map[string]any
	if err := unmarshalCompose(output.data.Bytes(), &config); err != nil {
		return "", err
	}
	services, ok := config["services"].(map[string]any)
	if !ok {
		return "", errors.New("Compose has no services")
	}
	r.Environment = maps.Clone(r.Environment)
	if r.Environment == nil {
		r.Environment = map[string]string{}
	}
	r.FileSecrets = slices.Clone(r.FileSecrets)
	for _, name := range slices.Sorted(maps.Keys(r.DatabaseCAs)) {
		for _, secret := range r.FileSecrets {
			if secret.Name == name {
				return "", errors.New("database CA trust file conflicts with a configured secret")
			}
		}
		r.Environment[name] = r.DatabaseCAs[name]
		r.FileSecrets = append(r.FileSecrets, FileSecret{Name: name, Variable: name, Services: slices.Sorted(maps.Keys(services))})
	}
	if err := validateFileSecretTargets(config, r); err != nil {
		return "", err
	}
	definitions, ok := config["secrets"].(map[string]any)
	if !ok {
		definitions = map[string]any{}
		config["secrets"] = definitions
	}
	root := filepath.Join(e.Root, "file-secrets", r.Workload, r.ID)
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	for _, secret := range r.FileSecrets {
		value := r.Environment[secret.Variable]
		path := filepath.Join(root, secret.Name)
		// The parent directory is private to the worker. Docker mounts only this file into selected services.
		if err := os.WriteFile(path, []byte(value), 0444); err != nil {
			return "", err
		}
		// The daemon's private umask otherwise leaves only root able to read the bind-mounted file.
		if err := os.Chmod(path, 0444); err != nil {
			return "", err
		}
		definitions[secret.Name] = map[string]any{"file": path}
		for _, name := range secret.Services {
			service := services[name].(map[string]any)
			list, _ := service["secrets"].([]any)
			service["secrets"] = append(list, map[string]any{"source": secret.Name, "target": secret.Name})
		}
		for _, name := range secret.BuildServices {
			service := services[name].(map[string]any)
			build := service["build"].(map[string]any)
			list, _ := build["secrets"].([]any)
			build["secrets"] = append(list, map[string]any{"source": secret.Name, "target": secret.Name})
		}
	}
	path := filepath.Join(dir, r.ID+".secrets.compose.json")
	return path, writeJSON(path, literalCompose(config))
}

func validateFileSecretTargets(config map[string]any, r Request) error {
	if err := ValidateFileSecrets(r.FileSecrets); err != nil {
		return err
	}
	services, _ := config["services"].(map[string]any)
	definitions, _ := config["secrets"].(map[string]any)
	for _, secret := range r.FileSecrets {
		if _, ok := r.Environment[secret.Variable]; !ok {
			return errors.New("file secret source variable is missing")
		}
		if _, ok := definitions[secret.Name]; ok {
			return errors.New("file secret name conflicts with Compose secrets")
		}
		for _, name := range secret.Services {
			if _, ok := services[name].(map[string]any); !ok {
				return errors.New("file secret runtime service does not exist")
			}
		}
		for _, name := range secret.BuildServices {
			service, ok := services[name].(map[string]any)
			if !ok {
				return errors.New("file secret build service does not exist")
			}
			if _, ok := service["build"].(map[string]any); !ok {
				return errors.New("file secret service has no build configuration")
			}
		}
	}
	return nil
}
