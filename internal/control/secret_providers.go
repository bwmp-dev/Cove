package control

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type secretProviderConfig struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	URL         string `json:"url"`
	Token       string `json:"token,omitempty"`
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
}

func validateSecretProvider(c *secretProviderConfig) error {
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 120 || len(c.Token) < 8 || len(c.Token) > 8192 || strings.ContainsAny(c.Token+c.Name+c.Namespace+c.Project+c.Environment, "\r\n\x00") || len(c.Namespace) > 256 || len(c.Project) > 256 || len(c.Environment) > 256 {
		return errors.New("name, valid provider settings, and an access token are required")
	}
	switch c.Kind {
	case "vault", "openbao", "infisical", "onepassword-connect":
	case "doppler":
		c.URL = "https://api.doppler.com"
	case "onepassword-service":
		if !validOnePasswordServiceToken(c.Token) {
			return errors.New("1Password service account token must use a 1password.com, 1password.eu or 1password.ca sign-in address")
		}
		c.URL = ""
		return nil
	case "bitwarden":
		if c.URL == "" {
			c.URL = "https://vault.bitwarden.com"
		}
		if c.URL != "https://vault.bitwarden.com" && c.URL != "https://vault.bitwarden.eu" {
			return errors.New("Bitwarden requires the US or EU cloud URL")
		}
		return nil
	default:
		return errors.New("unsupported secret provider")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(c.URL) > 2048 || strings.ContainsAny(c.URL, "\r\n\x00") {
		return errors.New("provider URL must be HTTPS without credentials, query or fragment")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !publicWebhookIP(ip) {
		return errors.New("provider URL must resolve to a public address")
	}
	c.URL = strings.TrimRight(c.URL, "/")
	if c.Kind == "infisical" && (c.Project == "" || c.Environment == "") {
		return errors.New("Infisical requires a project ID and environment slug")
	}
	return nil
}

func secretProviderAvailability(kind string) bool {
	command := ""
	if kind == "onepassword-service" {
		command = "op"
	}
	if kind == "bitwarden" {
		command = "bws"
	}
	if command == "" {
		return true
	}
	_, err := exec.LookPath(command)
	return err == nil
}

func secretProviderPublic(id string, c secretProviderConfig) map[string]any {
	return map[string]any{"id": id, "name": c.Name, "kind": c.Kind, "url": c.URL, "project": c.Project, "environment": c.Environment, "namespace": c.Namespace, "available": secretProviderAvailability(c.Kind)}
}

func (s *Server) secretProviders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	level := 1
	if r.Method != "GET" {
		level = 3
	}
	if !s.requireAccess(w, r, "team", "", level) {
		return
	}
	team := actor(r).Team
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), `SELECT id,configuration FROM secret_providers WHERE team_id=$1 ORDER BY id`, team)
		if err != nil {
			http.Error(w, "secret providers unavailable", 503)
			return
		}
		defer rows.Close()
		result := []map[string]any{}
		for rows.Next() {
			var id string
			var raw []byte
			var c secretProviderConfig
			if rows.Scan(&id, &raw) != nil || s.unsealJSON(raw, &c) != nil {
				http.Error(w, "secret providers unavailable", 503)
				return
			}
			result = append(result, secretProviderPublic(id, c))
		}
		if rows.Err() != nil {
			http.Error(w, "secret providers unavailable", 503)
			return
		}
		respond(w, 200, result)
		return
	}
	id := r.PathValue("connection")
	if r.Method == "DELETE" {
		result, err := s.DB.Exec(r.Context(), `DELETE FROM secret_providers WHERE id=$1 AND team_id=$2`, id, team)
		if err != nil {
			http.Error(w, "could not delete secret provider", 503)
			return
		}
		if result.RowsAffected() == 0 {
			http.NotFound(w, r)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	var in secretProviderConfig
	if !decode(w, r, &in) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "secret providers unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if r.Method == "PUT" {
		var raw []byte
		var saved secretProviderConfig
		if err := tx.QueryRow(r.Context(), `SELECT configuration FROM secret_providers WHERE id=$1 AND team_id=$2 FOR UPDATE`, id, team).Scan(&raw); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.NotFound(w, r)
			} else {
				http.Error(w, "secret provider unavailable", 503)
			}
			return
		}
		if s.unsealJSON(raw, &saved) != nil {
			http.Error(w, "secret provider unavailable", 503)
			return
		}
		if in.Kind != saved.Kind || strings.TrimRight(in.URL, "/") != saved.URL || in.Project != saved.Project || in.Environment != saved.Environment || in.Namespace != saved.Namespace {
			http.Error(w, "create a new connection to change provider settings", 409)
			return
		}
		if in.Token == "" {
			in.Token = saved.Token
		}
	}
	if err = validateSecretProvider(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// CLI providers require their vendor's encryption implementation on the control host.
	if !secretProviderAvailability(in.Kind) {
		http.Error(w, "install the provider CLI (op or bws) on the control host before configuring this provider", 409)
		return
	}
	raw, err := s.sealPreview(in)
	if err != nil {
		http.Error(w, "could not protect secret provider", 500)
		return
	}
	status := 200
	if id == "" {
		id = resourceID()
		status = 201
		_, err = tx.Exec(r.Context(), `INSERT INTO secret_providers(id,team_id,configuration) VALUES($1,$2,$3)`, id, team, raw)
	} else {
		_, err = tx.Exec(r.Context(), `UPDATE secret_providers SET configuration=$2 WHERE id=$1`, id, raw)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not save secret provider", 503)
		return
	}
	respond(w, status, secretProviderPublic(id, in))
}

