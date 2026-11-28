package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var errSecretCLIUnavailable = errors.New("required provider CLI is unavailable on the control host")

func validOnePasswordServiceToken(token string) bool {
	if !strings.HasPrefix(token, "ops_") {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "ops_"))
	if err != nil {
		return false
	}
	var identity struct {
		Address string `json:"signInAddress"`
	}
	if json.Unmarshal(raw, &identity) != nil {
		return false
	}
	u, err := url.Parse("https://" + identity.Address)
	if err != nil || u.Host != identity.Address || u.User != nil || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, domain := range []string{"1password.com", "1password.eu", "1password.ca"} {
		if strings.HasSuffix(host, "."+domain) && !strings.ContainsAny(host, "/\\:@ \t\r\n") {
			return true
		}
	}
	return false
}

type secretCommandFunc func(context.Context, string, []string, []string) ([]byte, error)
type cliVariableSource struct {
	server  *Server
	config  secretProviderConfig
	command secretCommandFunc
}

type secretOutput struct{ data bytes.Buffer }

func (b *secretOutput) Write(value []byte) (int, error) {
	if b.data.Len()+len(value) > 2*1024*1024 {
		return 0, errors.New("secret response exceeds limit")
	}
	return b.data.Write(value)
}
func runSecretCommand(ctx context.Context, name string, args, environment []string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, errSecretCLIUnavailable
	}
	command := exec.CommandContext(ctx, name, args...)
	command.Env = environment
	command.Stdin = nil
	command.Stderr = io.Discard
	output := &secretOutput{}
	command.Stdout = output
	if err := command.Run(); err != nil {
		return nil, errSecretUnavailable
	}
	return output.data.Bytes(), nil
}

var secretUUID = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

func (p cliVariableSource) Resolve(ctx context.Context, ref string) (value string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	directory, err := os.MkdirTemp("", "cove-secret-")
	if err != nil {
		return "", errSecretUnavailable
	}
	defer func() {
		if cleanup := os.RemoveAll(directory); cleanup != nil && err == nil {
			value = ""
			err = errSecretUnavailable
		}
	}()
	environment := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		name = strings.ToUpper(name)
		if strings.HasPrefix(name, "OP_") || strings.HasPrefix(name, "BWS_") {
			continue
		}
		environment = append(environment, entry)
	}
	if p.server != nil {
		proxy, e := p.server.outboundConfig(ctx)
		if e != nil {
			return "", errSecretUnavailable
		}
		environment = append(environment, proxy.Env()...)
	}
	var name string
	var args []string
	switch p.config.Kind {
	case "onepassword-service":
		if !validOnePasswordServiceToken(p.config.Token) {
			return "", errSecretUnavailable
		}
		if _, err := referenceSegments(ref, 3, 3); err != nil {
			return "", errSecretUnavailable
		}
		name = "op"
		args = []string{"read", "--no-newline", "op://" + ref}
		environment = append(environment, "OP_SERVICE_ACCOUNT_TOKEN="+p.config.Token, "OP_CONFIG_DIR="+directory, "OP_CACHE=false", "OP_BIOMETRIC_UNLOCK_ENABLED=false")
	case "bitwarden":
		if !secretUUID.MatchString(ref) {
			return "", errSecretUnavailable
		}
		name = "bws"
		suffix := "com"
		if p.config.URL == "https://vault.bitwarden.eu" {
			suffix = "eu"
		}
		config := "[profiles.default]\nserver_api = \"https://api.bitwarden." + suffix + "\"\nserver_identity = \"https://identity.bitwarden." + suffix + "\"\nstate_opt_out = true\n"
		configPath := filepath.Join(directory, "config")
		if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
			return "", errSecretUnavailable
		}
		args = []string{"--config-file", configPath, "--profile", "default", "--output", "json", "secret", "get", ref}
		environment = append(environment, "BWS_ACCESS_TOKEN="+p.config.Token)
	default:
		return "", errSecretUnavailable
	}
	run := p.command
	if run == nil {
		run = runSecretCommand
	}
	raw, err := run(ctx, name, args, environment)
	if err != nil {
		if errors.Is(err, errSecretCLIUnavailable) {
			return "", errSecretCLIUnavailable
		}
		return "", errSecretUnavailable
	}
	if len(raw) > 2*1024*1024 {
		return "", errSecretUnavailable
	}
	if p.config.Kind == "onepassword-service" {
		return string(raw), nil
	}
	var secret struct {
		ID    string  `json:"id"`
		Value *string `json:"value"`
	}
	if len(raw) > 0 && raw[0] == '[' {
		var secrets []json.RawMessage
		if json.Unmarshal(raw, &secrets) != nil || len(secrets) != 1 {
			return "", errSecretUnavailable
		}
		raw = secrets[0]
	}
	if json.Unmarshal(raw, &secret) != nil || !strings.EqualFold(secret.ID, ref) || secret.Value == nil {
		return "", errSecretUnavailable
	}
	return *secret.Value, nil
}
