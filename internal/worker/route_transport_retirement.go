package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (e *Engine) loadIngressTransition() (*dynamicIngress, error) {
	data, err := os.ReadFile(filepath.Join(e.Root, "ingress.route-transition.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var config dynamicIngress
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, errors.New("route transition checkpoint is unreadable")
	}
	return &config, nil
}

func (e *Engine) clearIngressTransition() error {
	err := os.Remove(filepath.Join(e.Root, "ingress.route-transition.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDirectory(e.Root)
}

func mergeIngressOwnership(target *dynamicIngress, checkpoint *dynamicIngress) {
	if checkpoint == nil {
		return
	}
	if target.HTTP.Routers == nil {
		target.HTTP.Routers = map[string]ingressRouter{}
	}
	for name, router := range checkpoint.HTTP.Routers {
		target.HTTP.Routers[name] = router
	}
	for protocol, config := range map[string]*ingressTransportConfig{"tcp": checkpoint.TCP, "udp": checkpoint.UDP} {
		if config == nil {
			continue
		}
		owned := target.transport(protocol)
		for name, router := range config.Routers {
			owned.Routers[name] = router
		}
	}
}

func retiringUDPIngress(previous, desired dynamicIngress) (dynamicIngress, bool, error) {
	if previous.UDP == nil {
		return dynamicIngress{}, false, nil
	}
	active := map[string]bool{}
	if desired.UDP != nil {
		for _, router := range desired.UDP.Routers {
			for _, entry := range router.EntryPoints {
				active[entry] = true
			}
		}
	}
	retired := map[string]bool{}
	for name, router := range previous.UDP.Routers {
		if !strings.HasPrefix(name, "cove-route-udp-") {
			continue
		}
		for _, entry := range router.EntryPoints {
			if strings.HasPrefix(entry, "cove-listener-") && !active[entry] {
				retired[entry] = true
			}
		}
	}
	if len(retired) == 0 {
		return dynamicIngress{}, false, nil
	}
	data, err := json.Marshal(desired)
	if err != nil {
		return dynamicIngress{}, false, err
	}
	var transition dynamicIngress
	if err := json.Unmarshal(data, &transition); err != nil {
		return dynamicIngress{}, false, err
	}
	for entry := range retired {
		// Traefik 3.7 retains a UDP entrypoint's handler when its last router disappears.
		// Switch it to an impossible destination before removal; port zero cannot host a service.
		transition.setTransportRoute(Route{Protocol: "udp", Policy: &RoutePolicy{Listener: strings.TrimPrefix(entry, "cove-listener-")}}, []map[string]string{{"address": "127.0.0.1:0"}})
	}
	return transition, true, nil
}

func (e *Engine) publishIngress(ctx context.Context, config string, out io.Writer) error {
	path := filepath.Join(e.Root, "ingress.traefik.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		return err
	}
	for _, args := range [][]string{{"docker", "cp", path, "cove-ingress:/data/routes.next"}, {"docker", "start", "cove-ingress"}, {"docker", "exec", "cove-ingress", "sh", "-ec", "mkdir -p /data/dynamic; mv /data/routes.next /data/dynamic/routes.yaml"}} {
		if err := e.Run(ctx, e.Root, args, nil, out); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) retireUDPIngress(ctx context.Context, desired dynamicIngress, out io.Writer) error {
	data, err := os.ReadFile(filepath.Join(e.Root, "ingress.traefik.yaml"))
	if os.IsNotExist(err) {
		data, err = []byte("{}"), nil
	}
	if err != nil {
		return err
	}
	var previous dynamicIngress
	if err = json.Unmarshal(data, &previous); err != nil {
		return err
	}
	checkpoint, err := e.loadIngressTransition()
	if err != nil {
		return err
	}
	mergeIngressOwnership(&previous, checkpoint)
	transition, needed, err := retiringUDPIngress(previous, desired)
	if err != nil || !needed {
		return err
	}
	data, err = json.Marshal(transition)
	if err != nil {
		return err
	}
	ownership := newIngressConfig()
	mergeIngressOwnership(&ownership, &previous)
	mergeIngressOwnership(&ownership, &transition)
	if err := writeJSON(filepath.Join(e.Root, "ingress.route-transition.json"), ownership); err != nil {
		return err
	}
	if err := e.publishIngress(ctx, string(data), out); err != nil {
		return err
	}
	for attempt := 0; attempt < 30; attempt++ {
		err = e.confirmTransportIngress(ctx, transition, true)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return err
}
