package catalog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cove/internal/worker"
)

func TestEveryBuiltinTemplate(t *testing.T) {
	var index Index
	if err := json.Unmarshal(builtinData, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Templates) < 50 {
		t.Fatal("at least 50 templates required", len(index.Templates))
	}
	seen := map[string]bool{}
	for _, template := range index.Templates {
		t.Run(template.ID, func(t *testing.T) {
			if seen[template.ID] {
				t.Fatal("duplicate ID")
			}
			seen[template.ID] = true
			if _, err := Validate(template); err != nil {
				t.Fatal(err)
			}
			values, err := Values(template, nil, true)
			if err != nil {
				t.Fatal(err)
			}
			draft, err := Draft(template, values, "app.example.test", true)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(draft.Compose, "volumes:") {
				t.Fatal("persistent named volumes required")
			}
		})
	}
}

func TestBuiltinValidationEvidenceMatchesCompose(t *testing.T) {
	index, _, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range index.Templates {
		t.Run(template.ID, func(t *testing.T) {
			record, err := BuiltinEvidence(template)
			if err != nil || !record.SchemaValidated || !record.ComposeConfig {
				t.Fatal("missing successful schema/docker compose config evidence for this exact definition", err)
			}
			if _, err = time.Parse(time.RFC3339Nano, record.CheckedAt); err != nil {
				t.Fatal("invalid validation timestamp", err)
			}
			if record.Runtime != "config_only" && record.Runtime != "started_healthy" {
				t.Fatal("invalid runtime status")
			}
			if record.Message == "" || len(record.Message) > 500 || strings.ContainsAny(record.Message, "\r\n\x1b") {
				t.Fatal("evidence must contain a short status, not application logs")
			}
		})
	}
}

func TestTemplateRejectsUnsafeCapabilitiesAndExternalReads(t *testing.T) {
	var index Index
	if err := json.Unmarshal(builtinData, &index); err != nil {
		t.Fatal(err)
	}
	base := index.Templates[0]
	for name, fragment := range map[string]string{
		"privileged":            "privileged: true",
		"host":                  "network_mode: host",
		"other-container":       "pid: container:another-workload",
		"variable-host":         "network_mode: ${MODE}",
		"socket":                "volumes: [/var/run/docker.sock:/var/run/docker.sock]",
		"home-bind":             "volumes: [~/secrets:/data]",
		"variable-mount-type":   "volumes: [{type: '${MOUNT_TYPE}', source: /etc, target: /data}]",
		"environment-file":      "env_file: /etc/environment",
		"extends":               "extends: {file: /etc/secret, service: app}",
		"dynamic-privilege":     "privileged: ${ENABLED}",
		"bare-host-environment": "environment: {PASSWORD: $UNDECLARED_HOST_PASSWORD}",
		"nested-host-variable":  "environment: {PASSWORD: '${POSTGRES_PASSWORD:-${UNDECLARED_PASSWORD}}'}",
		"gpu-reservation":       "deploy: {resources: {reservations: {devices: [{capabilities: [gpu]}]}}}",
		"host-network-driver":   "networks: [escape]\nnetworks:\n  escape: {driver: host}",
		"volume-driver-options": "volumes: [escape:/data]\nvolumes:\n  escape: {driver_opts: {type: none, o: bind, device: /}}",
	} {
		t.Run(name, func(t *testing.T) {
			v := base
			v.Images = map[string]string{"app": "postgres:17.12-alpine"}
			v.Compose = "services:\n  app:\n    image: postgres:17.12-alpine\n    " + fragment + "\n"
			if _, err := Validate(v); err == nil {
				t.Fatal("unsafe template accepted")
			}
		})
	}
	base.Compose = "services:\n  app:\n    image: postgres:17.12-alpine\n    privileged: true\n"
	base.Images = map[string]string{"app": "postgres:17.12-alpine"}
	base.Capabilities = []string{"privileged"}
	if warnings, err := Validate(base); err != nil || len(warnings) == 0 {
		t.Fatal(warnings, err)
	}
	if _, err := Draft(base, nil, "", false); err == nil {
		t.Fatal("host access did not require acknowledgement")
	}
}

func TestExportStripsValuesAndRequiresFreshInputs(t *testing.T) {
	var index Index
	json.Unmarshal(builtinData, &index)
	metadata := index.Templates[0]
	template, err := Export(metadata, "services:\n  app:\n    image: postgres:17.12-alpine\n    environment:\n      PASSWORD: private-password\n    command: [echo, private-command-value]\n    volumes: [data:/var/lib/postgresql/data]\nvolumes:\n  data: {}\n")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(template)
	if strings.Contains(string(data), "private-password") || strings.Contains(string(data), "private-command-value") {
		t.Fatal("export leaked source values")
	}
	if _, err := Values(template, nil, false); err == nil {
		t.Fatal("export silently invented replacement settings")
	}
}

