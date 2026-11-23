package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func (e *Engine) needsTransportConfirmation(routes map[string][]Route) (bool, error) {
	checkpoint, err := e.loadIngressTransition()
	if err != nil || checkpoint != nil {
		return checkpoint != nil, err
	}
	if len(transportRouteReferences(routes)) > 0 {
		return true, nil
	}
	data, err := os.ReadFile(filepath.Join(e.Root, "ingress.traefik.yaml"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var previous dynamicIngress
	if json.Unmarshal(data, &previous) != nil {
		return false, errors.New("previous generated route configuration is unreadable")
	}
	return previous.TCP != nil || previous.UDP != nil, nil
}

func transportIngressAccepted(data string, desired *ingressTransportConfig) bool {
	var routers []struct {
		Name, Status, Rule, Service string
		EntryPoints, Middlewares    []string
	}
	if json.Unmarshal([]byte(data), &routers) != nil {
		return false
	}
	expected := 0
	if desired != nil {
		expected = len(desired.Routers)
	}
	found := 0
	for _, router := range routers {
		if !strings.HasPrefix(router.Name, "cove-route-") {
			continue
		}
		if desired == nil {
			return false
		}
		name := strings.TrimSuffix(router.Name, "@file")
		want, ok := desired.Routers[name]
		if !ok || router.Status != "enabled" || router.Rule != want.Rule || strings.TrimSuffix(router.Service, "@file") != want.Service || !slices.Equal(router.EntryPoints, want.EntryPoints) || len(router.Middlewares) != len(want.Middlewares) {
			return false
		}
		for i, middleware := range want.Middlewares {
			if strings.TrimSuffix(router.Middlewares[i], "@file") != middleware {
				return false
			}
		}
		found++
	}
	return found == expected
}

func (e *Engine) confirmTransportIngress(ctx context.Context, desired dynamicIngress, required bool) error {
	if !required {
		return nil
	}
	for _, entry := range []struct {
		protocol string
		config   *ingressTransportConfig
	}{{"tcp", desired.TCP}, {"udp", desired.UDP}} {
		data, err := e.proxyAPI(ctx, "cove-ingress", "/api/"+entry.protocol+"/routers")
		if err != nil || !transportIngressAccepted(string(data), entry.config) {
			return errors.New("the proxy has not accepted the dedicated TCP and UDP routes")
		}
	}
	return nil
}
