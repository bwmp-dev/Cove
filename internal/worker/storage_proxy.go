package worker

import (
	"cove/internal/outbound"
	"net"
	"net/url"
)

func StorageProxyEnvironment(storage *StorageLocation, proxy outbound.Config) ([]string, error) {
	if err := proxy.Validate(); err != nil {
		return nil, err
	}
	environment := proxy.Env()
	if storage.Protocol == "sftp" {
		u, err := url.Parse(storage.Endpoint)
		if err != nil {
			return nil, err
		}
		port := u.Port()
		if port == "" {
			port = "22"
		}
		selected, err := proxy.For("https", net.JoinHostPort(u.Hostname(), port))
		if err != nil {
			return nil, err
		}
		value := ""
		if selected != nil {
			value = selected.String()
		}
		environment = append(environment, "RCLONE_CONFIG_COVE_HTTP_PROXY="+value)
	}
	return environment, nil
}

func (e *Engine) storageEnvironment(storage *StorageLocation, selected *outbound.Config) ([]string, error) {
	environment, err := storageEnvironment(storage)
	if err != nil {
		return nil, err
	}
	if selected == nil {
		selected, err = readOutboundConfig(e.Root)
		if err != nil {
			return nil, err
		}
	}
	proxy := outbound.Environment()
	if selected != nil {
		proxy = *selected
	}
	values, err := StorageProxyEnvironment(storage, proxy)
	if err != nil {
		return nil, err
	}
	return append(environment, values...), nil
}
