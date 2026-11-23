package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

type IngressExposurePlan struct {
	AffectedWorkloads []string                `json:"affected_workloads"`
	ChangeID          string                  `json:"change_id"`
	Revision          string                  `json:"revision"`
	Digest            string                  `json:"digest"`
	Settings          IngressExposureSettings `json:"settings"`
	PublicBindings    IngressPublicBindings   `json:"public_bindings"`
	Ports             map[string]int          `json:"ports"`
	RequiresRestart   bool                    `json:"requires_restart"`
	Warnings          []string                `json:"warnings"`
}

type preparedIngressExposure struct {
	Plan        IngressExposurePlan
	Snapshot    ingressProxySnapshot
	Body        map[string]json.RawMessage
	Static      []byte
	Effective   IngressExposureSettings
	Probes      []routeCertificateProbe
	ACMEStorage map[string]string
}

func canonicalIngressExposures(settings IngressExposureSettings) string {
	encoded, _ := json.Marshal(settings)
	var copy IngressExposureSettings
	json.Unmarshal(encoded, &copy)
	sortBindings := func(items []IngressBinding) {
		for i := range items {
			if address, err := netip.ParseAddr(items[i].Address); err == nil {
				items[i].Address = address.String()
			}
		}
		slices.SortFunc(items, func(a, b IngressBinding) int {
			if a.Address != b.Address {
				return strings.Compare(a.Address, b.Address)
			}
			return a.Port - b.Port
		})
	}
	if copy.Public != nil {
		sortBindings(copy.Public.HTTP)
		sortBindings(copy.Public.HTTPS)
		if len(copy.Public.HTTP) == 0 {
			copy.Public.HTTP = nil
		}
		if len(copy.Public.HTTPS) == 0 {
			copy.Public.HTTPS = nil
		}
	}
	slices.SortFunc(copy.Listeners, func(a, b IngressListener) int { return strings.Compare(a.ID, b.ID) })
	for i := range copy.Listeners {
		sortBindings(copy.Listeners[i].Bindings)
	}
	if len(copy.Listeners) == 0 {
		copy.Listeners = nil
	}
	encoded, _ = json.Marshal(copy)
	return string(encoded)
}

func proxyPublicBindings(snapshot ingressProxySnapshot) (IngressPublicBindings, error) {
	var host struct {
		PortBindings map[string][]ingressDockerBinding
	}
	if json.Unmarshal(snapshot.HostConfig, &host) != nil {
		return IngressPublicBindings{}, errors.New("proxy publications are unreadable")
	}
	read := func(key string) ([]IngressBinding, error) {
		result := []IngressBinding{}
		for _, binding := range host.PortBindings[key] {
			port, err := strconv.Atoi(binding.HostPort)
			if err != nil || port < 1 || port > 65535 {
				return nil, errors.New("proxy publications must use fixed host ports")
			}
			addresses := []string{binding.HostIP}
			if binding.HostIP == "" {
				addresses = []string{"0.0.0.0", "::"}
			}
			for _, address := range addresses {
				result = append(result, IngressBinding{Address: address, Port: port})
			}
		}
		return result, nil
	}
	http, err := read("80/tcp")
	if err != nil {
		return IngressPublicBindings{}, err
	}
	https, err := read("443/tcp")
	if err != nil {
		return IngressPublicBindings{}, err
	}
	return IngressPublicBindings{HTTP: http, HTTPS: https}, nil
}

