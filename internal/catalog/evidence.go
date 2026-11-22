package catalog

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

type Evidence struct {
	ComposeSHA256   string            `json:"compose_sha256"`
	SchemaValidated bool              `json:"schema_validated"`
	ComposeConfig   bool              `json:"compose_config"`
	Runtime         string            `json:"runtime"`
	CheckedAt       string            `json:"checked_at"`
	Message         string            `json:"message"`
	Services        map[string]string `json:"services,omitempty"`
}

//go:embed validation.json
var evidenceData []byte
var evidenceOnce sync.Once
var evidence map[string]Evidence
var evidenceError error

func BuiltinEvidence(t Template) (Evidence, error) {
	evidenceOnce.Do(func() {
		evidenceError = json.Unmarshal(evidenceData, &evidence)
	})
	if evidenceError != nil {
		return Evidence{}, fmt.Errorf("invalid embedded catalogue validation: %w", evidenceError)
	}
	result, ok := evidence[t.ID]
	if !ok || result.ComposeSHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(t.Compose))) {
		return Evidence{SchemaValidated: true, Runtime: "config_only", Message: "Template changed since the recorded runtime check. Run preflight on your worker."}, nil
	}
	result.SchemaValidated = true
	return result, nil
}
