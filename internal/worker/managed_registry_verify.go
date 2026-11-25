package worker

import (
	"context"
	"cove/internal/outbound"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

func VerifyManagedRegistry(ctx context.Context, registry *Registry, certificate string, selected ...*outbound.Config) error {
	if registry == nil {
		return errors.New("registry verification requires explicit credentials")
	}
	if err := ValidateRegistry(registry); err != nil {
		return err
	}
	if len(certificate) > 128<<10 {
		return errors.New("registry certificate exceeds verification limit")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return errors.New("registry TLS trust roots unavailable")
	}
	if certificate != "" && !roots.AppendCertsFromPEM([]byte(certificate)) {
		return errors.New("selected registry certificate unavailable")
	}
	config := outbound.Environment()
	if len(selected) > 0 && selected[0] != nil {
		config = *selected[0]
	}
	if err := config.Validate(); err != nil {
		return errors.New("registry outbound proxy policy is invalid")
	}
	transport := &http.Transport{Proxy: func(r *http.Request) (*url.URL, error) { return config.For(r.URL.Scheme, r.URL.Host) }, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 << 10}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, "GET", "https://"+registry.Server+"/v2/", nil)
	if err != nil {
		return errors.New("registry verification request unavailable")
	}
	request.SetBasicAuth(registry.Username, registry.Password)
	response, err := client.Do(request)
	if err != nil {
		return errors.New("registry authenticated HTTPS verification failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Docker-Distribution-Api-Version") != "registry/2.0" {
		return errors.New("registry authenticated HTTPS endpoint refused verification")
	}
	if _, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4096)); err != nil {
		return errors.New("registry verification response unavailable")
	}
	return nil
}
