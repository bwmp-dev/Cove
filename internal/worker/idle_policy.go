package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type IdlePolicy struct {
	Enabled       bool `json:"enabled"`
	AfterSeconds  int  `json:"after_seconds"`
	RequestDriven bool `json:"request_driven"`
}

func (p IdlePolicy) normalized() IdlePolicy {
	if p.AfterSeconds == 0 {
		p.AfterSeconds = 300
	}
	return p
}

func (p IdlePolicy) validate() error {
	p = p.normalized()
	if p.AfterSeconds < 60 || p.AfterSeconds > 86400 {
		return errors.New("idle sleep must wait between 60 and 86400 seconds")
	}
	if p.Enabled && !p.RequestDriven {
		return errors.New("idle sleep requires confirmation that the app is entirely request-driven")
	}
	return nil
}

type IdleStatus struct {
	Enabled        bool       `json:"enabled"`
	AfterSeconds   int        `json:"after_seconds"`
	Phase          string     `json:"phase"`
	ActiveRequests int        `json:"active_requests"`
	LastActivity   *time.Time `json:"last_activity,omitempty"`
	LastSleep      *time.Time `json:"last_sleep,omitempty"`
	LastWake       *time.Time `json:"last_wake,omitempty"`
	Error          string     `json:"error,omitempty"`
}

type idleEndpoint struct {
	Service   string `json:"service"`
	Port      int    `json:"port"`
	Key       string `json:"key"`
	Container string `json:"container"`
}

type idleWorkloadState struct {
	Policy    IdlePolicy        `json:"policy"`
	Config    idleGatewayConfig `json:"config"`
	Endpoints []idleEndpoint    `json:"endpoints"`
	LastSleep *time.Time        `json:"last_sleep,omitempty"`
	LastWake  *time.Time        `json:"last_wake,omitempty"`
	LastError string            `json:"last_error,omitempty"`
}

func (e *Engine) idleDir(workload string, parts ...string) string {
	return filepath.Join(append([]string{e.Root, "workloads", workload, "idle"}, parts...)...)
}

func (e *Engine) readIdle(workload string) (*idleWorkloadState, error) {
	if !workloadIdentifier.MatchString(workload) {
		return nil, errors.New("invalid idle workload")
	}
	data, err := os.ReadFile(filepath.Join(e.Root, "workloads", workload, "idle.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state idleWorkloadState
	if err = json.Unmarshal(data, &state); err != nil {
		return nil, errors.New("idle policy state is unreadable")
	}
	if err = state.Policy.validate(); err != nil {
		return nil, err
	}
	if !state.Policy.Enabled || state.Config.Workload != workload {
		return nil, errors.New("invalid persisted idle policy")
	}
	if err = state.Config.validate(); err != nil {
		return nil, err
	}
	for _, endpoint := range state.Endpoints {
		if !serviceIdentifier.MatchString(endpoint.Service) || endpoint.Port < 1 || endpoint.Port > 65535 || !containerIdentity.MatchString(endpoint.Container) || state.Config.Targets[endpoint.Key] == "" {
			return nil, errors.New("invalid persisted idle endpoint")
		}
	}
	return &state, nil
}

func idleToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (s idleWorkloadState) servers(service string, port int) ([]map[string]any, error) {
	listen, err := netip.ParseAddrPort(s.Config.Listen)
	if err != nil {
		return nil, err
	}
	servers := []map[string]any{}
	for _, endpoint := range s.Endpoints {
		if endpoint.Service == service && endpoint.Port == port {
			servers = append(servers, map[string]any{"preservePath": true, "url": "http://" + net.JoinHostPort(listen.Addr().String(), strconv.Itoa(int(listen.Port()))) + "/cove/" + endpoint.Key + "/"})
		}
	}
	return servers, nil
}

func (e *Engine) validateIdleRoutes(ctx context.Context, workload string, routes []Route) error {
	policy, err := e.readIdle(workload)
	if err != nil || policy == nil {
		return err
	}
	if err := idleAuthEligibility(routes); err != nil {
		return err
	}
	if len(routes) == 0 {
		return errors.New("disable idle sleep before removing all managed routes")
	}
	observed := map[string]bool{}
	for _, target := range routeTargets(routes) {
		observed[target.Service] = true
	}
	containers, err := e.HealthContainers(ctx, workload)
	if err != nil {
		return err
	}
	for _, container := range containers {
		if container.ExpectedCompleted {
			continue
		}
		if !observed[container.Service] {
			return errors.New("every service must retain a managed HTTP route while idle sleep is enabled")
		}
	}
	suspension, err := e.readSuspension(workload)
	if err != nil {
		return err
	}
	if suspension != nil && suspension.Options.Reason == "idle" && suspension.Phase != "awake" {
		for _, target := range routeTargets(routes) {
			names, err := e.serviceNames(workload, target.Service)
			if err != nil {
				return err
			}
			for _, name := range names {
				servers, err := policy.servers(name, target.Port)
				if err != nil {
					return err
				}
				if len(servers) == 0 {
					return errors.New("start the sleeping resource before changing its upstream service or port")
				}
			}
		}
	}
	return nil
}