func (e *Engine) prepareIngressExposure(ctx context.Context, client *http.Client, desired IngressExposureSettings, state ingressExposureState) (preparedIngressExposure, error) {
	var prepared preparedIngressExposure
	var owned IngressExposureSettings
	if err := json.Unmarshal([]byte(canonicalIngressExposures(desired)), &owned); err != nil {
		return prepared, errors.New("managed listener settings could not be copied")
	}
	desired = owned
	if state.Change != nil && state.Change.FinishedAt == nil {
		return prepared, errors.New("a proxy change is still settling; wait for recovery before planning another")
	}
	e.mu.Lock()
	blocked := e.draining || len(e.recovering) > 0 || e.customUnsettled
	e.mu.Unlock()
	if blocked {
		return prepared, errors.New("worker maintenance or pending route recovery must finish before a listener change")
	}
	mode, err := e.ingressMode()
	if err != nil {
		return prepared, errors.New("proxy migration state is unreadable")
	}
	if mode.Staged {
		return prepared, errors.New("complete the staged proxy migration before changing listeners")
	}
	pending, err := e.certificateReloadPending()
	if err != nil {
		return prepared, errors.New("certificate recovery state is unreadable")
	}
	if pending {
		return prepared, errors.New("managed certificate recovery must finish before changing listeners")
	}
	custom, err := e.loadCustomIngress()
	if err != nil {
		return prepared, errors.New("custom proxy recovery state is unreadable")
	}
	if unsettledCustom(custom.Change) {
		return prepared, errors.New("custom proxy configuration recovery must finish before a listener change")
	}
	routes, err := e.loadIngress()
	if err != nil {
		return prepared, errors.New("managed route inventory is unreadable")
	}
	for _, list := range routes {
		if err := ValidateRouteListeners(list, desired); err != nil {
			return prepared, err
		}
	}
	if err := e.validatePendingRouteListeners(desired); err != nil {
		return prepared, err
	}
	_, prepared.Probes, err = e.selectedRouteCertificates(routes)
	if err != nil {
		return prepared, err
	}
	affected := make([]string, 0, len(routes))
	for workload := range routes {
		affected = append(affected, workload)
	}
	slices.Sort(affected)
	snapshot, err := captureIngressProxy(ctx, client)
	if err != nil {
		return prepared, err
	}
	var config struct {
		Env []string
		Cmd []string
	}
	if json.Unmarshal(snapshot.Config, &config) != nil {
		return prepared, errors.New("private proxy launch is unreadable")
	}
	for _, value := range config.Env {
		if strings.HasPrefix(strings.ToUpper(value), "TRAEFIK_ENTRYPOINTS_") {
			return prepared, errors.New("entrypoints configured through environment variables need an explicit CLI or static-file configuration first")
		}
	}
	var source []byte
	if snapshot.Launch.Kind == "file" {
		out := &limitedBuffer{Limit: (1 << 20) + 1}
		if err := copyIngressProxyFile(ctx, client, snapshot.ID, snapshot.Launch.StaticFile, out, 1<<20); err != nil {
			return prepared, errors.New("proxy static configuration could not be read privately")
		}
		if out.data.Len() > 1<<20 {
			return prepared, errors.New("proxy static configuration exceeds its private size limit")
		}
		source = append([]byte{}, out.data.Bytes()...)
	}
	if err := validateCoveFileProvider(snapshot, source); err != nil {
		return prepared, err
	}
	entries, err := ingressStaticEntrypoints(snapshot, source)
	if err != nil {
		return prepared, err
	}
	prepared.ACMEStorage, err = ingressACMEStorage(snapshot, source)
	if err != nil {
		return prepared, err
	}
	public, err := proxyPublicBindings(snapshot)
	if err != nil {
		return prepared, err
	}
	current := state.Settings
	current.Public = &public
	current.HTTP3 = ingressProxyHTTP3(snapshot, source)
	effective := desired
	if effective.Public == nil {
		effective.Public = &public
	}
	ports, err := allocateIngressListenerPorts(effective, state.Ports, entries)
	if err != nil {
		return prepared, err
	}
	for _, required := range []struct {
		name string
		port int
	}{{"web", 80}, {"websecure", 443}} {
		found := false
		for _, entry := range entries {
			if entry.Name == required.name {
				port, protocol, _ := ingressEntrypointPort(entry.Address)
				found = protocol == "tcp" && port == required.port
			}
		}
		if !found {
			return prepared, errors.New("managed publication changes require Cove's standard web and websecure container ports")
		}
	}
	previousChange := ""
	if state.Change != nil {
		previousChange = state.Change.ID
	}
	routeData, _ := json.Marshal(routes)
	base, _ := json.Marshal(struct {
		ChangeID string
		ID       string
		Running  bool
		Host     json.RawMessage
		Networks map[string]ingressProxyEndpoint
		Settings IngressExposureSettings
		Ports    map[string]int
		Static   string
		Routes   string
	}{previousChange, snapshot.ID, snapshot.Running, snapshot.HostConfig, snapshot.Networks, state.Settings, state.Ports, contentRevision(string(source)), contentRevision(string(routeData))})
	revision := contentRevision(string(base))
	digest := contentRevision(revision + "/" + canonicalIngressExposures(desired))
	body, err := ingressProxyCreateBody(snapshot, digest[:32])
	if err != nil {
		return prepared, err
	}
	if err := bindIngressProxy(body, effective, ports, state.Ports, desired.Public != nil, current.HTTP3); err != nil {
		return prepared, err
	}
	if snapshot.Launch.Kind == "cli" {
		command, err := ingressExposureCLI(snapshot, effective, ports, state.Ports)
		if err != nil {
			return prepared, err
		}
		body["Cmd"], _ = json.Marshal(command)
	} else {
		prepared.Static, err = ingressExposureStaticFile(source, effective, ports, state.Ports)
		if err != nil {
			return prepared, err
		}
		command := []string{"-ec", "mkdir -p /data/dynamic\ncp /etc/traefik/panel.yaml /data/dynamic/panel.yaml\nexec traefik --configFile=" + ingressExposureStaticPath(digest[:32]) + "\n"}
		body["Entrypoint"], _ = json.Marshal([]string{"sh"})
		body["Cmd"], _ = json.Marshal(command)
	}
	prepared.Plan = IngressExposurePlan{ChangeID: digest[:32], Revision: revision, Digest: digest, Settings: desired, PublicBindings: public, Ports: ports, RequiresRestart: canonicalIngressExposures(effective) != canonicalIngressExposures(current), Warnings: []string{"Static listener changes briefly interrupt every connection through this node's shared proxy. Application containers are retained."}}
	prepared.Snapshot, prepared.Body, prepared.Effective = snapshot, body, effective
	prepared.Plan.AffectedWorkloads = affected
	return prepared, nil
}