type httpVariableSource struct {
	server *Server
	config secretProviderConfig
}

func (p httpVariableSource) read(ctx context.Context, path string, query url.Values, out any) error {
	c := p.config
	u, err := url.Parse(c.URL + path)
	if err != nil {
		return errSecretUnavailable
	}
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return errSecretUnavailable
	}
	req.Header.Set("Accept", "application/json")
	if c.Kind == "vault" || c.Kind == "openbao" {
		req.Header.Set("X-Vault-Token", c.Token)
		if c.Namespace != "" {
			req.Header.Set("X-Vault-Namespace", c.Namespace)
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	transport := publicWebhookTransport()
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 20 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if p.server.OAuthClient != nil && p.server.OAuthClient.Transport != nil {
		client.Transport = p.server.OAuthClient.Transport
	}
	response, err := client.Do(req)
	if err != nil {
		return errSecretUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errSecretUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(raw) > 2*1024*1024 || json.Unmarshal(raw, out) != nil {
		return errSecretUnavailable
	}
	return nil
}

func referenceSegments(ref string, min, max int) ([]string, error) {
	parts := strings.Split(ref, "/")
	if len(parts) < min || len(parts) > max {
		return nil, errSecretUnavailable
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, "%?#\\\r\n\x00") {
			return nil, errSecretUnavailable
		}
	}
	return parts, nil
}

func (p httpVariableSource) Resolve(ctx context.Context, ref string) (string, error) {
	c := p.config
	switch c.Kind {
	case "vault", "openbao":
		location, field, ok := strings.Cut(ref, "#")
		parts, err := referenceSegments(location, 2, 32)
		if !ok || field == "" || err != nil {
			return "", errSecretUnavailable
		}
		for i := range parts {
			parts[i] = url.PathEscape(parts[i])
		}
		var body struct {
			Data struct {
				Data map[string]any `json:"data"`
			} `json:"data"`
		}
		if err = p.read(ctx, "/v1/"+parts[0]+"/data/"+strings.Join(parts[1:], "/"), nil, &body); err != nil {
			return "", err
		}
		value, ok := body.Data.Data[field].(string)
		if !ok {
			return "", errSecretUnavailable
		}
		return value, nil
	case "infisical":
		parts, err := referenceSegments(strings.TrimPrefix(ref, "/"), 1, 32)
		if err != nil {
			return "", err
		}
		folder := "/" + strings.Join(parts[:len(parts)-1], "/")
		var body struct {
			Secret struct {
				Value  *string `json:"secretValue"`
				Hidden bool    `json:"secretValueHidden"`
			} `json:"secret"`
		}
		q := url.Values{"projectId": {c.Project}, "environment": {c.Environment}, "secretPath": {folder}, "viewSecretValue": {"true"}}
		if err = p.read(ctx, "/api/v4/secrets/"+url.PathEscape(parts[len(parts)-1]), q, &body); err != nil {
			return "", err
		}
		if body.Secret.Value == nil || body.Secret.Hidden {
			return "", errSecretUnavailable
		}
		return *body.Secret.Value, nil
	case "doppler":
		if !variableName.MatchString(ref) {
			return "", errSecretUnavailable
		}
		q := url.Values{"format": {"json"}, "include_dynamic_secrets": {"false"}, "secrets": {ref}}
		if c.Project != "" {
			q.Set("project", c.Project)
		}
		if c.Environment != "" {
			q.Set("config", c.Environment)
		}
		var body map[string]string
		if err := p.read(ctx, "/v3/configs/config/secrets/download", q, &body); err != nil {
			return "", err
		}
		value, ok := body[ref]
		if !ok {
			return "", errSecretUnavailable
		}
		return value, nil
	case "onepassword-connect":
		parts, err := referenceSegments(ref, 3, 3)
		if err != nil {
			return "", err
		}
		var body struct {
			Fields []struct {
				ID    string  `json:"id"`
				Value *string `json:"value"`
			} `json:"fields"`
		}
		if err = p.read(ctx, "/v1/vaults/"+url.PathEscape(parts[0])+"/items/"+url.PathEscape(parts[1]), nil, &body); err != nil {
			return "", err
		}
		for _, field := range body.Fields {
			if field.ID == parts[2] && field.Value != nil {
				return *field.Value, nil
			}
		}
	}
	return "", errSecretUnavailable
}

var externalSecretReference = regexp.MustCompile(`\{\{secret\.([a-f0-9]{32}):([^{}\r\n]+)\}\}`)

func (s *Server) resolveExternalVariables(ctx context.Context, workload string, values map[string]string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var team string
	if err := s.DB.QueryRow(ctx, `SELECT team_id FROM workloads WHERE id=$1`, workload).Scan(&team); err != nil {
		return nil, err
	}
	sources := map[string]variableSource{}
	cache := map[string]string{}
	result := map[string]string{}
	for name, input := range values {
		if strings.Contains(externalSecretReference.ReplaceAllString(input, ""), "{{secret.") {
			return nil, &variableResolutionError{name, "invalid external secret reference"}
		}
		var failure error
		size := len(input)
		output := externalSecretReference.ReplaceAllStringFunc(input, func(match string) string {
			if failure != nil {
				return ""
			}
			if value, ok := cache[match]; ok {
				size += len(value) - len(match)
				if size > 65536 {
					failure = errSecretUnavailable
					return ""
				}
				return value
			}
			parts := externalSecretReference.FindStringSubmatch(match)
			if len(parts[2]) > 2048 {
				failure = errSecretUnavailable
				return ""
			}
			id := parts[1]
			source := sources[id]
			if source == nil {
				var raw []byte
				var c secretProviderConfig
				if s.DB.QueryRow(ctx, `SELECT configuration FROM secret_providers WHERE id=$1 AND team_id=$2`, id, team).Scan(&raw) != nil || s.unsealJSON(raw, &c) != nil {
					failure = errSecretUnavailable
					return ""
				}
				if c.Kind == "bitwarden" || c.Kind == "onepassword-service" {
					source = cliVariableSource{config: c, command: s.secretCommand}
				} else {
					source = httpVariableSource{server: s, config: c}
				}
				sources[id] = source
			}
			value, err := source.Resolve(ctx, parts[2])
			if err != nil || !validSecretValue(value) {
				if errors.Is(err, errSecretCLIUnavailable) {
					failure = errSecretCLIUnavailable
					return ""
				}
				failure = errSecretUnavailable
				return ""
			}
			size += len(value) - len(match)
			if size > 65536 {
				failure = errSecretUnavailable
				return ""
			}
			cache[match] = value
			return value
		})
		if failure != nil || !validSecretValue(output) {
			reason := errSecretUnavailable.Error()
			if errors.Is(failure, errSecretCLIUnavailable) {
				reason = errSecretCLIUnavailable.Error()
			}
			return nil, &variableResolutionError{name, reason}
		}
		result[name] = output
	}
	return result, nil
}
