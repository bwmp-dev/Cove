package catalog

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"cove/internal/worker"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v3"
)

type Variable struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Type      string `json:"type"`
	Default   string `json:"default"`
	Required  bool   `json:"required"`
	Generated bool   `json:"generated"`
}

type Web struct {
	Route       bool   `json:"route"`
	Service     string `json:"service"`
	Port        int    `json:"port"`
	URLVariable string `json:"url_variable"`
}

type Resources struct {
	CPUs     float64 `json:"cpus"`
	MemoryMB int     `json:"memory_mb"`
	DiskMB   int     `json:"disk_mb"`
}

type Template struct {
	SchemaVersion int                 `json:"schema_version"`
	ID            string              `json:"id"`
	Name          string              `json:"name"`
	Description   string              `json:"description"`
	Category      string              `json:"category"`
	Source        string              `json:"source"`
	License       string              `json:"license"`
	Docs          string              `json:"docs"`
	Notes         string              `json:"notes"`
	Images        map[string]string   `json:"images"`
	Compose       string              `json:"compose"`
	Variables     []Variable          `json:"variables"`
	Web           *Web                `json:"web"`
	Storage       []string            `json:"storage"`
	Backup        string              `json:"backup"`
	Minimum       Resources           `json:"minimum"`
	Capabilities  []string            `json:"capabilities"`
	FileSecrets   []worker.FileSecret `json:"file_secrets"`
}

type Index struct {
	SchemaVersion int        `json:"schema_version"`
	Templates     []Template `json:"templates"`
}

var ID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var variableName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,95}$`)
var imageVersion = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./:_-]*(?:@sha256:[a-f0-9]{64}|:[a-zA-Z0-9_-]*[0-9]+\.[0-9]+[a-zA-Z0-9._-]*)$`)
var interpolation = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)
var generatedSecret = regexp.MustCompile(`^[a-f0-9]{64}$`)

func Decode(data []byte) (Index, error) {
	var index Index
	if len(data) > 8*1024*1024 {
		return index, errors.New("catalogue exceeds 8 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&index); err != nil {
		return index, errors.New("invalid versioned catalogue JSON")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return index, errors.New("one catalogue document required")
	}
	if index.SchemaVersion != 1 || len(index.Templates) == 0 || len(index.Templates) > 250 {
		return index, errors.New("schema_version 1 and 1–250 templates required")
	}
	seen := map[string]bool{}
	for _, t := range index.Templates {
		if seen[t.ID] {
			return index, errors.New("template IDs must be unique")
		}
		seen[t.ID] = true
		if _, err := Validate(t); err != nil {
			return index, fmt.Errorf("template %s: %w", t.ID, err)
		}
	}
	return index, nil
}

func HTTPS(value string) bool {
	u, e := url.Parse(value)
	return e == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == "" && len(value) <= 4096 && !strings.ContainsAny(value, "\r\n\x00")
}

func Values(t Template, supplied map[string]string, sample bool) (map[string]string, error) {
	values := map[string]string{}
	for _, v := range t.Variables {
		value, provided := supplied[v.Key]
		if !provided {
			value = v.Default
		}
		if v.Generated && (!provided || value == "") {
			data := make([]byte, 32)
			if _, err := rand.Read(data); err != nil {
				return nil, err
			}
			value = hex.EncodeToString(data)
		}
		if v.Generated && !generatedSecret.MatchString(value) {
			return nil, fmt.Errorf("%s requires 64 lowercase hexadecimal characters", v.Key)
		}
		if sample && value == "" {
			switch v.Type {
			case "integer":
				value = "1"
			case "boolean":
				value = "true"
			case "hostname":
				value = "app.example.test"
			case "email":
				value = "admin@example.test"
			case "url":
				value = "https://app.example.test"
			default:
				value = "template-check"
			}
		}
		if len(value) > 65536 || strings.ContainsRune(value, 0) || v.Required && strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s requires a valid value", v.Key)
		}
		if value != "" {
			switch v.Type {
			case "integer":
				if _, e := strconv.ParseInt(value, 10, 64); e != nil {
					return nil, fmt.Errorf("%s requires an integer", v.Key)
				}
			case "boolean":
				if value != "true" && value != "false" {
					return nil, fmt.Errorf("%s requires true or false", v.Key)
				}
			case "hostname":
				if !validHostname(value) {
					return nil, fmt.Errorf("%s requires a hostname", v.Key)
				}
			case "url":
				if !HTTPS(value) {
					return nil, fmt.Errorf("%s requires an HTTPS URL", v.Key)
				}
			case "email":
				address, e := mail.ParseAddress(value)
				if e != nil || address.Address != value {
					return nil, fmt.Errorf("%s requires an email address", v.Key)
				}
			}
		}
		values[v.Key] = value
	}
	for name := range supplied {
		if _, ok := values[name]; !ok {
			return nil, fmt.Errorf("unknown variable %s", name)
		}
	}
	return values, nil
}

