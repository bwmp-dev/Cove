package worker

import (
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const (
	MaxCustomIngressBytes = 256 * 1024
	maxCustomIngressFiles = 64
)

var customIngressName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,95}\.(?:yaml|yml)$`)

// ValidIngressFileName reports whether name can identify a file in the proxy's dynamic directory.
func ValidIngressFileName(name string) bool { return customIngressName.MatchString(name) }

// Generated files and the cove- namespace belong to Cove, so custom configuration never replaces them.
func protectedIngressFile(name string) bool {
	return name == "routes.yaml" || name == "panel.yaml" || strings.HasPrefix(name, "cove-")
}

var customGroups = map[string][]string{
	"http": {"routers", "services", "middlewares", "serversTransports"},
	"tcp":  {"routers", "services", "middlewares", "serversTransports"},
	"udp":  {"routers", "services"},
	"tls":  {"certificates", "options", "stores"},
}

type customObject struct {
	Section string
	Group   string
	Name    string
}

func (o customObject) key() string { return o.Section + "." + o.Group + "/" + o.Name }

func (o customObject) label() string {
	return fmt.Sprintf("%s %s %q", strings.ToUpper(o.Section), strings.TrimSuffix(o.Group, "s"), o.Name)
}

type customRouter struct {
	Section      string
	Name         string
	Rule         string
	EntryPoints  []string
	CertResolver string
}

type customIngress struct {
	Objects   []customObject
	Routers   []customRouter
	CertFiles []string
	Secrets   []string
	TLS       bool
}

// parseCustomIngress accepts literal Traefik dynamic configuration that Cove can manage.
func parseCustomIngress(data string) (customIngress, error) {
	if len(data) > MaxCustomIngressBytes {
		return customIngress{}, fmt.Errorf("custom proxy configuration exceeds %d KiB", MaxCustomIngressBytes/1024)
	}
	if !utf8.ValidString(data) {
		return customIngress{}, errors.New("custom proxy configuration must be UTF-8 text")
	}
	for _, r := range data {
		if (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || r == 0x7f {
			return customIngress{}, errors.New("custom proxy configuration must not contain control characters")
		}
	}
	// Traefik renders file provider content as a Go template, including comments.
	if strings.Contains(data, "{{") {
		return customIngress{}, errors.New("templates are not supported; use literal values")
	}
	return decodeCustomIngress(data)
}

func decodeCustomIngress(data string) (customIngress, error) {
	var parsed customIngress
	decoder := yaml.NewDecoder(strings.NewReader(data))
	var config map[string]any
	if err := decoder.Decode(&config); err != nil && err != io.EOF {
		return parsed, errors.New("invalid YAML: " + strings.TrimPrefix(err.Error(), "yaml: "))
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return parsed, errors.New("custom proxy configuration must contain one YAML document")
	}
	for section, value := range config {
		canonical, groups := customSection(section)
		if groups == nil {
			return parsed, fmt.Errorf("%q is not dynamic configuration; only http, tcp, udp and tls are supported. Entry points, providers and certificate resolvers are static proxy settings", section)
		}
		if value == nil {
			continue
		}
		sectionMap, ok := asMap(value)
		if !ok {
			return parsed, fmt.Errorf("%s must be a mapping", canonical)
		}
		for group, content := range sectionMap {
			index := slices.IndexFunc(groups, func(name string) bool { return strings.EqualFold(name, group) })
			if index < 0 {
				return parsed, fmt.Errorf("%s.%s is not a supported Traefik configuration group", canonical, group)
			}
			group = groups[index]
			if canonical == "tls" {
				parsed.TLS = true
			}
			if canonical == "tls" && group == "certificates" {
				if err := parsed.addCertificates(content); err != nil {
					return parsed, err
				}
				continue
			}
			if content == nil {
				continue
			}
			objects, ok := asMap(content)
			if !ok {
				return parsed, fmt.Errorf("%s.%s must map names to definitions", canonical, group)
			}
			for name, definition := range objects {
				if name == "" || len(name) > 200 || strings.ContainsAny(name, "@\t\r\n") {
					return parsed, fmt.Errorf("%s.%s contains an invalid name; names cannot contain @", canonical, group)
				}
				body, ok := asMap(definition)
				if !ok {
					return parsed, fmt.Errorf("%s.%s.%s must be a mapping", canonical, group, name)
				}
				parsed.Objects = append(parsed.Objects, customObject{Section: canonical, Group: group, Name: name})
				if group == "routers" {
					parsed.Routers = append(parsed.Routers, routerDefinition(canonical, name, body))
				}
				collectSecrets(body, &parsed.Secrets)
			}
		}
	}
	sort.Slice(parsed.Objects, func(i, j int) bool { return parsed.Objects[i].key() < parsed.Objects[j].key() })
	sort.Slice(parsed.Routers, func(i, j int) bool {
		return parsed.Routers[i].Section+parsed.Routers[i].Name < parsed.Routers[j].Section+parsed.Routers[j].Name
	})
	return parsed, nil
}

func customSection(name string) (string, []string) {
	for section, groups := range customGroups {
		if strings.EqualFold(section, name) {
			return section, groups
		}
	}
	return "", nil
}

func (c *customIngress) addCertificates(content any) error {
	if content == nil {
		return nil
	}
	list, ok := content.([]any)
	if !ok {
		return errors.New("tls.certificates must be a list")
	}
	for _, item := range list {
		certificate, ok := asMap(item)
		if !ok {
			return errors.New("each tls.certificates entry must be a mapping")
		}
		for _, field := range []string{"certFile", "keyFile"} {
			value, _ := lookup(certificate, field).(string)
			if strings.Contains(value, "-----BEGIN") {
				c.Secrets = append(c.Secrets, value)
			} else if value != "" && !slices.Contains(c.CertFiles, value) {
				c.CertFiles = append(c.CertFiles, value)
			}
		}
	}
	return nil
}

func routerDefinition(section, name string, body map[string]any) customRouter {
	router := customRouter{Section: section, Name: name}
	router.Rule, _ = lookup(body, "rule").(string)
	if entryPoints, ok := lookup(body, "entryPoints").([]any); ok {
		for _, entryPoint := range entryPoints {
			if value, ok := entryPoint.(string); ok {
				router.EntryPoints = append(router.EntryPoints, value)
			}
		}
	}
	if tls, ok := asMap(lookup(body, "tls")); ok {
		router.CertResolver, _ = lookup(tls, "certResolver").(string)
	}
	return router
}

var secretField = regexp.MustCompile(`(?i)user|pass|secret|token|key|credential|auth|cookie`)

// collectSecrets records values that must never appear in validation messages returned to callers.
func collectSecrets(value any, secrets *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if secretField.MatchString(key) && !strings.EqualFold(key, "certFile") && !strings.EqualFold(key, "keyFile") {
				collectSecretValues(nested, secrets)
			}
			collectSecrets(nested, secrets)
		}
	case []any:
		for _, nested := range typed {
			collectSecrets(nested, secrets)
		}
	}
}

func collectSecretValues(value any, secrets *[]string) {
	switch typed := value.(type) {
	case string:
		if len(typed) >= 4 {
			*secrets = append(*secrets, typed)
		}
	case []any:
		for _, nested := range typed {
			collectSecretValues(nested, secrets)
		}
	}
}

func asMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case map[any]any:
		converted := make(map[string]any, len(typed))
		for key, nested := range typed {
			name, ok := key.(string)
			if !ok {
				return nil, false
			}
			converted[name] = nested
		}
		return converted, true
	}
	return nil, false
}

// Traefik matches configuration keys case-insensitively.
func lookup(values map[string]any, key string) any {
	if value, ok := values[key]; ok {
		return value
	}
	for name, value := range values {
		if strings.EqualFold(name, key) {
			return value
		}
	}
	return nil
}

type hostClaim struct {
	Kind    string
	Value   string
	Negated bool
}

var hostMatchers = []string{"HostSNIRegexp", "HostSNI", "HostRegexp", "Host"}

// ruleHosts returns the hostname matchers in a Traefik rule.
func ruleHosts(rule string) []hostClaim {
	var claims []hostClaim
	for i := 0; i < len(rule); i++ {
		if i > 0 && isRuleIdentifier(rule[i-1]) {
			continue
		}
		kind := ""
		for _, candidate := range hostMatchers {
			if strings.HasPrefix(rule[i:], candidate) {
				kind = candidate
				break
			}
		}
		if kind == "" {
			continue
		}
		j := i + len(kind)
		for j < len(rule) && rule[j] == ' ' {
			j++
		}
		if j >= len(rule) || rule[j] != '(' {
			continue
		}
		before := strings.TrimRight(rule[:i], " ")
		negated := strings.HasSuffix(before, "!")
		for j++; j < len(rule) && rule[j] != ')'; {
			switch rule[j] {
			case '`':
				end := strings.IndexByte(rule[j+1:], '`')
				if end < 0 {
					return claims
				}
				claims = append(claims, hostClaim{Kind: kind, Value: rule[j+1 : j+1+end], Negated: negated})
				j += end + 2
			case '"':
				end := j + 1
				for end < len(rule) && rule[end] != '"' {
					if rule[end] == '\\' {
						end++
					}
					end++
				}
				if end >= len(rule) {
					return claims
				}
				value, err := strconv.Unquote(rule[j : end+1])
				if err != nil {
					value = rule[j+1 : end]
				}
				claims = append(claims, hostClaim{Kind: kind, Value: value, Negated: negated})
				j = end + 1
			default:
				j++
			}
		}
		i = j
	}
	return claims
}

