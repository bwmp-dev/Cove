package worker

import (
	"context"
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

type ingressTLS struct {
	Certificates []map[string]string `json:"certificates"`
}
type routeCertificateProbe struct {
	Hostname       string    `json:"hostname"`
	CertificatePEM string    `json:"certificate_pem"`
	VerifiedAt     time.Time `json:"verified_at"`
}

func RouteProtocol(routes []Route) int {
	for _, route := range routes {
		if route.IsTransportRoute() || route.Policy != nil && route.Policy.Timeouts != nil {
			return 6
		}
	}
	for _, route := range routes {
		if route.Policy != nil && (route.Policy.Visibility != "" || route.Policy.Listener != "") {
			return 5
		}
	}
	for _, route := range routes {
		if strings.HasPrefix(route.Domain, "*.") || route.Policy != nil && route.Policy.CertificateSource == "default" {
			return 4
		}
	}
	for _, route := range routes {
		if route.Policy != nil && route.Policy.CertificateID != "" {
			return 3
		}
	}
	if HasRouteExtensions(routes) {
		return 2
	}
	return 1
}

func validateCertificateRequest(r Request) error {
	if len(r.Certificates) > 32 {
		return errors.New("at most 32 route certificates are allowed")
	}
	seen := map[string]bool{}
	for _, c := range r.Certificates {
		if !certificateIdentifier.MatchString(c.ID) || seen[c.ID] || len(c.CertificatePEM) > 65536 || len(c.PrivateKeyPEM) > 16384 {
			return errors.New("invalid or duplicate route certificate")
		}
		referenced := false
		for _, route := range r.Routes {
			if route.Policy != nil && route.Policy.CertificateID == c.ID {
				referenced = true
				if err := CertificateCoversRoute(c, route.Domain); err != nil {
					return err
				}
			}
		}
		if !referenced {
			return errors.New("certificate material must be referenced by this operation's routes")
		}
		if err := CertificateOwnedByRoutes(c, r.Routes); err != nil {
			return err
		}
		seen[c.ID] = true
	}
	return nil
}

func (e *Engine) selectedRouteCertificates(routes map[string][]Route) (map[string]RouteCertificate, []routeCertificateProbe, error) {
	certificates := map[string]RouteCertificate{}
	probes := []routeCertificateProbe{}
	hosts := map[string]string{}
	probed := map[string]bool{}
	domains := map[string]bool{}
	for _, list := range routes {
		for _, route := range list {
			domains[route.Domain] = true
		}
	}
	for workload, list := range routes {
		for _, route := range list {
			if route.Policy == nil || route.Policy.CertificateID == "" {
				continue
			}
			key := workload + "/" + route.Policy.CertificateID
			if previous := hosts[route.Domain]; previous != "" && previous != key {
				return nil, nil, errors.New("all paths on a hostname must select the same uploaded certificate")
			}
			hosts[route.Domain] = key
			certificate, err := e.readRouteCertificate(workload, route.Policy.CertificateID)
			if err != nil {
				return nil, nil, errors.New("selected route certificate is unavailable on the worker")
			}
			if err = CertificateCoversRoute(certificate, route.Domain); err != nil {
				return nil, nil, err
			}
			if err = CertificateOwnedByRoutes(certificate, list); err != nil {
				return nil, nil, err
			}
			certificates[key] = certificate
			block, _ := pem.Decode([]byte(certificate.CertificatePEM))
			leaf, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, nil, err
			}
			// Files were validated on receipt. Historical time lets rollback confirm an expired previous leaf's identity.
			verifiedAt := time.Now().UTC()
			if verifiedAt.After(leaf.NotAfter) {
				verifiedAt = leaf.NotAfter.Add(-time.Second)
			}
			if !probed[route.Domain] {
				hostname := routeProbeHostname(route.Domain, domains)
				if hostname == "" {
					return nil, nil, errors.New("no unambiguous hostname is available to verify this wildcard certificate")
				}
				publicLeaf := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}))
				probes = append(probes, routeCertificateProbe{Hostname: hostname, CertificatePEM: publicLeaf, VerifiedAt: verifiedAt})
				probed[route.Domain] = true
			}
		}
	}
	for _, list := range routes {
		for _, route := range list {
			if hosts[route.Domain] != "" && (route.Policy == nil || route.Policy.CertificateID == "") {
				return nil, nil, errors.New("all paths on a hostname must select the same uploaded certificate")
			}
		}
	}
	slices.SortFunc(probes, func(a, b routeCertificateProbe) int {
		if a.Hostname < b.Hostname {
			return -1
		}
		if a.Hostname > b.Hostname {
			return 1
		}
		return 0
	})
	encoded, err := json.Marshal(probes)
	if err != nil {
		return nil, nil, err
	}
	if len(probes) > 2048 || len(encoded) > 3*1024*1024 {
		return nil, nil, errors.New("managed TLS probes exceed the node's 2048-hostname or 3 MiB budget")
	}
	return certificates, probes, nil
}

