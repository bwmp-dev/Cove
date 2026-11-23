package worker

import (
	"context"
	"cove/internal/outbound"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

func readOutboundConfig(root string) (*outbound.Config, error) {
	data, err := os.ReadFile(filepath.Join(root, "outbound-proxy.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("outbound proxy settings unavailable")
	}
	var config outbound.Config
	if json.Unmarshal(data, &config) != nil || config.Validate() != nil {
		return nil, errors.New("invalid outbound proxy settings")
	}
	return &config, nil
}

func (e *Engine) configureOutbound(config *outbound.Config) error {
	if config == nil {
		return nil
	}
	if config.Validate() != nil {
		return errors.New("invalid outbound proxy configuration")
	}
	if err := writeJSON(filepath.Join(e.Root, "outbound-proxy.json"), config); err != nil {
		return errors.New("could not save outbound proxy configuration")
	}
	return nil
}

func configuredRunner(root string) CommandRunner {
	return func(ctx context.Context, dir string, args, environment []string, out io.Writer) error {
		config, err := readOutboundConfig(root)
		if err != nil {
			return err
		}
		if config != nil {
			environment = append(config.Env(), environment...)
		}
		return RunCommand(ctx, dir, args, environment, out)
	}
}