func isRuleIdentifier(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

type ingressIssue struct {
	Key     string
	Message string
}

type customIngressContext struct {
	EntryPoints map[string]bool
	// Resolvers is nil when the proxy's certificate resolvers could not be determined.
	Resolvers map[string]bool
	Domains   []string
	// Others maps object keys defined by other files in the dynamic directory to that file.
	Others map[string]string
}

// customIngressIssues reports conflicts with Cove-managed routing and the proxy's static configuration.
// Issue keys let callers distinguish problems a change introduces from ones its file already had.
func customIngressIssues(config customIngress, c customIngressContext) ([]ingressIssue, []string) {
	var issues []ingressIssue
	var warnings []string
	for _, object := range config.Objects {
		if strings.HasPrefix(object.Name, "cove-") {
			issues = append(issues, ingressIssue{"reserved/" + object.key(), object.label() + " uses the reserved cove- prefix."})
		}
		if file := c.Others[object.key()]; file != "" {
			issues = append(issues, ingressIssue{"duplicate/" + object.key(), fmt.Sprintf("%s is also defined in %s. Traefik silently keeps only one definition; rename one of them.", object.label(), file)})
		}
	}
	for _, file := range config.CertFiles {
		if !certificatePath(file) {
			issues = append(issues, ingressIssue{"certificate/" + file, fmt.Sprintf("Certificate file %q must be an absolute path inside the proxy volume under /data/, outside /data/dynamic/, Cove's history and ACME or credential files. Inline PEM is also accepted.", file)})
		}
	}
	for _, router := range config.Routers {
		for _, entryPoint := range router.EntryPoints {
			if !c.EntryPoints[entryPoint] {
				issues = append(issues, ingressIssue{"entrypoint/" + entryPoint, fmt.Sprintf("Entry point %q is not defined in the proxy's static configuration. Cove does not add entry points or published ports; those require recreating the proxy with new static settings.", entryPoint)})
			}
		}
		if router.CertResolver != "" && c.Resolvers != nil && !c.Resolvers[router.CertResolver] {
			issues = append(issues, ingressIssue{"resolver/" + router.CertResolver, fmt.Sprintf("Certificate resolver %q is not configured on the proxy. Available resolvers are static settings: %s.", router.CertResolver, strings.Join(sortedKeys(c.Resolvers), ", "))})
		}
		if router.Section == "udp" || !sharedEntryPoints(router.EntryPoints) {
			continue
		}
		positive := false
		for _, claim := range ruleHosts(router.Rule) {
			if claim.Negated {
				continue
			}
			positive = true
			value := strings.TrimSuffix(strings.ToLower(claim.Value), ".")
			switch claim.Kind {
			case "Host", "HostSNI":
				if claim.Kind == "HostSNI" && value == "*" {
					issues = append(issues, ingressIssue{"catchall", fmt.Sprintf("TCP router %q matches every connection on Cove's shared entry points and would intercept Cove-managed hostnames. Give it a dedicated entry point.", router.Name)})
					continue
				}
				if slices.ContainsFunc(c.Domains, func(domain string) bool { return RouteDomainsOverlap(domain, value) }) {
					issues = append(issues, ingressIssue{"host/" + value, fmt.Sprintf("%s router %q claims %s, which Cove routes to a resource or the dashboard.", strings.ToUpper(router.Section), router.Name, value)})
				}
			case "HostRegexp", "HostSNIRegexp":
				pattern, err := regexp.Compile(claim.Value)
				if err != nil {
					continue
				}
				for _, domain := range c.Domains {
					if strings.HasPrefix(domain, "*.") {
						// Sampling hosts cannot prove an arbitrary custom regexp disjoint from a wildcard.
						issues = append(issues, ingressIssue{"host/" + domain, fmt.Sprintf("%s router %q uses a shared-entry-point regular expression alongside managed wildcard %s. Use exact Host rules or a dedicated entry point.", strings.ToUpper(router.Section), router.Name, domain)})
					} else if pattern.MatchString(domain) {
						issues = append(issues, ingressIssue{"host/" + domain, fmt.Sprintf("%s router %q matches %s, which Cove routes to a resource or the dashboard.", strings.ToUpper(router.Section), router.Name, domain)})
					}
				}
			}
		}
		if !positive && router.Section == "http" {
			warnings = append(warnings, fmt.Sprintf("HTTP router %q has no Host rule on a shared entry point. With a higher priority it takes requests for Cove-managed hostnames.", router.Name))
		}
	}
	return issues, warnings
}

// certificatePath reports whether a certificate or key file path stays inside the proxy volume and
// away from what Cove keeps there itself: dynamic files, their history, ACME state and credentials.
func certificatePath(value string) bool {
	if path.Clean(value) != value || !strings.HasPrefix(value, "/data/") {
		return false
	}
	for _, reserved := range []string{"/data/dynamic/", "/data/cove-custom-history/", "/data/cove-certificates/"} {
		if strings.HasPrefix(value, reserved) {
			return false
		}
	}
	return !strings.HasSuffix(value, ".json") && value != "/data/cloudflare-dns-token"
}

func sharedEntryPoints(entryPoints []string) bool {
	return len(entryPoints) == 0 || slices.Contains(entryPoints, "web") || slices.Contains(entryPoints, "websecure")
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// hostRecord keeps the hostnames a managed file claims so resource routes cannot take them later.
func hostRecord(config customIngress) (hosts, patterns []string) {
	for _, router := range config.Routers {
		if router.Section == "udp" || !sharedEntryPoints(router.EntryPoints) {
			continue
		}
		for _, claim := range ruleHosts(router.Rule) {
			if claim.Negated {
				continue
			}
			value := strings.TrimSuffix(strings.ToLower(claim.Value), ".")
			switch claim.Kind {
			case "Host", "HostSNI":
				if value != "*" && !slices.Contains(hosts, value) {
					hosts = append(hosts, value)
				}
			default:
				if !slices.Contains(patterns, claim.Value) {
					patterns = append(patterns, claim.Value)
				}
			}
		}
	}
	sort.Strings(hosts)
	sort.Strings(patterns)
	return hosts, patterns
}

func redact(message string, secrets []string) string {
	sorted := slices.Clone(secrets)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, secret := range sorted {
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	if len(message) > 400 {
		cut := 400
		for cut > 0 && !utf8.RuneStart(message[cut]) {
			cut--
		}
		message = message[:cut] + "…"
	}
	return message
}
