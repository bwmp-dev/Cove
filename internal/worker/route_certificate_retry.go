package worker

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type RouteCertificateRetry struct {
	Hostname      string    `json:"hostname"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	Message       string    `json:"message"`
}

type certificateRetryState struct {
	Hosts    []string  `json:"hosts"`
	Attempts int       `json:"attempts"`
	Next     time.Time `json:"next"`
}

func publicACMEHosts(routes map[string][]Route) []string {
	seen := map[string]bool{}
	for _, list := range routes {
		for _, route := range list {
			if route.IsPublicHTTPRoute() && !strings.HasPrefix(route.Domain, "*.") && (route.Policy == nil || route.Policy.CertificateID == "" && (route.Policy.CertificateSource == "" || route.Policy.CertificateSource == "acme")) {
				seen[route.Domain] = true
			}
		}
	}
	hosts := make([]string, 0, len(seen))
	for host := range seen {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	return hosts
}

func (e *Engine) certificateRetryState() (certificateRetryState, error) {
	var state certificateRetryState
	data, err := os.ReadFile(filepath.Join(e.Root, "certificate-retry.json"))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if len(data) > maxACMEStorageBytes || json.Unmarshal(data, &state) != nil || state.Attempts < 0 || state.Attempts > 1000000 {
		return state, errors.New("certificate retry checkpoint is unreadable")
	}
	return state, nil
}

func (e *Engine) CertificateRetries(workload string) ([]RouteCertificateRetry, error) {
	if !workloadIdentifier.MatchString(workload) {
		return nil, errors.New("invalid resource")
	}
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	routes, err := e.loadIngress()
	if err != nil {
		return nil, err
	}
	state, err := e.certificateRetryState()
	if err != nil {
		return nil, err
	}
	result := []RouteCertificateRetry{}
	for _, host := range publicACMEHosts(map[string][]Route{workload: routes[workload]}) {
		if !slices.Contains(state.Hosts, host) {
			continue
		}
		result = append(result, RouteCertificateRetry{Hostname: host, Attempts: state.Attempts, NextAttemptAt: state.Next, Message: "Certificate issuance is still pending. Cove retries automatically; check this hostname's DNS and public reachability on ports 80 and 443. Repeated failures slow retries to once every six hours."})
	}
	return result, nil
}

func missingACMECertificates(data []byte, hosts []string) ([]string, error) {
	var store map[string]struct {
		Certificates []struct {
			Certificate []byte `json:"certificate"`
		} `json:"Certificates"`
	}
	if len(data) > maxACMEStorageBytes || len(data) > 0 && json.Unmarshal(data, &store) != nil {
		return nil, errors.New("managed ACME storage is unreadable")
	}
	missing := append([]string{}, hosts...)
	for _, entry := range store["cove"].Certificates {
		block, _ := pem.Decode(entry.Certificate)
		if block == nil {
			return nil, errors.New("managed ACME certificate is unreadable")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("managed ACME certificate is unreadable")
		}
		// Renewal of stored certificates belongs to Traefik; configuration events
		// only retry names that have never acquired a certificate in its store.
		missing = slices.DeleteFunc(missing, func(host string) bool { return certificate.VerifyHostname(host) == nil })
	}
	return missing, nil
}

func (e *Engine) retryPublicCertificates(ctx context.Context, now time.Time, out io.Writer) error {
	unlock, err := e.lockNodeChange(false)
	if err != nil {
		return err
	}
	defer unlock()
	e.mu.Lock()
	if e.draining || e.hostChanging || e.hasMoveFence() || len(e.active) > 0 || len(e.pending) > 0 || len(e.recovering) > 0 || e.customUnsettled || e.customRunning || e.proxyUnsettled || e.proxyRunning {
		e.mu.Unlock()
		return nil
	}
	e.active[housekeepingWorkload] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, housekeepingWorkload); e.mu.Unlock() }()
	e.ingressMu.Lock()
	defer e.ingressMu.Unlock()
	if e.trafficRecoveryPending() || e.ingressExposureReady() != nil {
		return nil
	}
	pendingCertificate, err := e.certificateReloadPending()
	if err != nil {
		return err
	}
	if pendingCertificate {
		return nil
	}
	transition, err := e.loadIngressTransition()
	if err != nil {
		return err
	}
	if transition != nil {
		return nil
	}
	routes, err := e.loadIngress()
	if err != nil {
		return err
	}
	hosts := publicACMEHosts(routes)
	state, err := e.certificateRetryState()
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		if err := os.Remove(filepath.Join(e.Root, "certificate-retry.json")); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	proxy, err := e.inspectProxy(ctx)
	if err != nil {
		return err
	}
	if !proxy.Running {
		return nil
	}
	data, err := e.readACMEFile(ctx, "traefik-acme.json")
	if err != nil {
		return err
	}
	hosts, err = missingACMECertificates(data, hosts)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		if err := os.Remove(filepath.Join(e.Root, "certificate-retry.json")); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	state.Hosts = hosts
	if state.Next.IsZero() {
		state.Next = now.Add(time.Minute)
	}
	path := filepath.Join(e.Root, "certificate-retry.json")
	if now.Before(state.Next) {
		return writeJSON(path, state)
	}
	data, err = os.ReadFile(filepath.Join(e.Root, "ingress.traefik.yaml"))
	if err != nil {
		return err
	}
	var config dynamicIngress
	if json.Unmarshal(data, &config) != nil {
		return errors.New("managed ingress configuration is unreadable")
	}
	// Traefik ignores identical provider snapshots. An unused, namespaced
	// middleware changes the snapshot without changing any request's routing.
	revision := rand.Text()
	if config.HTTP.Middlewares == nil {
		config.HTTP.Middlewares = map[string]ingressMiddleware{}
	}
	config.HTTP.Middlewares["cove-certificate-retry"] = ingressMiddleware{Headers: map[string]any{"customRequestHeaders": map[string]string{"X-Cove-Unused-Retry": revision}}}
	encoded, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if state.Attempts < 1000000 {
		state.Attempts++
	}
	delay := time.Hour
	if state.Attempts >= 2 {
		delay = 6 * time.Hour
	}
	state.Next = now.Add(delay)
	// Persist the budget before delivery, including an ambiguous Docker reply.
	if err = writeJSON(path, state); err != nil {
		return err
	}
	return e.publishRunningIngress(ctx, string(encoded), out)
}

func (e *Engine) certificateRetryLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		probe, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := e.retryPublicCertificates(probe, time.Now(), os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "Public certificate retry is unavailable; check managed ingress state.")
		}
		cancel()
	}
}
