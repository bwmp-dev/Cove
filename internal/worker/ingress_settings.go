package worker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type ingressSettings struct {
	CloudflareDNS        bool `json:"cloudflare_dns"`
	HTTPRedirectPriority *int `json:"http_redirect_priority,omitempty"`
}

func (e *Engine) ingressSettings() (ingressSettings, error) {
	var settings ingressSettings
	data, err := os.ReadFile(filepath.Join(e.Root, "ingress-settings.json"))
	if os.IsNotExist(err) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return settings, err
	}
	if p := settings.HTTPRedirectPriority; p != nil && (*p < 1 || *p > 1000000) {
		return settings, errors.New("ingress HTTP redirect priority must be between 1 and 1000000")
	}
	return settings, nil
}