func validHostname(value string) bool {
	if len(value) > 253 || !strings.Contains(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`).MatchString(label) {
			return false
		}
	}
	return true
}

func Draft(t Template, supplied map[string]string, hostname string, acknowledged bool) (worker.Request, error) {
	var r worker.Request
	if _, err := Validate(t); err != nil {
		return r, err
	}
	if len(t.Capabilities) > 0 && !acknowledged {
		return r, errors.New("explicit acknowledgement of template host access is required")
	}
	values, err := Values(t, supplied, false)
	if err != nil {
		return r, err
	}
	r = worker.Request{ID: "template-check", Workload: "template-check", Action: "deploy", Compose: t.Compose, Environment: values, FileSecrets: t.FileSecrets, Routes: []worker.Route{}}
	if t.Web != nil && t.Web.Route {
		hostname = strings.ToLower(strings.TrimSpace(hostname))
		if !validHostname(hostname) {
			return r, errors.New("valid route hostname required")
		}
		r.Routes = []worker.Route{{Domain: hostname, Service: t.Web.Service, Port: t.Web.Port}}
		if t.Web.URLVariable != "" {
			r.Environment[t.Web.URLVariable] = "https://" + hostname
		}
	}
	return r, worker.ValidateRequest(r)
}

func Validate(t Template) ([]string, error) {
	for _, value := range append([]string{t.Name, t.Description, t.Category, t.Notes, t.Backup}, t.Storage...) {
		if strings.ContainsRune(value, 0) {
			return nil, errors.New("template metadata cannot contain NUL characters")
		}
	}
	if t.SchemaVersion != 1 || !ID.MatchString(t.ID) || strings.TrimSpace(t.Name) == "" || len(t.Name) > 120 || t.Description == "" || len(t.Description) > 1000 || t.Category == "" || len(t.Category) > 80 || len(t.Notes) > 8000 || !HTTPS(t.Source) || !HTTPS(t.License) || !HTTPS(t.Docs) {
		return nil, errors.New("version 1, valid metadata and HTTPS source/licence/docs links required")
	}
	if len(t.Compose) == 0 || len(t.Compose) > 1024*1024 || len(t.Variables) > 128 || t.Minimum.CPUs <= 0 || t.Minimum.CPUs > 256 || t.Minimum.MemoryMB < 16 || t.Minimum.MemoryMB > 1048576 || t.Minimum.DiskMB < 0 {
		return nil, errors.New("bounded Compose, variables and positive minimum resources required")
	}
	if strings.TrimSpace(t.Backup) == "" || len(t.Backup) > 4000 || len(t.Storage) > 128 {
		return nil, errors.New("bounded storage and backup guidance required")
	}
	allowed := []string{"privileged", "host_network", "host_pid", "host_ipc", "host_userns", "host_uts", "host_cgroup", "host_mount", "docker_socket", "devices", "capabilities", "security_options"}
	seen := map[string]bool{}
	for _, c := range t.Capabilities {
		if !slices.Contains(allowed, c) || seen[c] {
			return nil, errors.New("invalid or duplicate host capability")
		}
		seen[c] = true
	}
	for _, v := range t.Variables {
		if !variableName.MatchString(v.Key) || v.Label == "" || len(v.Label) > 256 || strings.ContainsRune(v.Label, 0) || seen["var:"+v.Key] || !slices.Contains([]string{"string", "secret", "integer", "boolean", "hostname", "url", "email"}, v.Type) || v.Generated && v.Type != "secret" || v.Type == "secret" && v.Default != "" {
			return nil, errors.New("typed unique variables required; secret defaults are forbidden")
		}
		seen["var:"+v.Key] = true
	}
	if t.Web != nil && t.Web.URLVariable != "" && (!t.Web.Route || !variableName.MatchString(t.Web.URLVariable) || seen["var:"+t.Web.URLVariable]) {
		return nil, errors.New("web URL variable must be a separate valid variable on a routed service")
	}
	var doc map[string]any
	d := yaml.NewDecoder(strings.NewReader(t.Compose))
	if d.Decode(&doc) != nil || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("one valid Compose YAML document required")
	}
	if doc["include"] != nil || doc["name"] != nil {
		return nil, errors.New("external includes and fixed project names are forbidden")
	}
	services, ok := doc["services"].(map[string]any)
	if !ok || len(services) == 0 || len(services) > 32 {
		return nil, errors.New("1–32 Compose services required")
	}
	if len(t.Images) != len(services) {
		return nil, errors.New("every service requires a matching pinned image")
	}
	warnings := []string{}
	require := func(capability string) error {
		if !seen[capability] {
			return fmt.Errorf("explicit %s host capability declaration required", capability)
		}
		return nil
	}
	for name, raw := range services {
		s, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("service must be a mapping")
		}
		image, _ := s["image"].(string)
		if image != t.Images[name] || !imageVersion.MatchString(image) || strings.Contains(image, "${") {
			return nil, fmt.Errorf("service %s requires an explicit pinned image tag or digest", name)
		}
		for _, key := range []string{"build", "extends", "env_file", "label_file", "container_name", "volumes_from", "external_links", "use_api_socket", "develop", "post_start", "pre_stop", "credential_spec"} {
			if s[key] != nil {
				return nil, fmt.Errorf("template field %s is not self-contained", key)
			}
		}
		for key, capability := range map[string]string{"network_mode": "host_network", "pid": "host_pid", "ipc": "host_ipc", "userns_mode": "host_userns", "uts": "host_uts", "cgroup": "host_cgroup"} {
			v, _ := s[key].(string)
			if strings.Contains(v, "$") {
				return nil, errors.New("host namespace modes cannot use variables")
			}
			if v == "host" || strings.HasPrefix(v, "container:") {
				if err := require(capability); err != nil {
					return nil, err
				}
			}
		}
		if p, _ := s["privileged"].(bool); p {
			if err := require("privileged"); err != nil {
				return nil, err
			}
		}
		if value, ok := s["privileged"]; ok {
			if _, valid := value.(bool); !valid {
				return nil, errors.New("privileged must be an explicit boolean")
			}
		}
		for key, capability := range map[string]string{"devices": "devices", "device_cgroup_rules": "devices", "gpus": "devices", "runtime": "devices", "cap_add": "capabilities", "security_opt": "security_options", "sysctls": "security_options", "cgroup_parent": "security_options"} {
			if s[key] != nil {
				if err := require(capability); err != nil {
					return nil, err
				}
			}
		}
		if deploy, ok := s["deploy"].(map[string]any); ok {
			if resources, ok := deploy["resources"].(map[string]any); ok {
				if reservations, ok := resources["reservations"].(map[string]any); ok && reservations["devices"] != nil {
					if err := require("devices"); err != nil {
						return nil, err
					}
				}
			}
		}
		mounts, _ := s["volumes"].([]any)
		for _, mount := range mounts {
			var source, kind string
			switch m := mount.(type) {
			case string:
				parts := strings.Split(m, ":")
				if len(parts) < 2 {
					return nil, errors.New("anonymous volumes are forbidden")
				}
				source = parts[0]
				if strings.HasPrefix(source, "/") || strings.HasPrefix(source, ".") || strings.Contains(source, "$") {
					kind = "bind"
				}
			case map[string]any:
				source, _ = m["source"].(string)
				kind, _ = m["type"].(string)
				if strings.Contains(kind, "$") {
					return nil, errors.New("mount types cannot use variables")
				}
				if source == "" {
					return nil, errors.New("anonymous volumes are forbidden")
				}
			}
			if strings.Contains(source, "$") {
				return nil, errors.New("mount sources cannot use variables")
			}
			if kind == "bind" {
				capability := "host_mount"
				if strings.Contains(source, "docker.sock") || strings.Contains(source, "podman.sock") {
					capability = "docker_socket"
				}
				if err := require(capability); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, section := range []string{"configs", "secrets", "volumes", "networks"} {
		entries, _ := doc[section].(map[string]any)
		for _, raw := range entries {
			entry, _ := raw.(map[string]any)
			if entry["file"] != nil || entry["environment"] != nil || entry["external"] != nil || entry["name"] != nil || entry["driver_opts"] != nil {
				return nil, errors.New("template resources must be self-contained and resource-scoped")
			}
			if driver, _ := entry["driver"].(string); driver != "" && !(section == "volumes" && driver == "local") && !(section == "networks" && driver == "bridge") {
				return nil, errors.New("template resources require local volumes and bridge networks")
			}
		}
	}
	for _, match := range interpolation.FindAllStringSubmatch(strings.ReplaceAll(t.Compose, "$$", ""), -1) {
		if !seen["var:"+match[1]] && (t.Web == nil || t.Web.URLVariable != match[1]) {
			return nil, fmt.Errorf("undeclared template variable %s", match[1])
		}
	}
	values, err := Values(t, nil, true)
	if err != nil {
		return nil, err
	}
	if t.Web != nil && t.Web.URLVariable != "" {
		values[t.Web.URLVariable] = "https://app.example.test"
	}
	project, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{WorkingDir: ".", ConfigFiles: []types.ConfigFile{{Filename: "compose.yaml", Content: []byte(t.Compose)}}, Environment: types.Mapping(maps.Clone(values))}, func(o *loader.Options) {
		o.SetProjectName("cove-template", true)
		o.ResolvePaths = false
		o.SkipInclude = true
		o.SkipExtends = true
	})
	if err != nil {
		return nil, errors.New("Compose schema or service references are invalid")
	}
	for _, service := range project.Services {
		for _, mount := range service.Volumes {
			if mount.Type == "bind" {
				capability := "host_mount"
				if strings.Contains(mount.Source, "docker.sock") || strings.Contains(mount.Source, "podman.sock") {
					capability = "docker_socket"
				}
				if err := require(capability); err != nil {
					return nil, err
				}
			}
		}
		if service.Runtime != "" && service.Runtime != "runc" {
			if err := require("devices"); err != nil {
				return nil, err
			}
		}
	}
	if t.Web != nil {
		if _, ok := project.Services[t.Web.Service]; !ok || t.Web.Port < 1 || t.Web.Port > 65535 {
			return nil, errors.New("valid web service and port required")
		}
	}
	for _, file := range t.FileSecrets {
		if !seen["var:"+file.Variable] || len(file.BuildServices) > 0 {
			return nil, errors.New("runtime file secrets need declared variables; build secrets require published images")
		}
		for _, variable := range t.Variables {
			if variable.Key == file.Variable && variable.Type != "secret" {
				return nil, errors.New("file secrets require secret variables")
			}
		}
		for _, name := range file.Services {
			if _, ok := project.Services[name]; !ok {
				return nil, errors.New("file secret service is not defined")
			}
		}
	}
	if err = worker.ValidateRequest(worker.Request{ID: "template-check", Workload: "template-check", Action: "deploy", Compose: t.Compose, Environment: values, FileSecrets: t.FileSecrets}); err != nil {
		return nil, err
	}
	for _, capability := range t.Capabilities {
		warnings = append(warnings, "Requires "+capability+" access to the worker host; review before using this template.")
	}
	for _, service := range project.Services {
		if len(service.Ports) > 0 {
			warnings = append(warnings, "Publishes host ports; review bind addresses and conflicts before deploying.")
			break
		}
	}
	return warnings, nil
}
