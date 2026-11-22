package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type migrationIngressMode struct {
	Staged       bool `json:"staged"`
	TLSChallenge bool `json:"tls_challenge"`
}

func (e *Engine) migrationIngressConfig(config string) (string, error) {
	mode, err := e.ingressMode()
	if err != nil || !mode.Staged {
		return config, err
	}
	var enabled []string
	data, err := os.ReadFile(filepath.Join(e.Root, "ingress-domains.json"))
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err == nil {
		if err = json.Unmarshal(data, &enabled); err != nil {
			return "", err
		}
	}
	var desired dynamicIngress
	if err = json.Unmarshal([]byte(config), &desired); err != nil {
		return "", err
	}
	for name, router := range desired.HTTP.Routers {
		allowed := false
		for _, domain := range enabled {
			if router.Rule == "Host(`"+domain+"`)" {
				allowed = true
				break
			}
		}
		if !allowed {
			// Do not contact the CA until the legacy proxy forwards this hostname.
			router.TLS = map[string]string{}
			desired.HTTP.Routers[name] = router
		}
	}
	if len(desired.HTTP.Routers) == 0 {
		return "{}", nil
	}
	data, err = json.Marshal(desired)
	return string(data), err
}

func (e *Engine) ingressMode() (migrationIngressMode, error) {
	var mode migrationIngressMode
	data, err := os.ReadFile(filepath.Join(e.Root, "ingress-mode.json"))
	if os.IsNotExist(err) {
		return mode, nil
	}
	if err != nil {
		return mode, err
	}
	if err = json.Unmarshal(data, &mode); err != nil {
		return mode, err
	}
	// The legacy proxy may intercept HTTP ACME challenges. TLS challenges pass
	// through the domain's TCP bridge unchanged, including during renewal.
	if mode.Staged && !mode.TLSChallenge {
		return mode, errors.New("staged ingress requires TLS challenges")
	}
	return mode, nil
}

func (e *Engine) PrepareIngress(ctx context.Context, out io.Writer) error {
	data, err := os.ReadFile(filepath.Join(e.Root, "draining.json"))
	var draining bool
	if err != nil || json.Unmarshal(data, &draining) != nil || !draining {
		return errors.New("drain the worker before preparing migration ingress")
	}
	paths, err := filepath.Glob(filepath.Join(e.Root, "operations", "*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var record journal
		if err = json.Unmarshal(data, &record); err != nil {
			return err
		}
		if record.Operation.State == "running" {
			return errors.New("wait for running operations before preparing migration ingress")
		}
	}
	routes, err := e.loadIngress()
	if err != nil {
		return err
	}
	return e.syncIngress(ctx, routes, out)
}