func (e *Engine) applyRouteCertificates(ctx context.Context, routes map[string][]Route, config *dynamicIngress, out io.Writer) error {
	certificates, probes, err := e.selectedRouteCertificates(routes)
	if err != nil {
		return err
	}
	if err := e.projectRouteCertificateACME(ctx, routes, out); err != nil {
		return err
	}
	if len(certificates) == 0 {
		return nil
	}
	if err := e.Run(ctx, e.Root, []string{"docker", "start", "cove-ingress"}, nil, out); err != nil {
		return err
	}
	config.TLS = &ingressTLS{}
	keys := make([]string, 0, len(certificates))
	for key := range certificates {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		certificate := certificates[key]
		destination := "/data/cove-certificates/" + key
		if err := e.Run(ctx, e.Root, []string{"docker", "exec", "cove-ingress", "mkdir", "-p", filepath.ToSlash(filepath.Dir(destination))}, nil, out); err != nil {
			return err
		}
		for _, item := range []struct{ suffix, value string }{{".crt", certificate.CertificatePEM}, {".key", certificate.PrivateKeyPEM}} {
			local := filepath.Join(e.Root, "workloads", filepath.FromSlash(key+item.suffix))
			if err := os.WriteFile(local, []byte(item.value), 0600); err != nil {
				return err
			}
			err := e.Run(ctx, e.Root, []string{"docker", "cp", local, "cove-ingress:" + destination + item.suffix}, nil, io.Discard)
			removeErr := os.Remove(local)
			if err != nil {
				return err
			}
			if removeErr != nil {
				return removeErr
			}
			if err := e.Run(ctx, e.Root, []string{"docker", "exec", "cove-ingress", "chmod", "600", destination + item.suffix}, nil, out); err != nil {
				return err
			}
		}
		config.TLS.Certificates = append(config.TLS.Certificates, map[string]string{"certFile": destination + ".crt", "keyFile": destination + ".key"})
	}
	path := filepath.Join(e.Root, "certificate-probes.json")
	return writeJSON(path, probes)
}

func (e *Engine) confirmRouteCertificates(ctx context.Context, routes map[string][]Route) error {
	certificates, _, err := e.selectedRouteCertificates(routes)
	if err != nil || len(certificates) == 0 {
		return err
	}
	image, err := e.linkGatewayImage(ctx)
	if err != nil {
		return err
	}
	diagnostic := &limitedBuffer{Limit: 4096}
	if err := e.Run(ctx, e.Root, []string{"docker", "run", "--rm", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges:true", "--memory=64m", "--pids-limit=32", "--network=container:cove-ingress", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--mount", "type=bind,source=" + filepath.Join(e.Root, "certificate-probes.json") + ",target=/config/probes.json,readonly", image, "certificate-probe"}, nil, diagnostic); err != nil {
		return fmt.Errorf("managed proxy did not confirm the selected route certificates: %s", strings.TrimSpace(diagnostic.data.String()))
	}
	return nil
}

func RunRouteCertificateProbe(ctx context.Context, input io.Reader) error {
	var probes []routeCertificateProbe
	decoder := json.NewDecoder(io.LimitReader(input, 3*1024*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&probes); err != nil {
		return errors.New("invalid certificate probe configuration")
	}
	if err := decoder.Decode(new(any)); err != io.EOF || len(probes) > 2048 {
		return errors.New("invalid certificate probe configuration")
	}
	for _, probe := range probes {
		if probe.VerifiedAt.IsZero() {
			return errors.New("certificate probe validation time is required")
		}
		if err := checkRouteCertificateTLS(ctx, "127.0.0.1:443", probe.Hostname, probe.CertificatePEM, probe.VerifiedAt); err != nil {
			return err
		}
	}
	return nil
}
