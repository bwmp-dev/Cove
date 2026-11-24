package worker

import (
	"bytes"
	"context"
	"cove/internal/outbound"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

func (e *Engine) configureOutbound(ctx context.Context, config *outbound.Config) error {
	if config == nil {
		return nil
	}
	if config.Validate() != nil {
		return errors.New("invalid outbound proxy configuration")
	}
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	if config.HTTP != "" || config.HTTPS != "" {
		publishing, err := e.readCloudflarePublishing()
		if err != nil {
			return err
		}
		if publishing.Desired != nil || publishing.Active != nil || publishing.Candidate != nil || len(publishing.Retired) > 0 {
			return errors.New("disable and finish Cloudflare Tunnel cleanup before enabling an HTTP outbound proxy; its edge transport cannot use that proxy")
		}
	}
	path := filepath.Join(e.Root, "outbound-proxy.json")
	previous, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return errors.New("outbound proxy settings unavailable")
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(previous), encoded) {
		return nil
	}
	routes, err := e.loadIngress()
	if err != nil {
		return err
	}
	refresh := false
	for _, list := range routes {
		for _, route := range list {
			if route.Policy != nil && route.Policy.ForwardAuth != nil && route.Policy.ForwardAuth.URL != "" {
				refresh = true
			}
		}
	}
	if err = writeJSON(path, config); err != nil {
		return errors.New("could not save outbound proxy configuration")
	}
	if refresh {
		if err = e.syncIngress(ctx, routes, io.Discard); err != nil {
			var rollback error
			if len(previous) == 0 {
				rollback = os.Remove(path)
			} else {
				rollback = writeJSON(path, json.RawMessage(previous))
			}
			return errors.Join(fmt.Errorf("could not refresh forward-auth outbound policy: %w", err), rollback)
		}
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

func (e *Engine) requireDockerProxy(ctx context.Context, config *outbound.Config) error {
	if config == nil || config.HTTP == "" && config.HTTPS == "" {
		return nil
	}
	var observed struct {
		HTTP    string `json:"http"`
		HTTPS   string `json:"https"`
		NoProxy string `json:"no_proxy"`
	}
	output := &limitedBuffer{Limit: 32768}
	if err := e.Run(ctx, e.Root, []string{"docker", "info", "--format", `{"http":{{json .HTTPProxy}},"https":{{json .HTTPSProxy}},"no_proxy":{{json .NoProxy}}}`}, nil, output); err != nil || json.Unmarshal(output.data.Bytes(), &observed) != nil {
		return errors.New("cannot verify Docker daemon outbound proxy; configure and verify its proxy before publishing or pulling images")
	}
	if strings.TrimRight(observed.HTTP, "/") != strings.TrimRight(config.HTTP, "/") || strings.TrimRight(observed.HTTPS, "/") != strings.TrimRight(config.HTTPS, "/") || strings.TrimSpace(observed.NoProxy) != strings.TrimSpace(config.NoProxy) {
		return errors.New("Docker daemon proxy differs from this operation's outbound policy; configure its http-proxy, https-proxy and no-proxy during an approved maintenance window before publishing or pulling images")
	}
	return nil
}