func TestTemplateIndexRejectsUnknownFieldsAndUnpinnedImages(t *testing.T) {
	for _, data := range []string{`{"schema_version":2,"templates":[]}`, `{"schema_version":1,"templates":[],"unexpected":true}`, `{"schema_version":1,"templates":[]} {}`} {
		if _, err := Decode([]byte(data)); err == nil {
			t.Fatal("invalid index accepted", data)
		}
	}
	index, _, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{"postgres", "postgres:latest", "postgres:alpine", "postgres:${VERSION}"} {
		v := index.Templates[0]
		v.Images = map[string]string{"app": image}
		v.Compose = "services:\n  app:\n    image: " + image + "\n"
		if _, err := Validate(v); err == nil {
			t.Fatal("unpinned image accepted", image)
		}
	}
}

func TestTemplateVariablesAndFileSecrets(t *testing.T) {
	index, _, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	template := index.Templates[0]
	first, err := Values(template, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Values(template, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, variable := range template.Variables {
		if variable.Generated && (!generatedSecret.MatchString(first[variable.Key]) || first[variable.Key] == second[variable.Key]) {
			t.Fatal("secret generation failed")
		}
	}
	template.Variables = []Variable{{Key: "COUNT", Label: "Count", Type: "integer", Required: true}, {Key: "TOKEN", Label: "Token", Type: "secret", Generated: true}}
	if _, err = Values(template, map[string]string{"COUNT": "wrong"}, false); err == nil {
		t.Fatal("invalid typed input accepted")
	}
	if _, err = Values(template, map[string]string{"COUNT": "2", "TOKEN": "unsafe:password"}, false); err == nil {
		t.Fatal("unsafe generated-secret replacement accepted")
	}
	if _, err = Values(template, map[string]string{"COUNT": "2", "UNDECLARED": "value"}, false); err == nil {
		t.Fatal("undeclared variable accepted")
	}
	template.Web = nil
	template.Images = map[string]string{"app": "postgres:17.12-alpine"}
	template.Compose = "services:\n  app:\n    image: postgres:17.12-alpine\n    environment:\n      COUNT: ${COUNT}\n"
	template.FileSecrets = []worker.FileSecret{{Name: "token", Variable: "TOKEN", Services: []string{"app"}}}
	if _, err = Validate(template); err != nil {
		t.Fatal(err)
	}
	template.FileSecrets[0].Services = []string{"missing"}
	if _, err = Validate(template); err == nil {
		t.Fatal("undefined file-secret target accepted")
	}
}

func TestExportFileSecretsAndLiteralCommandsNeverLeak(t *testing.T) {
	index, _, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	metadata := index.Templates[0]
	metadata.Web = nil
	metadata.FileSecrets = []worker.FileSecret{{Name: "token", Variable: "SETTING_1", Services: []string{"app"}}}
	template, err := Export(metadata, "services:\n  app:\n    image: postgres:17.12-alpine\n    environment: [PASSWORD=private-environment]\n    entrypoint: [echo, private-entrypoint]\n    healthcheck:\n      test: [CMD-SHELL, echo private-health]\n    ports: [8080:80]\n    labels: {token: private-label}\n    volumes: [data:/var/lib/postgresql/data]\nvolumes:\n  data: {}\nconfigs:\n  credential:\n    content: private-config\n")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(template)
	if err != nil || strings.Contains(string(data), "private-") || strings.Contains(template.Compose, "ports:") || strings.Contains(template.Compose, "labels:") {
		t.Fatal("export leaked prior configuration", err)
	}
	seen := map[string]bool{}
	for _, variable := range template.Variables {
		if seen[variable.Key] || variable.Default != "" || variable.Type != "secret" {
			t.Fatal("unsafe or duplicate exported variable")
		}
		seen[variable.Key] = true
	}
}

func TestExportPreviewMatchesConfirmedDefinition(t *testing.T) {
	index, _, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	metadata := index.Templates[0]
	compose := "services:\n  app:\n    image: postgres:17.12-alpine\n    command: [echo, value]\n    environment: {FIRST: one, SECOND: two}\n    healthcheck: {test: [CMD-SHELL, echo ready]}\nconfigs:\n  first: {content: one}\n  second: {content: two}\n"
	var previous string
	for range 20 {
		definition, err := Export(metadata, compose)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(definition)
		if err != nil {
			t.Fatal(err)
		}
		if previous != "" && previous != string(data) {
			t.Fatal("export changed variable mappings between preview and confirmation")
		}
		previous = string(data)
	}
}
