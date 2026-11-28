package worker

import (
	"cove/internal/outbound"
	"errors"
	"strings"
)

func (e *Engine) buildProxy(env []string) (outbound.Config, error) {
	proxy := outbound.Environment()
	if saved, err := readOutboundConfig(e.Root); err != nil {
		return proxy, err
	} else if saved != nil {
		proxy = *saved
	}
	for _, variable := range env {
		key, value, ok := strings.Cut(variable, "=")
		if !ok {
			continue
		}
		switch key {
		case "HTTP_PROXY":
			proxy.HTTP = value
		case "HTTPS_PROXY":
			proxy.HTTPS = value
		case "NO_PROXY":
			proxy.NoProxy = value
		}
	}
	return proxy, proxy.Validate()
}

func ValidateBuildCacheProxy(proxy outbound.Config) error {
	if err := proxy.Validate(); err != nil {
		return err
	}
	if proxy.HTTP != "" || proxy.HTTPS != "" {
		return errors.New("hard build-cache limits cannot be used with an HTTP outbound proxy; disable the cache cap or use a node without that proxy")
	}
	return nil
}

func (e *Engine) cacheProxyReady() error {
	proxy, err := e.buildProxy(nil)
	if err != nil {
		return err
	}
	return ValidateBuildCacheProxy(proxy)
}
